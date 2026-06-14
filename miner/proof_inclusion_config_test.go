//go:build test

package miner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Unset YAML fields → DefaultProofInclusionTrackerConfig values.
func TestProofInclusionTrackerConfig_Defaults(t *testing.T) {
	got := TransactionConfig{}.ProofInclusionTrackerConfig()
	def := DefaultProofInclusionTrackerConfig()

	require.False(t, got.Disabled)
	require.Equal(t, def.PollInterval, got.PollInterval)
	require.Equal(t, def.MaxConcurrent, got.MaxConcurrent)
	require.Equal(t, def.MaxPollDuration, got.MaxPollDuration)
	require.Equal(t, def.MaxRebroadcasts, got.MaxRebroadcasts)
	require.Equal(t, def.RebroadcastSafetyBlocks, got.RebroadcastSafetyBlocks)
	require.Equal(t, 2, got.MaxRebroadcasts, "default rebroadcasts")
}

// Set YAML fields → honored, including the pointer semantics for rebroadcasts.
func TestProofInclusionTrackerConfig_Overrides(t *testing.T) {
	zero := 0
	five := 5

	// Explicit 0 → observe-only (must be distinguishable from unset).
	observeOnly := TransactionConfig{ProofMaxRebroadcasts: &zero}.ProofInclusionTrackerConfig()
	require.Equal(t, 0, observeOnly.MaxRebroadcasts, "explicit 0 must disable rebroadcast, not fall back to default")

	c := TransactionConfig{
		DisableProofInclusionTracking:     true,
		ProofInclusionPollIntervalMs:      1500,
		ProofInclusionPollerMaxConcurrent: 8,
		ProofInclusionMaxPollDurationMs:   300000,
		ProofMaxRebroadcasts:              &five,
		ProofRebroadcastSafetyBlocks:      3,
	}.ProofInclusionTrackerConfig()

	require.True(t, c.Disabled)
	require.Equal(t, 1500*time.Millisecond, c.PollInterval)
	require.Equal(t, 8, c.MaxConcurrent)
	require.Equal(t, 300000*time.Millisecond, c.MaxPollDuration)
	require.Equal(t, 5, c.MaxRebroadcasts)
	require.Equal(t, int64(3), c.RebroadcastSafetyBlocks)
}
