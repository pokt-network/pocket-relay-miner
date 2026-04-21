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

func TestUpdateClaimOnChainOutcome_IncludedSuccess(t *testing.T) {
	tr, _ := setupSubmissionTracker(t)
	seedClaim(t, tr, "pokt1test", "sess-1", "txhash-abc")

	ctx := context.Background()
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier:        "pokt1test",
		TxHash:          "txhash-abc",
		Outcome:         "included_success",
		InclusionHeight: 127,
	}))

	got, err := tr.GetRecord(ctx, "pokt1test", 110, "sess-1")
	require.NoError(t, err)
	assert.Equal(t, "included_success", got.ClaimOnChainOutcome)
	assert.Equal(t, int64(127), got.ClaimInclusionHeight)
	assert.Empty(t, got.ClaimOnChainError)
	// Existing fields must be preserved.
	assert.True(t, got.ClaimSuccess)
	assert.Equal(t, "txhash-abc", got.ClaimTxHash)
}

func TestUpdateClaimOnChainOutcome_IncludedFailureCapturesRawLog(t *testing.T) {
	tr, _ := setupSubmissionTracker(t)
	seedClaim(t, tr, "pokt1test", "sess-2", "txhash-fail")

	ctx := context.Background()
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier:        "pokt1test",
		TxHash:          "txhash-fail",
		Outcome:         "included_failure",
		ErrMsg:          "invalid relay difficulty",
		InclusionHeight: 131,
	}))

	got, err := tr.GetRecord(ctx, "pokt1test", 110, "sess-2")
	require.NoError(t, err)
	assert.Equal(t, "included_failure", got.ClaimOnChainOutcome)
	assert.Equal(t, "invalid relay difficulty", got.ClaimOnChainError,
		"DeliverTx RawLog must be captured so operators can diagnose")
}

func TestUpdateClaimOnChainOutcome_MempoolTimeout(t *testing.T) {
	tr, _ := setupSubmissionTracker(t)
	seedClaim(t, tr, "pokt1test", "sess-3", "txhash-timeout")

	ctx := context.Background()
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier: "pokt1test",
		TxHash:   "txhash-timeout",
		Outcome:  "mempool_timeout",
	}))

	got, err := tr.GetRecord(ctx, "pokt1test", 110, "sess-3")
	require.NoError(t, err)
	assert.Equal(t, "mempool_timeout", got.ClaimOnChainOutcome)
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
		Outcome:         "included_success",
		InclusionHeight: 200,
	}))

	for _, id := range []string{"sess-a", "sess-b"} {
		got, err := tr.GetRecord(ctx, "pokt1test", 110, id)
		require.NoError(t, err)
		assert.Equal(t, "included_success", got.ClaimOnChainOutcome, id)
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
		Outcome:  "included_success",
	}))
}

func TestUpdateProofOnChainOutcome_Smoke(t *testing.T) {
	tr, _ := setupSubmissionTracker(t)
	ctx := context.Background()

	// Create a record with a proof tx hash.
	require.NoError(t, tr.TrackClaimSubmission(
		ctx, "pokt1test", "svc", "pokt1app", "sess-proof", 100, 110,
		"0xaa", "ctx", true, "", 105, 110, 50, 100, true, "",
	))
	require.NoError(t, tr.TrackProofSubmission(
		ctx, "pokt1test", 110, "sess-proof", "0xbb", "ptx", true, "", 120, 125, true, "",
	))

	require.NoError(t, tr.UpdateProofOnChainOutcome(ctx, ProofOnChainUpdate{
		Supplier:        "pokt1test",
		TxHash:          "ptx",
		Outcome:         "included_success",
		InclusionHeight: 130,
	}))

	got, err := tr.GetRecord(ctx, "pokt1test", 110, "sess-proof")
	require.NoError(t, err)
	assert.Equal(t, "included_success", got.ProofOnChainOutcome)
	assert.Equal(t, int64(130), got.ProofInclusionHeight)
}
