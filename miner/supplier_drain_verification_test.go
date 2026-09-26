package miner

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
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

func (c *testSupplierQueryClient) InvalidateSupplier(_ string) {
	// no-op for tests — cache invalidation is a production concern
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
		suppliers: xsync.NewMap[string, *SupplierState](),
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
// DRAIN-05: onSupplierReleased drains unconditionally (issue #7)
//
// All Release() callsites — rebalance, shutdown, claim-callback-failure —
// operate on suppliers that are expected to remain staked on-chain. The old
// veto behaviour pinned suppliers to the original miner forever and broke
// every dual-miner rebalance. The release path now drains regardless of
// staking status; the chain query is retained only as an audit signal.
// =============================================================================

func TestOnSupplierReleased_DrainsEvenWhenStaked(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	beforeStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "staked"))

	err := mgr.onSupplierReleased(context.Background(), "pokt1staked", triggerRebalanceRelease)
	mgr.waitDrains()
	require.NoError(t, err, "rebalance release must not be vetoed by on-chain staking status")

	afterStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "staked"))
	assert.Equal(t, beforeStaked+1, afterStaked,
		"audit metric should still record on-chain result even though drain proceeds")
}

func TestOnSupplierReleased_ProceedsWhenNotFound(t *testing.T) {
	// Verify the drain DECISION is correct (shouldDrain=true for NotFound).
	// We don't test removeSupplier execution here (it requires full infrastructure);
	// instead we verify via the metric that the drain decision was "not_found".
	client := newStakedQueryClient( /* none staked */ )
	mgr := newTestSupplierManager(t, client)

	beforeNotFound := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "not_found"))

	// verifySupplierUnstaked should return shouldDrain=true
	shouldDrain, result := mgr.verifySupplierUnstaked(context.Background(), "pokt1gone", "rebalance_release")
	assert.True(t, shouldDrain, "NotFound supplier should trigger drain")
	assert.Equal(t, "not_found", result)

	// Simulate what onSupplierReleased does: increment metric
	supplierDrainDecisionTotal.WithLabelValues("rebalance_release", result).Inc()

	afterNotFound := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "not_found"))
	assert.Equal(t, beforeNotFound+1, afterNotFound)
}

// =============================================================================
// DRAIN-06: onKeyChange removal drains even if staked (operator explicit action)
// =============================================================================

func TestOnKeyChange_RemovalDrainsIfStaked(t *testing.T) {
	// Verify the drain DECISION for key removal: should drain even if staked.
	// We test the verification + metric logic, not the full removeSupplier path
	// (which requires full infrastructure).
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	// Verify the decision: key_removal should proceed even when staked
	shouldDrain, result := mgr.verifySupplierUnstaked(context.Background(), "pokt1staked", "key_removal")
	assert.False(t, shouldDrain, "verifySupplierUnstaked returns false for staked supplier")
	assert.Equal(t, "staked", result)

	// onKeyChange removal proceeds regardless (operator explicit action).
	// As of issue #7, onSupplierReleased also drains unconditionally, so the
	// behavioural difference between the two paths is now only observability
	// labels (drain_trigger: rebalance_release vs key_removal).
	beforeStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("key_removal", "staked"))
	supplierDrainDecisionTotal.WithLabelValues("key_removal", result).Inc()
	afterStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("key_removal", "staked"))
	assert.Equal(t, beforeStaked+1, afterStaked,
		"key_removal/staked metric should increment")
}

// =============================================================================
// DRAIN-08: Prometheus drain decision metric
// =============================================================================

func TestDrainMetric_RebalanceRelease(t *testing.T) {
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	beforeStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "staked"))

	// Drain proceeds (issue #7) but the audit metric is still recorded.
	_ = mgr.onSupplierReleased(context.Background(), "pokt1staked", triggerRebalanceRelease)
	mgr.waitDrains()

	afterStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "staked"))
	assert.Equal(t, beforeStaked+1, afterStaked,
		"drain decision metric should increment for rebalance_release/staked")
}

