//go:build test

package observability

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewInstructionTimer tests creating a new instruction timer.
func TestNewInstructionTimer(t *testing.T) {
	timer := NewInstructionTimer()
	require.NotNil(t, timer, "Timer should not be nil")
	require.NotNil(t, timer.Timestamps, "Timestamps should be initialized")
	require.Len(t, timer.Timestamps, 0, "Timestamps should be empty initially")
	require.Equal(t, 16, cap(timer.Timestamps), "Timestamps should have capacity 16")
}

// TestInstructionTimer_Record tests recording an instruction.
func TestInstructionTimer_Record(t *testing.T) {
	timer := NewInstructionTimer()

	timer.Record(InstructionRelayReceived)
	require.Len(t, timer.Timestamps, 1, "Should have one timestamp")
	require.Equal(t, InstructionRelayReceived, timer.Timestamps[0].instruction)

	timer.Record(InstructionParseRelayRequest)
	require.Len(t, timer.Timestamps, 2, "Should have two timestamps")
	require.Equal(t, InstructionParseRelayRequest, timer.Timestamps[1].instruction)
}

// TestInstructionTimer_RecordWithTimestamp tests recording with specific timestamp.
func TestInstructionTimer_RecordWithTimestamp(t *testing.T) {
	timer := NewInstructionTimer()
	ts := time.Now().Add(-1 * time.Minute)

	timer.RecordWithTimestamp(InstructionRelayReceived, ts)
	require.Len(t, timer.Timestamps, 1)
	require.Equal(t, ts, timer.Timestamps[0].timestamp, "Timestamp should match")
}

// TestInstructionTimer_GetDurations tests calculating durations between instructions.
func TestInstructionTimer_GetDurations(t *testing.T) {
	timer := NewInstructionTimer()

	now := time.Now()
	timer.RecordWithTimestamp(InstructionRelayReceived, now)
	timer.RecordWithTimestamp(InstructionParseRelayRequest, now.Add(10*time.Millisecond))
	timer.RecordWithTimestamp(InstructionValidateRelayRequest, now.Add(20*time.Millisecond))

	durations := timer.GetDurations()
	require.Len(t, durations, 2, "Should have durations for 2 transitions")
	require.Equal(t, 10*time.Millisecond, durations[InstructionParseRelayRequest])
	require.Equal(t, 10*time.Millisecond, durations[InstructionValidateRelayRequest])
}

// TestInstructionTimer_GetDurations_Empty tests getting durations with no timestamps.
func TestInstructionTimer_GetDurations_Empty(t *testing.T) {
	timer := NewInstructionTimer()
	durations := timer.GetDurations()
	require.Empty(t, durations, "Durations should be empty")
}

// TestInstructionTimer_GetDurations_Single tests getting durations with single timestamp.
func TestInstructionTimer_GetDurations_Single(t *testing.T) {
	timer := NewInstructionTimer()
	timer.Record(InstructionRelayReceived)

	durations := timer.GetDurations()
	require.Empty(t, durations, "Durations should be empty with single timestamp")
}

// TestInstructionTimer_TotalDuration tests calculating total duration.
func TestInstructionTimer_TotalDuration(t *testing.T) {
	timer := NewInstructionTimer()

	now := time.Now()
	timer.RecordWithTimestamp(InstructionRelayReceived, now)
	timer.RecordWithTimestamp(InstructionParseRelayRequest, now.Add(10*time.Millisecond))
	timer.RecordWithTimestamp(InstructionSendClientResponse, now.Add(50*time.Millisecond))

	totalDuration := timer.TotalDuration()
	require.Equal(t, 50*time.Millisecond, totalDuration, "Total duration should be 50ms")
}

// TestInstructionTimer_TotalDuration_Empty tests total duration with no timestamps.
func TestInstructionTimer_TotalDuration_Empty(t *testing.T) {
	timer := NewInstructionTimer()
	totalDuration := timer.TotalDuration()
	require.Equal(t, time.Duration(0), totalDuration, "Total duration should be 0")
}

