//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/alicebob/miniredis/v2"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// fakeSupplierQueryClient is a minimal SupplierQueryClient that lets each
// test script the exact behaviour of GetSupplier — success with arbitrary
// services, codes.NotFound, or a transient non-NotFound error — without
// touching the chain.
type fakeSupplierQueryClient struct {
	supplier   sharedtypes.Supplier
	err        error
	calls      int
	invalidate int
}

func (f *fakeSupplierQueryClient) GetSupplier(_ context.Context, operatorAddress string) (sharedtypes.Supplier, error) {
	f.calls++
	if f.err != nil {
		return sharedtypes.Supplier{}, f.err
	}
	return f.supplier, nil
}

func (f *fakeSupplierQueryClient) GetParams(context.Context) (*suppliertypes.Params, error) {
	return &suppliertypes.Params{}, nil
}

func (f *fakeSupplierQueryClient) InvalidateSupplier(string) { f.invalidate++ }

// newCacheTestSupplierManager wires a SupplierManager with the minimum deps
// needed to exercise resolveAndPublishSupplierState against a real
// miniredis-backed SupplierCache.
func newCacheTestSupplierManager(t *testing.T, qc *fakeSupplierQueryClient) (*SupplierManager, *cache.SupplierCache, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	ctx := context.Background()
	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	supplierCache := cache.NewSupplierCache(logger, redisClient, cache.SupplierCacheConfig{
		KeyPrefix: "test:ha:supplier",
	})

	// Build a minimal SupplierManager with only the fields the helper uses:
	// logger, config.SupplierCache, config.SupplierQueryClient, config.MinerID.
	// Start() is intentionally NOT called — we are testing the private helper
	// directly, not the reconcile / warmup pipeline.
	mgr := &SupplierManager{
		logger: logger,
		config: SupplierManagerConfig{
			SupplierCache:       supplierCache,
			SupplierQueryClient: qc, // nil-typed fake is still a nil interface if qc==nil
			MinerID:             "test-miner",
		},
	}
	if qc == nil {
		mgr.config.SupplierQueryClient = nil
	}
	return mgr, supplierCache, mr
}

// TestResolveAndPublish_ChainOK: happy path — chain query succeeds,
// cache is populated with Staked:true and the full service list.
func TestResolveAndPublish_ChainOK(t *testing.T) {
	const addr = "pokt1test_chain_ok"
	qc := &fakeSupplierQueryClient{
		supplier: sharedtypes.Supplier{
			OperatorAddress: addr,
			OwnerAddress:    "pokt1owner",
			Stake:           &cosmostypes.Coin{Denom: "upokt", Amount: sdkmath.NewInt(1000)},
			Services: []*sharedtypes.SupplierServiceConfig{
				{ServiceId: "svc-a"},
				{ServiceId: "svc-b"},
			},
		},
	}
	mgr, supplierCache, _ := newCacheTestSupplierManager(t, qc)

	owner, services := mgr.resolveAndPublishSupplierState(context.Background(), addr, nil)
	require.Equal(t, "pokt1owner", owner)
	require.Equal(t, []string{"svc-a", "svc-b"}, services)

	state, err := supplierCache.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	require.NotNil(t, state, "chain_ok must write a cache entry")
	require.True(t, state.Staked, "chain_ok must write Staked:true")
	require.Equal(t, cache.SupplierStatusActive, state.Status)
	require.Equal(t, []string{"svc-a", "svc-b"}, state.Services,
		"chain_ok must persist the exact service list from the chain")
	require.Equal(t, "pokt1owner", state.OwnerAddress)
	require.Equal(t, "test-miner", state.UpdatedBy)
}

// TestResolveAndPublish_ChainNotFound: supplier is legitimately unstaked.
// Must write Staked:false / Status:not_staked, NOT Staked:true+empty
// services. This is the path that corrects stale Staked:true entries
// when an operator unstakes.
func TestResolveAndPublish_ChainNotFound(t *testing.T) {
	const addr = "pokt1test_not_found"
	qc := &fakeSupplierQueryClient{err: status.Error(codes.NotFound, "not staked")}
	mgr, supplierCache, _ := newCacheTestSupplierManager(t, qc)

	owner, services := mgr.resolveAndPublishSupplierState(context.Background(), addr, nil)
	require.Empty(t, owner)
	require.Empty(t, services)

	state, err := supplierCache.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	require.NotNil(t, state, "chain_not_found must write a cache entry so stale Staked:true gets corrected")
	require.False(t, state.Staked, "NotFound must write Staked:false, NEVER Staked:true with empty services")
	require.Equal(t, cache.SupplierStatusNotStaked, state.Status)
	require.Empty(t, state.Services)
}

