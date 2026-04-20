//go:build test

package miner

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alitto/pond/v2"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	sdkmath "cosmossdk.io/math"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// fakeKeyManager is a minimal keys.KeyManager that only exposes
// ListSuppliers and OnKeyChange (other methods are unused by the
// reconcile path).
type fakeKeyManager struct{ addrs []string }

func (f *fakeKeyManager) ListSuppliers() []string                             { return f.addrs }
func (f *fakeKeyManager) OnKeyChange(_ keys.KeyChangeCallback)                {}
func (f *fakeKeyManager) GetSigner(string) (cryptotypes.PrivKey, error)       { return nil, fmt.Errorf("n/a") }
func (f *fakeKeyManager) HasKey(string) bool                                  { return false }
func (f *fakeKeyManager) AddKey(string, cryptotypes.PrivKey) error            { return nil }
func (f *fakeKeyManager) RemoveKey(string) error                              { return nil }
func (f *fakeKeyManager) Reload(context.Context) error                        { return nil }
func (f *fakeKeyManager) Start(context.Context) error                         { return nil }
func (f *fakeKeyManager) Close() error                                        { return nil }

// toggleableSupplierQueryClient flips between NotFound and staked based on
// the staked atomic flag, simulating an operator running MsgStakeSupplier
// after the miner started.
type toggleableSupplierQueryClient struct {
	addr   string
	staked atomic.Bool
}

func (t *toggleableSupplierQueryClient) GetSupplier(_ context.Context, operatorAddress string) (sharedtypes.Supplier, error) {
	if operatorAddress != t.addr {
		return sharedtypes.Supplier{}, status.Error(codes.NotFound, "unknown supplier")
	}
	if !t.staked.Load() {
		return sharedtypes.Supplier{}, status.Error(codes.NotFound, "not staked")
	}
	return sharedtypes.Supplier{
		OperatorAddress: t.addr,
		Stake:           &cosmostypes.Coin{Denom: "upokt", Amount: sdkmath.NewInt(1000)},
		Services: []*sharedtypes.SupplierServiceConfig{
			{ServiceId: "svc-a"},
		},
	}, nil
}

func (t *toggleableSupplierQueryClient) GetParams(context.Context) (*suppliertypes.Params, error) {
	return &suppliertypes.Params{}, nil
}

func (t *toggleableSupplierQueryClient) InvalidateSupplier(string) {}

// TestSupplierManager_Reconcile_PicksUpStakedAfterStart is the behavioural
// proof for the "operator stakes a supplier after miner startup" bug:
// before the fix, filterStakedSuppliers ran exactly once and a key whose
// supplier was not yet staked on-chain never made it into the claimer.
// After the fix, the periodic reconcile observes the stake transition and
// pushes the supplier into the claimer.
func TestSupplierManager_Reconcile_PicksUpStakedAfterStart(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer redisClient.Close()

	supplierAddr := "pokt1supplier_under_test"
	km := &fakeKeyManager{addrs: []string{supplierAddr}}
	qc := &toggleableSupplierQueryClient{addr: supplierAddr}
	// Supplier not yet staked on-chain at miner startup.
	qc.staked.Store(false)

	registry := NewSupplierRegistry(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		SupplierRegistryConfig{},
	)

	pool := pond.NewPool(4)
	defer pool.StopAndWait()

	mgr := NewSupplierManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		km,
		registry,
		SupplierManagerConfig{
			RedisClient:               redisClient,
			MinerID:                   "test-miner",
			SupplierQueryClient:       qc,
			WorkerPool:                pool,
			SupplierReconcileInterval: 0, // manual reconcile only for the test
		},
	)

	require.NoError(t, mgr.Start(ctx))
	defer func() { _ = mgr.Close() }()

	require.NotNil(t, mgr.claimer,
		"claimer must exist even when no supplier is staked at startup; the reconciler needs it to push newly-staked suppliers into")

	// Operator broadcasts MsgStakeSupplier; stake now visible on chain.
	qc.staked.Store(true)

	// Reconcile pass picks up the transition.
	mgr.reconcile(ctx)

	snapshot := claimedSuppliersSnapshot(mgr.claimer)
	require.Contains(t, snapshot, supplierAddr,
		"after reconcile, the claimer must know about the newly-staked supplier")

	// Supplier unstakes later; reconcile must drop it.
	qc.staked.Store(false)
	mgr.reconcile(ctx)

	snapshot = claimedSuppliersSnapshot(mgr.claimer)
	require.NotContains(t, snapshot, supplierAddr,
		"after reconcile sees unstaked supplier, claimer must no longer track it")
}

