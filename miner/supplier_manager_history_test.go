//go:build test

package miner

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/alicebob/miniredis/v2"
	"github.com/alitto/pond/v2"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/hashicorp/go-version"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// historySupplierQueryClient returns a pre-built Supplier so tests can
// exercise the ServiceConfigHistory-aware path of filterStakedSuppliers.
type historySupplierQueryClient struct {
	addr     string
	supplier atomic.Pointer[sharedtypes.Supplier]
}

func (h *historySupplierQueryClient) set(s *sharedtypes.Supplier) { h.supplier.Store(s) }

func (h *historySupplierQueryClient) GetSupplier(_ context.Context, operatorAddress string) (sharedtypes.Supplier, error) {
	if operatorAddress != h.addr {
		return sharedtypes.Supplier{}, fmt.Errorf("unknown supplier")
	}
	s := h.supplier.Load()
	if s == nil {
		return sharedtypes.Supplier{}, fmt.Errorf("no supplier set")
	}
	return *s, nil
}

func (h *historySupplierQueryClient) GetParams(context.Context) (*suppliertypes.Params, error) {
	return &suppliertypes.Params{}, nil
}

func (h *historySupplierQueryClient) InvalidateSupplier(string) {}

// fakeBlockClient lets the test control the height observed by the manager.
type fakeBlockClient struct{ height atomic.Int64 }

func (b *fakeBlockClient) LastBlock(context.Context) client.Block {
	return fakeBlock{h: b.height.Load()}
}

func (b *fakeBlockClient) CommittedBlocksSequence(context.Context) client.BlockReplayObservable {
	return nil
}

func (b *fakeBlockClient) GetChainVersion() *version.Version { return nil }

func (b *fakeBlockClient) Close() {}

type fakeBlock struct{ h int64 }

func (f fakeBlock) Height() int64 { return f.h }
func (f fakeBlock) Hash() []byte  { return nil }

// supplierWithHistory builds a Supplier whose ServiceConfigHistory reflects
// the activation / deactivation heights the test specifies. Each entry is
// {serviceID, activationHeight, deactivationHeight}.
func supplierWithHistory(addr string, entries ...[3]any) *sharedtypes.Supplier {
	history := make([]*sharedtypes.ServiceConfigUpdate, 0, len(entries))
	for _, e := range entries {
		svcID := e[0].(string)
		act := int64(e[1].(int))
		deact := int64(e[2].(int))
		history = append(history, &sharedtypes.ServiceConfigUpdate{
			OperatorAddress:    addr,
			Service:            &sharedtypes.SupplierServiceConfig{ServiceId: svcID},
			ActivationHeight:   act,
			DeactivationHeight: deact,
		})
	}
	// Also populate .Services (denormalized current snapshot) with the
	// services that would naïvely look current to a consumer that ignores
	// heights. The test's invariant is that the manager ignores this and
	// uses the history-aware filter instead.
	servs := make([]*sharedtypes.SupplierServiceConfig, 0, len(history))
	for _, u := range history {
		servs = append(servs, u.Service)
	}
	return &sharedtypes.Supplier{
		OperatorAddress:      addr,
		Stake:                &cosmostypes.Coin{Denom: "upokt", Amount: sdkmath.NewInt(1000)},
		Services:             servs,
		ServiceConfigHistory: history,
	}
}

// servicesForSupplierFromCache extracts the services list the manager wrote
// to the supplier cache.
func servicesForSupplierFromCache(t *testing.T, sc *cache.SupplierCache, addr string) []string {
	t.Helper()
	state, err := sc.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	require.NotNil(t, state, "expected cached state for supplier %s", addr)
	out := append([]string(nil), state.Services...)
	return out
}

func newManagerForHistoryTest(t *testing.T, km *fakeKeyManager, qc *historySupplierQueryClient, bc *fakeBlockClient) (*SupplierManager, *cache.SupplierCache, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{URL: fmt.Sprintf("redis://%s", mr.Addr())})
	require.NoError(t, err)

	supplierCache := cache.NewSupplierCache(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		cache.SupplierCacheConfig{KeyPrefix: "ha:supplier"},
	)
	require.NoError(t, supplierCache.Start(ctx))

	registry := NewSupplierRegistry(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		SupplierRegistryConfig{},
	)

	pool := pond.NewPool(4)

	mgr := NewSupplierManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		km,
		registry,
		SupplierManagerConfig{
			RedisClient:               redisClient,
			MinerID:                   "test-miner",
			SupplierQueryClient:       qc,
			BlockClient:               bc,
			SupplierCache:             supplierCache,
			WorkerPool:                pool,
			SupplierReconcileInterval: -1, // disable background loop; tests call reconcile directly
		},
	)
	require.NoError(t, mgr.Start(ctx))

	cleanup := func() {
		_ = mgr.Close()
		_ = supplierCache.Close()
		_ = redisClient.Close()
		pool.StopAndWait()
		cancel()
		mr.Close()
	}
	return mgr, supplierCache, cleanup
}