// TestInstructionTimer_TotalDuration_Single tests total duration with single timestamp.
func TestInstructionTimer_TotalDuration_Single(t *testing.T) {
	timer := NewInstructionTimer()
	timer.Record(InstructionRelayReceived)

	totalDuration := timer.TotalDuration()
	require.Equal(t, time.Duration(0), totalDuration, "Total duration should be 0 with single timestamp")
}

// TestInstructionTimer_Reset tests resetting the timer.
func TestInstructionTimer_Reset(t *testing.T) {
	timer := NewInstructionTimer()

	timer.Record(InstructionRelayReceived)
	timer.Record(InstructionParseRelayRequest)
	timer.Record(InstructionValidateRelayRequest)
	require.Len(t, timer.Timestamps, 3)

	timer.Reset()
	require.Len(t, timer.Timestamps, 0, "Timestamps should be empty after reset")
	require.NotNil(t, timer.Timestamps, "Timestamps should not be nil after reset")
}

// TestInstructionTimer_MultipleResets tests multiple reset calls.
func TestInstructionTimer_MultipleResets(t *testing.T) {
	timer := NewInstructionTimer()

	timer.Record(InstructionRelayReceived)
	timer.Reset()
	require.Len(t, timer.Timestamps, 0)

	timer.Record(InstructionParseRelayRequest)
	require.Len(t, timer.Timestamps, 1)

	timer.Reset()
	require.Len(t, timer.Timestamps, 0)

	timer.Reset() // Reset empty timer
	require.Len(t, timer.Timestamps, 0)
}

// TestRecordDurations tests the RecordDurations helper function.
func TestRecordDurations(t *testing.T) {
	now := time.Now()
	timestamps := []*InstructionTimestamp{
		{instruction: InstructionRelayReceived, timestamp: now},
		{instruction: InstructionParseRelayRequest, timestamp: now.Add(5 * time.Millisecond)},
		{instruction: InstructionValidateRelayRequest, timestamp: now.Add(10 * time.Millisecond)},
	}

	// This will record to global metrics (promauto)
	RecordDurations("test_component", timestamps)
	// Ensure no panic
}

// TestRecordDurations_Empty tests RecordDurations with empty timestamps.
func TestRecordDurations_Empty(t *testing.T) {
	RecordDurations("test_component", []*InstructionTimestamp{})
	// Should not panic
}

// TestRecordDurations_Single tests RecordDurations with single timestamp.
func TestRecordDurations_Single(t *testing.T) {
	now := time.Now()
	timestamps := []*InstructionTimestamp{
		{instruction: InstructionRelayReceived, timestamp: now},
	}

	RecordDurations("test_component", timestamps)
	// Should not panic (no durations to record)
}

// TestInstructionTimer_RelayFlow tests a typical relay processing flow.
func TestInstructionTimer_RelayFlow(t *testing.T) {
	timer := NewInstructionTimer()
	now := time.Now()

	// Simulate relay processing flow
	timer.RecordWithTimestamp(InstructionRelayReceived, now)
	timer.RecordWithTimestamp(InstructionParseRelayRequest, now.Add(1*time.Millisecond))
	timer.RecordWithTimestamp(InstructionValidateRelayRequest, now.Add(3*time.Millisecond))
	timer.RecordWithTimestamp(InstructionGetSession, now.Add(5*time.Millisecond))
	timer.RecordWithTimestamp(InstructionForwardToBackend, now.Add(10*time.Millisecond))
	timer.RecordWithTimestamp(InstructionSignResponse, now.Add(50*time.Millisecond))
	timer.RecordWithTimestamp(InstructionPublishToRedis, now.Add(52*time.Millisecond))
	timer.RecordWithTimestamp(InstructionSendClientResponse, now.Add(53*time.Millisecond))

	durations := timer.GetDurations()
	require.Len(t, durations, 7, "Should have 7 instruction durations")

	totalDuration := timer.TotalDuration()
	require.Equal(t, 53*time.Millisecond, totalDuration, "Total should be 53ms")

	// Verify specific durations
	require.Equal(t, 1*time.Millisecond, durations[InstructionParseRelayRequest])
	require.Equal(t, 2*time.Millisecond, durations[InstructionValidateRelayRequest])
	require.Equal(t, 40*time.Millisecond, durations[InstructionSignResponse])
}