// TestResolveAndPublish_ChainTransientError_DoesNotOverwrite: the
// regression test for Jonathan's 119-supplier outage. A transient
// fullnode error during startup MUST NOT overwrite an existing cache
// entry with empty services. The previous (healthy) entry must survive
// unchanged so relayers can keep serving traffic until the next refresh.
func TestResolveAndPublish_ChainTransientError_DoesNotOverwrite(t *testing.T) {
	const addr = "pokt1test_transient"
	qc := &fakeSupplierQueryClient{err: status.Error(codes.Unavailable, "fullnode down")}
	mgr, supplierCache, _ := newCacheTestSupplierManager(t, qc)

	// Seed a healthy pre-existing entry (simulates state from a previous
	// successful refresh before the fullnode went down).
	preexisting := &cache.SupplierState{
		Status:          cache.SupplierStatusActive,
		Staked:          true,
		OperatorAddress: addr,
		OwnerAddress:    "pokt1owner_prev",
		Services:        []string{"svc-a", "svc-b", "svc-c", "svc-d", "svc-e", "svc-f", "svc-g", "svc-h"},
		UpdatedBy:       "previous-miner",
	}
	require.NoError(t, supplierCache.SetSupplierState(context.Background(), preexisting))

	owner, services := mgr.resolveAndPublishSupplierState(context.Background(), addr, nil)
	require.Empty(t, owner, "transient error must return empty locals")
	require.Empty(t, services)

	state, err := supplierCache.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	require.NotNil(t, state, "pre-existing entry must survive the transient failure")
	require.True(t, state.Staked, "pre-existing Staked:true must NOT be flipped to false on transient error")
	require.Equal(t, cache.SupplierStatusActive, state.Status)
	require.Equal(t, preexisting.Services, state.Services,
		"pre-existing services MUST survive unchanged — this is the whole point of the fix")
	require.Equal(t, "pokt1owner_prev", state.OwnerAddress)
	require.Equal(t, "previous-miner", state.UpdatedBy,
		"UpdatedBy must NOT be overwritten by a miner that failed to query the chain")
}

// TestResolveAndPublish_ChainTransientError_NoOverwrite_WhenEmpty:
// even with nothing pre-cached, a transient error must not create a
// Staked:true / Services:[] poisoned entry. The cache must remain empty.
func TestResolveAndPublish_ChainTransientError_NoOverwrite_WhenEmpty(t *testing.T) {
	const addr = "pokt1test_transient_empty"
	qc := &fakeSupplierQueryClient{err: errors.New("network timeout")}
	mgr, supplierCache, _ := newCacheTestSupplierManager(t, qc)

	_, _ = mgr.resolveAndPublishSupplierState(context.Background(), addr, nil)

	state, err := supplierCache.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	require.Nil(t, state, "transient chain error with no pre-existing entry must leave the cache empty, NEVER write Staked:true+[]")
}

// TestResolveAndPublish_Prewarmed: trusted path — prewarmed data always
// writes through, including services taken verbatim from warmup.
func TestResolveAndPublish_Prewarmed(t *testing.T) {
	const addr = "pokt1test_prewarmed"
	// No query client to prove we never hit it.
	mgr, supplierCache, _ := newCacheTestSupplierManager(t, nil)

	prewarmed := &SupplierWarmupData{
		OwnerAddress: "pokt1owner_pre",
		Services:     []string{"svc-x", "svc-y"},
	}

	owner, services := mgr.resolveAndPublishSupplierState(context.Background(), addr, prewarmed)
	require.Equal(t, "pokt1owner_pre", owner)
	require.Equal(t, []string{"svc-x", "svc-y"}, services)

	state, err := supplierCache.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.True(t, state.Staked)
	require.Equal(t, cache.SupplierStatusActive, state.Status)
	require.Equal(t, []string{"svc-x", "svc-y"}, state.Services)
	require.Equal(t, "pokt1owner_pre", state.OwnerAddress)
}