// TestSupplierManager_Reconcile_RespectsServiceConfigHistory is the
// behavioural proof for the user's concern: when an operator re-stakes a
// supplier with a service removed, poktroll schedules the removal at the
// NEXT session boundary via ServiceConfigHistory (old entry gets
// deactivation_height = session_end, not pruned immediately). Until that
// height passes, the supplier must keep serving relays for the removed
// service — otherwise we reject relays the supplier was still entitled to
// process. Activation works the same way: a service whose
// activation_height is in the future must not be treated as active yet.
func TestSupplierManager_Reconcile_RespectsServiceConfigHistory(t *testing.T) {
	addr := "pokt1supplier_history"
	km := &fakeKeyManager{addrs: []string{addr}}
	qc := &historySupplierQueryClient{addr: addr}
	bc := &fakeBlockClient{}

	// Initial: 2 services, both active from height 100, no deactivation yet.
	qc.set(supplierWithHistory(addr,
		[3]any{"svc-http", 100, 0},
		[3]any{"svc-stream", 100, 0},
	))
	bc.height.Store(120)

	mgr, supplierCache, cleanup := newManagerForHistoryTest(t, km, qc, bc)
	defer cleanup()

	// First reconcile at height 120: both services active.
	mgr.reconcile(context.Background())
	svcs := servicesForSupplierFromCache(t, supplierCache, addr)
	require.ElementsMatch(t, []string{"svc-http", "svc-stream"}, svcs,
		"at height 120 both services must be active in the cache")

	// Operator re-stakes removing svc-stream. Chain schedules deactivation
	// at session end (height 130). Before that height, we still serve.
	qc.set(supplierWithHistory(addr,
		[3]any{"svc-http", 100, 0},
		[3]any{"svc-stream", 100, 130}, // scheduled deactivation
		[3]any{"svc-http", 125, 0},     // (hypothetical) same service reinstated
	))

	// At height 125 — before the session-end at 130 — svc-stream must
	// still appear because its deactivation height has not yet been reached.
	bc.height.Store(125)
	mgr.reconcile(context.Background())
	svcs = servicesForSupplierFromCache(t, supplierCache, addr)
	require.Contains(t, svcs, "svc-stream",
		"at height 125 svc-stream is still active (deactivation_height=130 not yet reached); dropping it now loses valid relays")
	require.Contains(t, svcs, "svc-http")

	// Cross the session boundary. At height 131 svc-stream must drop.
	bc.height.Store(131)
	mgr.reconcile(context.Background())
	svcs = servicesForSupplierFromCache(t, supplierCache, addr)
	require.NotContains(t, svcs, "svc-stream",
		"at height 131 svc-stream has passed its deactivation height and must be dropped from the cache")
	require.Contains(t, svcs, "svc-http")

	// Pre-scheduled activation: a new service-grpc is registered with
	// activation_height=200. At height 150 it must NOT appear yet;
	// at height 200 it must appear.
	qc.set(supplierWithHistory(addr,
		[3]any{"svc-http", 100, 0},
		[3]any{"svc-grpc", 200, 0},
	))
	bc.height.Store(150)
	mgr.reconcile(context.Background())
	svcs = servicesForSupplierFromCache(t, supplierCache, addr)
	require.NotContains(t, svcs, "svc-grpc",
		"at height 150 svc-grpc activation_height=200 has not been reached; must not be serviceable yet")
	bc.height.Store(200)
	mgr.reconcile(context.Background())
	svcs = servicesForSupplierFromCache(t, supplierCache, addr)
	require.Contains(t, svcs, "svc-grpc",
		"at height 200 svc-grpc activation_height is reached and service must be cached")

	_ = time.Second // keep time import used in case future tests need it
}
