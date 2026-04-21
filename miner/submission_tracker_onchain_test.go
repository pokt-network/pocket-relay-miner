//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

func setupSubmissionTracker(t *testing.T) (*SubmissionTracker, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{URL: fmt.Sprintf("redis://%s", mr.Addr())})
	require.NoError(t, err)

	return NewSubmissionTracker(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		client,
		1*time.Hour,
	), mr
}

// seedClaim creates a tracking record representing a successful claim broadcast
// (ClaimSuccess=true, no on-chain outcome yet) — the shape WS-B is designed to
// update.
func seedClaim(t *testing.T, tr *SubmissionTracker, supplier, sessionID, txHash string) {
	t.Helper()
	err := tr.TrackClaimSubmission(
		context.Background(),
		supplier,
		"svc-a",
		"pokt1app",
		sessionID,
		100,    // session start
		110,    // session end
		"0xaa", // claim hash
		txHash,
		true,
		"",
		105, // submit height
		110, // current height
		50,  // relays
		100, // cu
		false,
		"",
	)
	require.NoError(t, err)
}

func TestUpdateClaimOnChainOutcome_OnChainFound(t *testing.T) {
	tr, _ := setupSubmissionTracker(t)
	seedClaim(t, tr, "pokt1test", "sess-1", "txhash-abc")

	ctx := context.Background()
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier:        "pokt1test",
		TxHash:          "txhash-abc",
		Outcome:         "on_chain_found",
		InclusionHeight: 127,
	}))

	got, err := tr.GetRecord(ctx, "pokt1test", 110, "sess-1")
	require.NoError(t, err)
	assert.Equal(t, "on_chain_found", got.ClaimOnChainOutcome)
	assert.Equal(t, int64(127), got.ClaimInclusionHeight)
	// Existing fields must be preserved.
	assert.True(t, got.ClaimSuccess)
	assert.Equal(t, "txhash-abc", got.ClaimTxHash)
}

func TestUpdateClaimOnChainOutcome_OnChainMissing(t *testing.T) {
	// The claim window closed without the claim landing — this is the
	// "lost reward" signal operators need to count.
	tr, _ := setupSubmissionTracker(t)
	seedClaim(t, tr, "pokt1test", "sess-3", "txhash-missing")

	ctx := context.Background()
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier: "pokt1test",
		TxHash:   "txhash-missing",
		Outcome:  "on_chain_missing",
	}))

	got, err := tr.GetRecord(ctx, "pokt1test", 110, "sess-3")
	require.NoError(t, err)
	assert.Equal(t, "on_chain_missing", got.ClaimOnChainOutcome)
	assert.Equal(t, int64(0), got.ClaimInclusionHeight)
}

func TestUpdateClaimOnChainOutcome_MultipleSessionsSameTxHash(t *testing.T) {
	// Claims are batched: a single tx can cover multiple sessions. All their
	// records must be updated together.
	tr, _ := setupSubmissionTracker(t)
	seedClaim(t, tr, "pokt1test", "sess-a", "txhash-batch")
	seedClaim(t, tr, "pokt1test", "sess-b", "txhash-batch")
	seedClaim(t, tr, "pokt1test", "sess-c", "txhash-other") // must NOT be updated

	ctx := context.Background()
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier:        "pokt1test",
		TxHash:          "txhash-batch",
		Outcome:         "on_chain_found",
		InclusionHeight: 200,
	}))

	for _, id := range []string{"sess-a", "sess-b"} {
		got, err := tr.GetRecord(ctx, "pokt1test", 110, id)
		require.NoError(t, err)
		assert.Equal(t, "on_chain_found", got.ClaimOnChainOutcome, id)
	}

	// Unrelated session untouched.
	got, err := tr.GetRecord(ctx, "pokt1test", 110, "sess-c")
	require.NoError(t, err)
	assert.Empty(t, got.ClaimOnChainOutcome,
		"session with different tx_hash must not be updated")
}

func TestUpdateClaimOnChainOutcome_EmptyTxHashIsNoop(t *testing.T) {
	tr, _ := setupSubmissionTracker(t)
	ctx := context.Background()
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier: "pokt1test",
		TxHash:   "", // empty — must skip silently
		Outcome:  "on_chain_found",
	}))
}