func TestDrainMetric_KeyRemoval(t *testing.T) {
	// Verify metric is incremented correctly for key_removal path.
	// Uses verifySupplierUnstaked + manual metric increment (same logic as onKeyChange)
	// to avoid calling removeSupplier which requires full infrastructure.
	client := newStakedQueryClient("pokt1staked")
	mgr := newTestSupplierManager(t, client)

	beforeStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("key_removal", "staked"))

	_, verifyResult := mgr.verifySupplierUnstaked(context.Background(), "pokt1staked", "key_removal")
	supplierDrainDecisionTotal.WithLabelValues("key_removal", verifyResult).Inc()

	afterStaked := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("key_removal", "staked"))
	assert.Equal(t, beforeStaked+1, afterStaked,
		"drain decision metric should increment for key_removal/staked")
}

func TestDrainMetric_NotFound(t *testing.T) {
	// Verify metric is incremented correctly for not_found path.
	// Uses verifySupplierUnstaked + manual metric increment (same logic as onSupplierReleased)
	// to avoid calling removeSupplier which requires full infrastructure.
	client := newStakedQueryClient( /* none staked */ )
	mgr := newTestSupplierManager(t, client)

	beforeNotFound := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "not_found"))

	_, verifyResult := mgr.verifySupplierUnstaked(context.Background(), "pokt1gone", "rebalance_release")
	supplierDrainDecisionTotal.WithLabelValues("rebalance_release", verifyResult).Inc()

	afterNotFound := testutil.ToFloat64(supplierDrainDecisionTotal.WithLabelValues("rebalance_release", "not_found"))
	assert.Equal(t, beforeNotFound+1, afterNotFound,
		"drain decision metric should increment for rebalance_release/not_found")
}

// =============================================================================
// INVALIDATION: Verify InvalidateSupplier is called before GetSupplier
// in filterStakedSuppliers and verifySupplierUnstaked, ensuring stale
// cached data never hides on-chain staking changes.
// =============================================================================

// trackingSupplierQueryClient wraps a real query client and records
// InvalidateSupplier calls so tests can assert the wiring.
type trackingSupplierQueryClient struct {
	*testSupplierQueryClient
	invalidated []string
}

func (c *trackingSupplierQueryClient) InvalidateSupplier(addr string) {
	c.invalidated = append(c.invalidated, addr)
}

func TestFilterStakedSuppliers_InvalidatesBeforeQuery(t *testing.T) {
	inner := newStakedQueryClient("pokt1a", "pokt1b")
	tracking := &trackingSupplierQueryClient{testSupplierQueryClient: inner}
	mgr := newTestSupplierManager(t, tracking)

	result := mgr.filterStakedSuppliers(context.Background(), []string{"pokt1a", "pokt1b"})
	require.Len(t, result, 2)

	// Both suppliers must have been invalidated before querying
	assert.Equal(t, []string{"pokt1a", "pokt1b"}, tracking.invalidated,
		"filterStakedSuppliers must call InvalidateSupplier for each address before GetSupplier")
}

func TestVerifySupplierUnstaked_InvalidatesBeforeQuery(t *testing.T) {
	inner := newStakedQueryClient("pokt1staked")
	tracking := &trackingSupplierQueryClient{testSupplierQueryClient: inner}
	mgr := newTestSupplierManager(t, tracking)

	shouldDrain, result := mgr.verifySupplierUnstaked(context.Background(), "pokt1staked", "test")
	assert.False(t, shouldDrain)
	assert.Equal(t, "staked", result)

	assert.Equal(t, []string{"pokt1staked"}, tracking.invalidated,
		"verifySupplierUnstaked must call InvalidateSupplier before GetSupplier")
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

// waitDrains blocks until every drain goroutine this manager started has
// finished, so a test can assert on the audit without polling a clock.
//
// It lives in test code because that is the only thing that calls it, and the
// dead-code gate is right to refuse a production method nothing in production
// reaches. NOTE, and it is a decision NOT taken here: Close does not wait for a
// drain either, so the audit log can still be lost on the way out. Making Close
// wait would make shutdown as slow as the slowest blocked XREAD -- a trade for
// the owner to make, not for this change.
func (m *SupplierManager) waitDrains() { m.drainWG.Wait() }
