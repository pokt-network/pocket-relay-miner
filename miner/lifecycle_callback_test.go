package miner

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
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
