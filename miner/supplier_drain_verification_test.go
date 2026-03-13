package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// --- Test double implementing SupplierQueryClient ---

// testSupplierQueryClient is a configurable test double for SupplierQueryClient.
// It implements the real interface with deterministic behavior — NOT a mock.
type testSupplierQueryClient struct {
	getSupplierFn func(ctx context.Context, addr string) (sharedtypes.Supplier, error)
}

func (c *testSupplierQueryClient) GetSupplier(ctx context.Context, addr string) (sharedtypes.Supplier, error) {
	return c.getSupplierFn(ctx, addr)
}

func (c *testSupplierQueryClient) GetParams(_ context.Context) (*suppliertypes.Params, error) {
	return &suppliertypes.Params{}, nil
}

// --- Helper constructors ---

// newStakedQueryClient returns a client that reports the given addresses as staked,
// and returns NotFound for all others.
func newStakedQueryClient(addrs ...string) *testSupplierQueryClient {
	stakedSet := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		stakedSet[a] = true
	}
	return &testSupplierQueryClient{
		getSupplierFn: func(_ context.Context, addr string) (sharedtypes.Supplier, error) {
			if stakedSet[addr] {
				return sharedtypes.Supplier{OperatorAddress: addr}, nil
			}
			return sharedtypes.Supplier{}, grpcstatus.Error(codes.NotFound, "supplier not found")
		},
	}
}

// newErrorQueryClient returns a client that always returns the given error.
func newErrorQueryClient(err error) *testSupplierQueryClient {
	return &testSupplierQueryClient{
		getSupplierFn: func(_ context.Context, _ string) (sharedtypes.Supplier, error) {
			return sharedtypes.Supplier{}, err
		},
	}
}

// --- Minimal SupplierManager for drain-verification tests ---

func newTestSupplierManager(t *testing.T, queryClient SupplierQueryClient) *SupplierManager {
	t.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return &SupplierManager{
		logger: logger,
		config: SupplierManagerConfig{
			SupplierQueryClient: queryClient,
			MinerID:             "test-instance",
		},
		suppliers: make(map[string]*SupplierState),
		ctx:       ctx,
		cancelFn:  cancel,
	}
}

// =============================================================================
// DRAIN-01: filterStakedSuppliers fail-open on transient errors
// =============================================================================

func TestFilterStakedSuppliers_FailOpen(t *testing.T) {
	// When GetSupplier returns a transient error (Unavailable), the supplier
	// must still be INCLUDED in the returned slice (fail-open).
	client := newErrorQueryClient(grpcstatus.Error(codes.Unavailable, "connection refused"))
	mgr := newTestSupplierManager(t, client)

	result := mgr.filterStakedSuppliers(context.Background(), []string{"pokt1transient"})
	require.Len(t, result, 1, "transient error should include supplier (fail-open)")
	assert.Equal(t, "pokt1transient", result[0])
}

func TestFilterStakedSuppliers_ExcludesNotFound(t *testing.T) {
	// NotFound means not staked — exclude.
	client := newStakedQueryClient( /* none staked */ )
	mgr := newTestSupplierManager(t, client)

	result := mgr.filterStakedSuppliers(context.Background(), []string{"pokt1gone"})
	require.Empty(t, result, "NotFound supplier should be excluded")
}

func TestFilterStakedSuppliers_IncludesStaked(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	result := mgr.filterStakedSuppliers(context.Background(), []string{"pokt1staked"})
	require.Len(t, result, 1)
	assert.Equal(t, "pokt1staked", result[0])
}

// =============================================================================
// DRAIN-02: verifySupplierUnstaked — supplier is staked
// =============================================================================

func TestVerifySupplierUnstaked_Staked(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	shouldDrain, result := mgr.verifySupplierUnstaked(context.Background(), "pokt1staked", "test")
	assert.False(t, shouldDrain, "staked supplier should not drain")
	assert.Equal(t, "staked", result)
}

// =============================================================================
// DRAIN-03: verifySupplierUnstaked — supplier not found (genuinely unstaked)
// =============================================================================

func TestVerifySupplierUnstaked_NotFound(t *testing.T) {
	client := newStakedQueryClient( /* none staked */ )
	mgr := newTestSupplierManager(t, client)

	shouldDrain, result := mgr.verifySupplierUnstaked(context.Background(), "pokt1gone", "test")
	assert.True(t, shouldDrain, "NotFound supplier should drain")
	assert.Equal(t, "not_found", result)
}

// =============================================================================
// DRAIN-04: verifySupplierUnstaked — network error (fail-safe: don't drain)
// =============================================================================

func TestVerifySupplierUnstaked_Error(t *testing.T) {
	client := newErrorQueryClient(grpcstatus.Error(codes.Unavailable, "connection refused"))
	mgr := newTestSupplierManager(t, client)

	shouldDrain, result := mgr.verifySupplierUnstaked(context.Background(), "pokt1error", "test")
	assert.False(t, shouldDrain, "network error should not drain (fail-safe)")
	assert.Equal(t, "error", result)
}

func TestVerifySupplierUnstaked_NoQueryClient(t *testing.T) {
	mgr := newTestSupplierManager(t, nil)

	shouldDrain, result := mgr.verifySupplierUnstaked(context.Background(), "pokt1any", "test")
	assert.False(t, shouldDrain, "no query client should not drain")
	assert.Equal(t, "no_query_client", result)
}

// =============================================================================
// DRAIN-05: onSupplierReleased aborts drain when supplier is staked
// =============================================================================