// TestInstructionTimer_MinerFlow tests a typical miner processing flow.
func TestInstructionTimer_MinerFlow(t *testing.T) {
	timer := NewInstructionTimer()
	now := time.Now()

	// Simulate miner claim flow
	timer.RecordWithTimestamp(InstructionConsumeFromStream, now)
	timer.RecordWithTimestamp(InstructionDeserializeRelay, now.Add(1*time.Millisecond))
	timer.RecordWithTimestamp(InstructionCheckDuplicateRelay, now.Add(2*time.Millisecond))
	timer.RecordWithTimestamp(InstructionGetOrCreateSessionTree, now.Add(5*time.Millisecond))
	timer.RecordWithTimestamp(InstructionUpdateSessionTree, now.Add(10*time.Millisecond))
	timer.RecordWithTimestamp(InstructionMarkRelayProcessed, now.Add(12*time.Millisecond))
	timer.RecordWithTimestamp(InstructionAcknowledgeMessage, now.Add(13*time.Millisecond))

	durations := timer.GetDurations()
	require.Len(t, durations, 6)

	totalDuration := timer.TotalDuration()
	require.Equal(t, 13*time.Millisecond, totalDuration)
}

// TestInstructionTimer_ClaimFlow tests claim submission flow.
func TestInstructionTimer_ClaimFlow(t *testing.T) {
	timer := NewInstructionTimer()
	now := time.Now()

	// Simulate claim flow
	timer.RecordWithTimestamp(InstructionWaitForClaimWindow, now)
	timer.RecordWithTimestamp(InstructionFlushSessionTree, now.Add(100*time.Millisecond))
	timer.RecordWithTimestamp(InstructionCalculateClaimRoot, now.Add(110*time.Millisecond))
	timer.RecordWithTimestamp(InstructionBuildClaimMessage, now.Add(115*time.Millisecond))
	timer.RecordWithTimestamp(InstructionSubmitClaim, now.Add(120*time.Millisecond))
	timer.RecordWithTimestamp(InstructionWaitForClaimTx, now.Add(5120*time.Millisecond))

	totalDuration := timer.TotalDuration()
	require.Equal(t, 5120*time.Millisecond, totalDuration)

	// Verify claim submission is the longest step
	durations := timer.GetDurations()
	require.Equal(t, 5000*time.Millisecond, durations[InstructionWaitForClaimTx])
}

// TestInstructionTimer_ProofFlow tests proof submission flow.
func TestInstructionTimer_ProofFlow(t *testing.T) {
	timer := NewInstructionTimer()
	now := time.Now()

	// Simulate proof flow
	timer.RecordWithTimestamp(InstructionWaitForProofWindow, now)
	timer.RecordWithTimestamp(InstructionGetProofPath, now.Add(100*time.Millisecond))
	timer.RecordWithTimestamp(InstructionGenerateClosestProof, now.Add(150*time.Millisecond))
	timer.RecordWithTimestamp(InstructionBuildProofMessage, now.Add(160*time.Millisecond))
	timer.RecordWithTimestamp(InstructionSubmitProof, now.Add(170*time.Millisecond))
	timer.RecordWithTimestamp(InstructionWaitForProofTx, now.Add(5170*time.Millisecond))

	totalDuration := timer.TotalDuration()
	require.Equal(t, 5170*time.Millisecond, totalDuration)

	durations := timer.GetDurations()
	require.Equal(t, 5000*time.Millisecond, durations[InstructionWaitForProofTx])
}

