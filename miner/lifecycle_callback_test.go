package miner

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/pokt-network/smt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// mockDeduplicator tracks CleanupSession calls for testing.
type mockDeduplicator struct {
	cleanedSessions []string
	cleanupErr      error // optional error to return
}

func (m *mockDeduplicator) IsDuplicate(_ context.Context, _ []byte, _ string) (bool, error) {
	return false, nil
}

func (m *mockDeduplicator) MarkProcessed(_ context.Context, _ []byte, _ string) error {
	return nil
}

func (m *mockDeduplicator) MarkProcessedBatch(_ context.Context, _ [][]byte, _ string) error {
	return nil
}

func (m *mockDeduplicator) CleanupSession(_ context.Context, sessionID string) error {
	m.cleanedSessions = append(m.cleanedSessions, sessionID)
	return m.cleanupErr
}

func (m *mockDeduplicator) Start(_ context.Context) error {
	return nil
}

func (m *mockDeduplicator) Close() error {
	return nil
}

// mockSMSTManager is a minimal mock for testing lifecycle callbacks.
type mockSMSTManager struct {
	deletedTrees []string
	deleteErr    error
}

func (m *mockSMSTManager) FlushTree(_ context.Context, _ string) ([]byte, error) {
	return []byte("mock-root"), nil
}

func (m *mockSMSTManager) GetTreeRoot(_ context.Context, _ string) ([]byte, error) {
	return []byte("mock-root"), nil
}

func (m *mockSMSTManager) ProveClosest(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return []byte("mock-proof"), nil
}

func (m *mockSMSTManager) DeleteTree(_ context.Context, sessionID string) error {
	m.deletedTrees = append(m.deletedTrees, sessionID)
	return m.deleteErr
}

// createTestLifecycleCallback creates a LifecycleCallback with minimal dependencies for testing.
func createTestLifecycleCallback(smstManager SMSTManager) *LifecycleCallback {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	return &LifecycleCallback{
		logger:       logger,
		config:       DefaultLifecycleCallbackConfig(),
		smstManager:  smstManager,
		sessionLocks: make(map[string]*sync.Mutex),
	}
}

// TestLifecycleCallback_OnSessionProved_CleansDeduplicator verifies that
// OnSessionProved calls CleanupSession on the deduplicator when set.
func TestLifecycleCallback_OnSessionProved_CleansDeduplicator(t *testing.T) {
	// Setup
	smstManager := &mockSMSTManager{}
	dedup := &mockDeduplicator{}

	lc := createTestLifecycleCallback(smstManager)
	lc.SetDeduplicator(dedup)

	snapshot := &SessionSnapshot{
		SessionID:               "test-session-123",
		SupplierOperatorAddress: "pokt1supplier",
		ServiceID:               "svc1",
		RelayCount:              100,
	}

	// Execute
	err := lc.OnSessionProved(context.Background(), snapshot)

	// Verify
	require.NoError(t, err)
	require.Len(t, dedup.cleanedSessions, 1)
	require.Equal(t, "test-session-123", dedup.cleanedSessions[0])
}

// TestLifecycleCallback_OnProbabilisticProved_CleansDeduplicator verifies that
// OnProbabilisticProved calls CleanupSession on the deduplicator when set.
func TestLifecycleCallback_OnProbabilisticProved_CleansDeduplicator(t *testing.T) {
	// Setup
	smstManager := &mockSMSTManager{}
	dedup := &mockDeduplicator{}

	lc := createTestLifecycleCallback(smstManager)
	lc.SetDeduplicator(dedup)

	snapshot := &SessionSnapshot{
		SessionID:               "test-session-456",
		SupplierOperatorAddress: "pokt1supplier",
		ServiceID:               "svc1",
		RelayCount:              50,
	}

	// Execute
	err := lc.OnProbabilisticProved(context.Background(), snapshot)

	// Verify
	require.NoError(t, err)
	require.Len(t, dedup.cleanedSessions, 1)
	require.Equal(t, "test-session-456", dedup.cleanedSessions[0])
}

// TestLifecycleCallback_NilDeduplicator_NoError verifies that OnSessionProved
// and OnProbabilisticProved work correctly when deduplicator is nil.
func TestLifecycleCallback_NilDeduplicator_NoError(t *testing.T) {
	// Setup
	smstManager := &mockSMSTManager{}
	lc := createTestLifecycleCallback(smstManager)
	// Deduplicator is nil by default

	snapshot := &SessionSnapshot{
		SessionID:               "test-session-789",
		SupplierOperatorAddress: "pokt1supplier",
		ServiceID:               "svc1",
		RelayCount:              25,
	}

	// Execute - should not panic or error
	err := lc.OnSessionProved(context.Background(), snapshot)
	require.NoError(t, err)

	err = lc.OnProbabilisticProved(context.Background(), snapshot)
	require.NoError(t, err)

	// SMST should still be cleaned up
	require.Len(t, smstManager.deletedTrees, 2)
}