func TestOnSupplierReleased_AbortsWhenStaked(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	// Pre-populate supplier in the map
	mgr.suppliers["pokt1staked"] = &SupplierState{
		OperatorAddr: "pokt1staked",
		Status:       SupplierStatusActive,
	}

	err := mgr.onSupplierReleased(context.Background(), "pokt1staked")
	require.NoError(t, err)

	// Supplier should still exist (drain was aborted)
	mgr.suppliersMu.RLock()
	_, exists := mgr.suppliers["pokt1staked"]
	mgr.suppliersMu.RUnlock()
	assert.True(t, exists, "supplier should still exist after drain aborted (staked on-chain)")
}

func TestOnSupplierReleased_DrainsWhenNotFound(t *testing.T) {
	client := newStakedQueryClient( /* none staked */ )
	mgr := newTestSupplierManager(t, client)

	// Pre-populate supplier
	supplierCtx, cancelFn := context.WithCancel(context.Background())
	mgr.suppliers["pokt1gone"] = &SupplierState{
		OperatorAddr: "pokt1gone",
		Status:       SupplierStatusActive,
		cancelFn:     cancelFn,
		wg:           sync.WaitGroup{},
	}
	_ = supplierCtx // context managed by the state

	err := mgr.onSupplierReleased(context.Background(), "pokt1gone")
	require.NoError(t, err)

	// Give removeSupplier a moment to process (it's synchronous in onSupplierReleased)
	// The supplier should be marked as draining
	mgr.suppliersMu.RLock()
	state, exists := mgr.suppliers["pokt1gone"]
	mgr.suppliersMu.RUnlock()
	if exists {
		assert.Equal(t, SupplierStatusDraining, state.Status, "supplier should be draining")
	}
	// If it doesn't exist, removeSupplier already completed — also acceptable
}

// =============================================================================
// DRAIN-06: onKeyChange removal drains even if staked (operator explicit action)
// =============================================================================

func TestOnKeyChange_RemovalDrainsIfStaked(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	// Pre-populate supplier
	supplierCtx, cancelFn := context.WithCancel(context.Background())
	mgr.suppliers["pokt1staked"] = &SupplierState{
		OperatorAddr: "pokt1staked",
		Status:       SupplierStatusActive,
		cancelFn:     cancelFn,
		wg:           sync.WaitGroup{},
	}
	_ = supplierCtx

	// onKeyChange with added=false triggers removal
	mgr.onKeyChange("pokt1staked", false)

	// Wait briefly for the goroutine to execute removeSupplier
	time.Sleep(100 * time.Millisecond)

	// Supplier should be draining or removed (key removal proceeds even if staked)
	mgr.suppliersMu.RLock()
	state, exists := mgr.suppliers["pokt1staked"]
	mgr.suppliersMu.RUnlock()
	if exists {
		assert.Equal(t, SupplierStatusDraining, state.Status,
			"supplier should be draining after key removal even though staked")
	}
	// If removed entirely, that's also correct behavior
}

// =============================================================================
// DRAIN-08: Prometheus drain decision metric
// =============================================================================

func TestDrainMetric_RebalanceRelease(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	// Pre-populate supplier
	mgr.suppliers["pokt1staked"] = &SupplierState{
		OperatorAddr: "pokt1staked",
		Status:       SupplierStatusActive,
	}

	// Get the counter value before
	beforeStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "staked"))

	// Trigger drain decision (will be aborted because staked)
	_ = mgr.onSupplierReleased(context.Background(), "pokt1staked")

	// Counter should have incremented
	afterStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "staked"))
	assert.Equal(t, beforeStaked+1, afterStaked,
		"drain decision metric should increment for rebalance_release/staked")
}

func TestDrainMetric_KeyRemoval(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	// Pre-populate supplier
	supplierCtx, cancelFn := context.WithCancel(context.Background())
	mgr.suppliers["pokt1staked"] = &SupplierState{
		OperatorAddr: "pokt1staked",
		Status:       SupplierStatusActive,
		cancelFn:     cancelFn,
		wg:           sync.WaitGroup{},
	}
	_ = supplierCtx

	beforeStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("key_removal", "staked"))

	mgr.onKeyChange("pokt1staked", false)
	time.Sleep(100 * time.Millisecond)

	afterStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("key_removal", "staked"))
	assert.Equal(t, beforeStaked+1, afterStaked,
		"drain decision metric should increment for key_removal/staked")
}

func TestDrainMetric_NotFound(t *testing.T) {
	client := newStakedQueryClient( /* none staked */ )
	mgr := newTestSupplierManager(t, client)

	supplierCtx, cancelFn := context.WithCancel(context.Background())
	mgr.suppliers["pokt1gone"] = &SupplierState{
		OperatorAddr: "pokt1gone",
		Status:       SupplierStatusActive,
		cancelFn:     cancelFn,
		wg:           sync.WaitGroup{},
	}
	_ = supplierCtx

	beforeNotFound := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "not_found"))

	_ = mgr.onSupplierReleased(context.Background(), "pokt1gone")

	afterNotFound := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "not_found"))
	assert.Equal(t, beforeNotFound+1, afterNotFound,
		"drain decision metric should increment for rebalance_release/not_found")
}

// Verify the metric is registered with correct labels using the MinerRegistry
func TestDrainMetric_RegisteredInMinerRegistry(t *testing.T) {
	// The metric should be gatherable from the MinerRegistry
	families, err := observability.MinerRegistry.Gather()
	require.NoError(t, err)

	found := false
	for _, mf := range families {
		if mf.GetName() == "ha_miner_supplier_drain_decision_total" {
			found = true
			break
		}
	}
	assert.True(t, found, "supplier_drain_decision_total should be registered in MinerRegistry")
}