// TestInstructionTimer_LeaderElection tests leader election flow.
func TestInstructionTimer_LeaderElection(t *testing.T) {
	timer := NewInstructionTimer()
	now := time.Now()

	timer.RecordWithTimestamp(InstructionAcquireLock, now)
	timer.RecordWithTimestamp(InstructionRenewLock, now.Add(30*time.Second))
	timer.RecordWithTimestamp(InstructionReleaseLock, now.Add(60*time.Second))

	totalDuration := timer.TotalDuration()
	require.Equal(t, 60*time.Second, totalDuration)

	durations := timer.GetDurations()
	require.Equal(t, 30*time.Second, durations[InstructionRenewLock])
	require.Equal(t, 30*time.Second, durations[InstructionReleaseLock])
}

// TestInstructionTimer_ManyInstructions tests timer with many instructions.
func TestInstructionTimer_ManyInstructions(t *testing.T) {
	timer := NewInstructionTimer()
	now := time.Now()

	// Record 100 instructions with unique names
	for i := 0; i < 100; i++ {
		instruction := fmt.Sprintf("instruction_%d", i)
		timer.RecordWithTimestamp(instruction, now.Add(time.Duration(i)*time.Millisecond))
	}

	require.Len(t, timer.Timestamps, 100, "Should have 100 timestamps")

	durations := timer.GetDurations()
	require.Len(t, durations, 99, "Should have 99 durations")

	totalDuration := timer.TotalDuration()
	require.Equal(t, 99*time.Millisecond, totalDuration)
}

// TestInstructionTimer_ZeroDuration tests instructions with zero duration.
func TestInstructionTimer_ZeroDuration(t *testing.T) {
	timer := NewInstructionTimer()
	now := time.Now()

	// Record instructions at same timestamp
	timer.RecordWithTimestamp(InstructionRelayReceived, now)
	timer.RecordWithTimestamp(InstructionParseRelayRequest, now)
	timer.RecordWithTimestamp(InstructionValidateRelayRequest, now)

	durations := timer.GetDurations()
	require.Len(t, durations, 2)

	// All durations should be zero
	for _, duration := range durations {
		require.Equal(t, time.Duration(0), duration, "Duration should be 0")
	}

	totalDuration := timer.TotalDuration()
	require.Equal(t, time.Duration(0), totalDuration)
}

// TestInstructionTimer_RealTiming tests timer with real time delays.
func TestInstructionTimer_RealTiming(t *testing.T) {
	timer := NewInstructionTimer()

	timer.Record(InstructionRelayReceived)
	time.Sleep(5 * time.Millisecond)
	timer.Record(InstructionParseRelayRequest)
	time.Sleep(5 * time.Millisecond)
	timer.Record(InstructionValidateRelayRequest)

	durations := timer.GetDurations()
	require.Len(t, durations, 2)

	// Each duration should be at least 5ms
	for _, duration := range durations {
		require.Greater(t, duration, 4*time.Millisecond, "Duration should be at least 4ms")
		require.Less(t, duration, 20*time.Millisecond, "Duration should be less than 20ms")
	}

	totalDuration := timer.TotalDuration()
	require.Greater(t, totalDuration, 9*time.Millisecond)
	require.Less(t, totalDuration, 50*time.Millisecond)
}

// TestInstructionConstants tests that instruction constants are defined.
func TestInstructionConstants(t *testing.T) {
	// Relay instructions
	require.NotEmpty(t, InstructionRelayReceived)
	require.NotEmpty(t, InstructionParseRelayRequest)
	require.NotEmpty(t, InstructionValidateRelayRequest)
	require.NotEmpty(t, InstructionSignResponse)
	require.NotEmpty(t, InstructionPublishToRedis)

	// Miner instructions
	require.NotEmpty(t, InstructionConsumeFromStream)
	require.NotEmpty(t, InstructionUpdateSessionTree)
	require.NotEmpty(t, InstructionSubmitClaim)
	require.NotEmpty(t, InstructionSubmitProof)

	// Redis instructions
	require.NotEmpty(t, InstructionRedisXAdd)
	require.NotEmpty(t, InstructionRedisGet)
	require.NotEmpty(t, InstructionRedisSet)

	// Leader election instructions
	require.NotEmpty(t, InstructionAcquireLock)
	require.NotEmpty(t, InstructionRenewLock)
	require.NotEmpty(t, InstructionReleaseLock)
}