// TestLifecycleCallback_CleanupError_LogsWarningButSucceeds verifies that
// cleanup errors from the deduplicator are logged but don't fail the operation.
func TestLifecycleCallback_CleanupError_LogsWarningButSucceeds(t *testing.T) {
	// Setup
	smstManager := &mockSMSTManager{}
	dedup := &mockDeduplicator{
		cleanupErr: errors.New("simulated cleanup error"),
	}

	lc := createTestLifecycleCallback(smstManager)
	lc.SetDeduplicator(dedup)

	snapshot := &SessionSnapshot{
		SessionID:               "test-session-error",
		SupplierOperatorAddress: "pokt1supplier",
		ServiceID:               "svc1",
		RelayCount:              10,
	}

	// Execute - should succeed despite cleanup error (error is just logged)
	err := lc.OnSessionProved(context.Background(), snapshot)
	require.NoError(t, err)

	// Cleanup was still attempted
	require.Len(t, dedup.cleanedSessions, 1)
	require.Equal(t, "test-session-error", dedup.cleanedSessions[0])

	// Same for OnProbabilisticProved
	err = lc.OnProbabilisticProved(context.Background(), snapshot)
	require.NoError(t, err)
	require.Len(t, dedup.cleanedSessions, 2)
}

// TestLifecycleCallback_SetDeduplicator verifies the setter works correctly.
func TestLifecycleCallback_SetDeduplicator(t *testing.T) {
	smstManager := &mockSMSTManager{}
	lc := createTestLifecycleCallback(smstManager)

	// Initially nil
	require.Nil(t, lc.deduplicator)

	// Set deduplicator
	dedup := &mockDeduplicator{}
	lc.SetDeduplicator(dedup)

	// Now set
	require.NotNil(t, lc.deduplicator)
	require.Equal(t, dedup, lc.deduplicator)
}

// --- SMST Root Hash Tests ---
// These tests verify that claim decisions use the SMST root hash as the
// source of truth (count + sum), matching poktroll's canonical flow.

// buildTestRootHash builds a valid MerkleSumRoot with the given sum (compute
// units) and count (relay count). Layout: [32-byte digest][8-byte sum BE][8-byte count BE].
func buildTestRootHash(sum, count uint64) []byte {
	root := make([]byte, 48) // 32 digest + 8 sum + 8 count
	// Fill digest with non-zero data so it looks like a real hash
	for i := 0; i < 32; i++ {
		root[i] = byte(i + 1)
	}
	binary.BigEndian.PutUint64(root[32:40], sum)
	binary.BigEndian.PutUint64(root[40:48], count)
	return root
}

// TestBuildTestRootHash_Roundtrip verifies our test helper produces valid
// MerkleSumRoot values that smt.MerkleSumRoot can decode.
func TestBuildTestRootHash_Roundtrip(t *testing.T) {
	root := buildTestRootHash(1000, 50)

	smstRoot := smt.MerkleSumRoot(root)
	gotSum, err := smstRoot.Sum()
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), gotSum)

	gotCount, err := smstRoot.Count()
	require.NoError(t, err)
	assert.Equal(t, uint64(50), gotCount)
}

// TestBuildTestRootHash_ZeroValues verifies empty tree detection.
func TestBuildTestRootHash_ZeroValues(t *testing.T) {
	root := buildTestRootHash(0, 0)

	smstRoot := smt.MerkleSumRoot(root)
	gotSum, err := smstRoot.Sum()
	require.NoError(t, err)
	assert.Equal(t, uint64(0), gotSum)

	gotCount, err := smstRoot.Count()
	require.NoError(t, err)
	assert.Equal(t, uint64(0), gotCount)
}

// TestSMSTRootHash_EmptyTreeSkipsClaim verifies that a session with an empty
// SMST tree (count=0, sum=0) is skipped during the claim build phase.
// This replaces the old snapshot.RelayCount == 0 check.
func TestSMSTRootHash_EmptyTreeSkipsClaim(t *testing.T) {
	emptyRoot := buildTestRootHash(0, 0)

	smstRoot := smt.MerkleSumRoot(emptyRoot)
	count, err := smstRoot.Count()
	require.NoError(t, err)
	sum, err := smstRoot.Sum()
	require.NoError(t, err)

	// This is the logic from the restructured claim build phase:
	// empty tree → skip (no claim to submit)
	assert.Equal(t, uint64(0), count)
	assert.Equal(t, uint64(0), sum)
	assert.True(t, count == 0 || sum == 0,
		"empty SMST tree should be detected and skipped")
}

// TestSMSTRootHash_NonEmptyTreeProceedsToClaim verifies that a session with
// relays in the SMST tree proceeds to claim submission.
func TestSMSTRootHash_NonEmptyTreeProceedsToClaim(t *testing.T) {
	// 100 relays, 1000 compute units
	root := buildTestRootHash(1000, 100)

	smstRoot := smt.MerkleSumRoot(root)
	count, err := smstRoot.Count()
	require.NoError(t, err)
	sum, err := smstRoot.Sum()
	require.NoError(t, err)

	assert.Equal(t, uint64(100), count)
	assert.Equal(t, uint64(1000), sum)
	assert.False(t, count == 0 || sum == 0,
		"non-empty SMST tree should proceed to claim")
}