// TestSupplierManager_Reconcile_DefersRemovalWhilePendingSessions is the
// behavioural proof for the drain-gated unstake bug: when a supplier is
// unstaked on-chain while it still has a mid-flight session (claim not yet
// submitted), the reconcile path MUST keep the supplier in the claimer so
// the per-supplier pipeline (stream consumer, SMST, lifecycle manager)
// stays alive until the claim+proof work settles. Dropping it immediately
// would orphan the pending claim — the relays would never be submitted
// on-chain and revenue would be silently lost.
//
// Scenario:
//  1. Supplier is staked, claimer picks it up.
//  2. A pending session exists in Redis (state=claiming, i.e. flushed,
//     awaiting claim submission).
//  3. Supplier unstake transaction lands; chain starts returning NotFound.
//  4. reconcile must NOT remove the supplier — pending work would be lost.
//  5. Session transitions to a terminal state (proved).
//  6. reconcile now removes the supplier — the pipeline can be torn down.
func TestSupplierManager_Reconcile_DefersRemovalWhilePendingSessions(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer redisClient.Close()

	supplierAddr := "pokt1supplier_pending_drain"
	km := &fakeKeyManager{addrs: []string{supplierAddr}}
	qc := &toggleableSupplierQueryClient{addr: supplierAddr}
	qc.staked.Store(true)

	registry := NewSupplierRegistry(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		SupplierRegistryConfig{},
	)

	pool := pond.NewPool(4)
	defer pool.StopAndWait()

	mgr := NewSupplierManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		km,
		registry,
		SupplierManagerConfig{
			RedisClient:               redisClient,
			MinerID:                   "test-miner",
			SupplierQueryClient:       qc,
			WorkerPool:                pool,
			SessionTTL:                time.Hour,
			SupplierReconcileInterval: 0,
		},
	)

	require.NoError(t, mgr.Start(ctx))
	defer func() { _ = mgr.Close() }()

	// Seed a pending (non-terminal) session for the supplier. Represents
	// the mid-flight "relays accepted, claim not yet submitted" scenario.
	store := NewRedisSessionStore(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		SessionStoreConfig{
			KeyPrefix:       redisClient.KB().MinerSessionsPrefix(),
			SupplierAddress: supplierAddr,
			SessionTTL:      time.Hour,
		},
	)
	defer func() { _ = store.Close() }()

	pendingSessionID := "session_pending_claim"
	require.NoError(t, store.Save(ctx, &SessionSnapshot{
		SessionID:               pendingSessionID,
		SupplierOperatorAddress: supplierAddr,
		ServiceID:               "svc-a",
		State:                   SessionStateClaiming,
		RelayCount:              9999,
	}))

	// Initial reconcile to establish the supplier in the claimer list.
	mgr.reconcile(ctx)
	require.Contains(t, claimedSuppliersSnapshot(mgr.claimer), supplierAddr)

	// Supplier unstakes on-chain while the claim is still pending.
	qc.staked.Store(false)
	mgr.reconcile(ctx)

	require.Contains(t, claimedSuppliersSnapshot(mgr.claimer), supplierAddr,
		"supplier must NOT be dropped while it still has a non-terminal session — the pending claim would be orphaned")

	// Session reaches a terminal state (claim + proof settled).
	require.NoError(t, store.UpdateState(ctx, pendingSessionID, SessionStateProved))

	mgr.reconcile(ctx)
	require.NotContains(t, claimedSuppliersSnapshot(mgr.claimer), supplierAddr,
		"once every session is terminal, the unstaked supplier must be dropped so the pipeline can be torn down")
}

// claimedSuppliersSnapshot returns a copy of the claimer's configured
// supplier list. Reads allSuppliers under its mutex.
func claimedSuppliersSnapshot(c *SupplierClaimer) []string {
	c.allSuppliersMu.Lock()
	defer c.allSuppliersMu.Unlock()
	out := make([]string, len(c.allSuppliers))
	copy(out, c.allSuppliers)
	return out
}