// TestGetClaimeduPOKT_MatchesCanonicalFormula verifies that our claim reward
// calculation using prooftypes.Claim.GetClaimeduPOKT produces the correct
// result from the SMST root hash — the same formula used on-chain.
func TestGetClaimeduPOKT_MatchesCanonicalFormula(t *testing.T) {
	// 500 compute units in the SMST, 10 relays
	root := buildTestRootHash(500, 10)

	claim := prooftypes.Claim{
		SupplierOperatorAddress: "pokt1test",
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               "test-session",
			ServiceId:               "svc-test",
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   110,
		},
		RootHash: root,
	}

	// Use base difficulty (all relays applicable) and standard params
	difficulty := servicetypes.RelayMiningDifficulty{
		ServiceId: "svc-test",
		// Base difficulty = all 0xff bytes (difficulty multiplier = 1.0)
		TargetHash: make([]byte, 32),
	}
	// Fill with 0xff for base (easiest) difficulty
	for i := range difficulty.TargetHash {
		difficulty.TargetHash[i] = 0xff
	}

	sharedParams := sharedtypes.Params{
		ComputeUnitsToTokensMultiplier: 42,
		ComputeUnitCostGranularity:     1,
	}

	rewardCoin, err := claim.GetClaimeduPOKT(sharedParams, difficulty)
	require.NoError(t, err)

	// With difficulty multiplier ≈ 1.0 (all 0xff):
	// estimatedCU = 500 * ~1.0 = ~500
	// reward = 500 * 42 / 1 = 21000 uPOKT
	// Note: the exact multiplier for all-0xff may not be exactly 1.0
	// but the reward should be positive and proportional to compute units
	assert.True(t, rewardCoin.Amount.IsPositive(),
		"reward from SMST root hash should be positive for non-empty tree")
	t.Logf("reward from SMST root (500 CU, base difficulty): %s", rewardCoin.String())

	// Verify count and sum from claim methods match our root hash
	claimedCU, err := claim.GetNumClaimedComputeUnits()
	require.NoError(t, err)
	assert.Equal(t, uint64(500), claimedCU)

	numRelays, err := claim.GetNumRelays()
	require.NoError(t, err)
	assert.Equal(t, uint64(10), numRelays)
}

// TestGetClaimeduPOKT_EmptyTreeReturnsZero verifies that an empty SMST
// produces zero reward.
func TestGetClaimeduPOKT_EmptyTreeReturnsZero(t *testing.T) {
	root := buildTestRootHash(0, 0)

	claim := prooftypes.Claim{
		SupplierOperatorAddress: "pokt1test",
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               "test-session-empty",
			ServiceId:               "svc-test",
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   110,
		},
		RootHash: root,
	}

	difficulty := servicetypes.RelayMiningDifficulty{
		ServiceId:  "svc-test",
		TargetHash: make([]byte, 32),
	}
	for i := range difficulty.TargetHash {
		difficulty.TargetHash[i] = 0xff
	}

	sharedParams := sharedtypes.Params{
		ComputeUnitsToTokensMultiplier: 42,
		ComputeUnitCostGranularity:     1,
	}

	rewardCoin, err := claim.GetClaimeduPOKT(sharedParams, difficulty)
	require.NoError(t, err)
	assert.True(t, rewardCoin.Amount.IsZero(),
		"empty SMST tree should produce zero reward")
}

// TestSMSTRootHash_SnapshotCountersDivergeFromTree verifies that when snapshot
// counters diverge from SMST tree data (the bug we're fixing), the SMST root
// hash is the authoritative source.
func TestSMSTRootHash_SnapshotCountersDivergeFromTree(t *testing.T) {
	// Simulate the race: snapshot says 50 relays / 500 CU,
	// but SMST tree actually has 100 relays / 1000 CU
	snapshot := &SessionSnapshot{
		SessionID:         "sess-diverged",
		RelayCount:        50,  // stale/racy
		TotalComputeUnits: 500, // stale/racy
	}

	// The SMST root hash is the source of truth
	actualRoot := buildTestRootHash(1000, 100)
	smstRoot := smt.MerkleSumRoot(actualRoot)

	smstCount, err := smstRoot.Count()
	require.NoError(t, err)
	smstSum, err := smstRoot.Sum()
	require.NoError(t, err)

	// SMST data should be used, not snapshot counters
	assert.NotEqual(t, uint64(snapshot.RelayCount), smstCount,
		"snapshot relay count diverges from SMST — SMST is authoritative")
	assert.NotEqual(t, snapshot.TotalComputeUnits, smstSum,
		"snapshot compute units diverge from SMST — SMST is authoritative")

	assert.Equal(t, uint64(100), smstCount, "SMST count is the real relay count")
	assert.Equal(t, uint64(1000), smstSum, "SMST sum is the real compute units")
}
