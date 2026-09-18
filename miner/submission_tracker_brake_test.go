//go:build test

package miner

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// The submission record is the only ledger of what this miner sent. It is
// written after the claim or the proof is already broadcast, so it records a
// fact and never takes new work, and the memory reserve that closes the store
// exists precisely so that the writes around a claim still fit.
//
// The store closing must therefore not stop it. It used to: every write asked
// the ingestion gate -- "can I read new relays?" -- to decide whether to record
// that a claim had been sent. A run that closed the store left 250 claims with
// no record, and the proof path later created records that said those claims
// had failed. The control that identifies the cause is the rest of that run:
// three other sessions of the same 250 suppliers, outside the closed window,
// have complete claim records.
//
// That run closed the store with 1022 MiB free against a 1024 MiB reserve, so
// it was 2 MiB from the line, not near an OOM. The tests below close it much
// harder on purpose; nothing here needs the store to be nearly full.

// closeStoreByReserve leaves Redis with less free memory than the reserve, so
// every gate closes while Redis still accepts writes -- the shape of the
// measured incident. It returns a health whose gates are shut.
func closeStoreByReserve(t *testing.T, client *redistransport.Client, prefix string) *redistransport.StoreHealth {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, client.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err())

	// The reserve is 1 GiB or an eighth of maxmemory, whichever is smaller
	// (storeCloseBelow), so on a test Redis the free memory has to be under an
	// eighth of the limit -- and the limit is set relative to what Redis holds
	// right now. This Redis is shared with whatever else is running, so a
	// filler makes the window under the line proportionally large: without it
	// the window is a sixteenth of a nearly empty Redis, a couple of hundred
	// kilobytes, and a peer writing more than that while this test runs leaves
	// Redis refusing the very write the test is about.
	filler := prefix + ":reserve-filler"
	require.NoError(t, client.Set(ctx, filler, make([]byte, 32<<20), time.Minute).Err())
	t.Cleanup(func() { _ = client.Del(context.Background(), filler).Err() })

	info, err := client.InfoMap(ctx, "memory").Result()
	require.NoError(t, err)
	used, err := strconv.ParseUint(info["Memory"]["used_memory"], 10, 64)
	require.NoError(t, err)
	// A sixteenth of the limit is under the eighth that closes the gates, and
	// with the filler above it is megabytes, not kilobytes.
	free := used / 16
	require.NoError(t, client.ConfigSet(ctx, "maxmemory", strconv.FormatUint(used+free, 10)).Err())
	t.Cleanup(func() { _ = client.ConfigSet(context.Background(), "maxmemory", "0").Err() })

	health := redistransport.NewStoreHealth(zerolog.Nop(), client.UniversalClient, "test_tracker", redistransport.StoreGateIngestion)
	// The preflight refuses a store with no limit or an evicting policy; this
	// one has both set, so it must accept it and close the gates on the sample.
	require.NoError(t, health.Start(ctx))
	require.False(t, health.Operable(),
		"CONTROL: the gate must be closed, or this test proves nothing about writing while it is")
	return health
}

// TestSubmissionTracker_WritesEveryKindWhileTheStoreIsClosed: with the store
// closed by the memory reserve, each of the four record kinds is still written.
func TestSubmissionTracker_WritesEveryKindWhileTheStoreIsClosed(t *testing.T) {
	ctx := context.Background()
	client, prefix := newTestRedis(t)
	closeStoreByReserve(t, client, prefix)

	tr := NewSubmissionTracker(zerolog.Nop(), client, time.Hour)
	const supplier = "pokt1track"
	const sessionID = "sess-closed"
	const txHash = "0xtx"

	seedClaim(t, tr, supplier, sessionID, txHash)
	require.True(t, keyExists(t, client, client.KB().TxTrackKey(supplier, 110, sessionID)),
		"LINK tracker-writes-closed: the claim record is written while the store is closed")

	record, err := tr.GetRecord(ctx, supplier, 110, sessionID)
	require.NoError(t, err)
	require.Equal(t, txHash, record.ClaimTxHash, "and it holds the claim it was asked to record")

	require.NoError(t, tr.TrackProofSubmission(ctx, supplier, 110, sessionID,
		"0xproof", "0xprooftx", true, "", 105, 110, true, ""))
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier: supplier, TxHash: txHash, Outcome: "on_chain_found", InclusionHeight: 111,
	}))
	require.NoError(t, tr.UpdateProofOnChainOutcome(ctx, ProofOnChainUpdate{
		Supplier: supplier, SessionEnd: 110, SessionID: sessionID,
		Outcome: "on_chain_found", InclusionHeight: 112,
	}))

	record, err = tr.GetRecord(ctx, supplier, 110, sessionID)
	require.NoError(t, err)
	require.Equal(t, "0xprooftx", record.ProofTxHash, "LINK tracker-writes-closed: the proof record too")
	require.Equal(t, "on_chain_found", record.ClaimOnChainOutcome, "and the claim outcome")
	require.Equal(t, int64(111), record.ClaimInclusionHeight)
	require.Equal(t, "on_chain_found", record.ProofOnChainOutcome, "and the proof outcome")
	require.Equal(t, int64(112), record.ProofInclusionHeight)
}

// TestSubmissionTracker_AWriteRefusedByRedisReachesTheCallerAndIsCounted: when
// Redis itself refuses the write, that is the one answer that cannot be wrong.
// The error must reach the caller -- which logs it -- and be counted apart from
// any other failure.
func TestSubmissionTracker_AWriteRefusedByRedisReachesTheCallerAndIsCounted(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	tr := NewSubmissionTracker(zerolog.Nop(), client, time.Hour)
	const supplier = "pokt1refused"

	require.NoError(t, client.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err())
	info, err := client.InfoMap(ctx, "memory").Result()
	require.NoError(t, err)
	used, err := strconv.ParseUint(info["Memory"]["used_memory"], 10, 64)
	require.NoError(t, err)
	oomFailed := testutil.ToFloat64(trackingWritesFailed.WithLabelValues("claim", "oom"))
	otherFailed := testutil.ToFloat64(trackingWritesFailed.WithLabelValues("claim", "other"))
	require.NoError(t, client.ConfigSet(ctx, "maxmemory", strconv.FormatUint(used, 10)).Err())
	t.Cleanup(func() { _ = client.ConfigSet(context.Background(), "maxmemory", "0").Err() })

	err = tr.TrackClaimSubmission(ctx, supplier, "svc-a", "pokt1app", "sess-oom",
		100, 110, "0xaa", "0xtx", true, "", 105, 110, 50, 100, false, "")
	require.Error(t, err, "LINK tracker-write-failed: a refused write is reported to the caller")

	require.Equal(t, oomFailed+1, testutil.ToFloat64(trackingWritesFailed.WithLabelValues("claim", "oom")),
		"LINK tracker-write-failed: the refusal is counted as what it is, an OOM")
	require.Equal(t, otherFailed, testutil.ToFloat64(trackingWritesFailed.WithLabelValues("claim", "other")),
		"and not as some other failure")
}

// TestSubmissionTracker_AProofWithNoClaimSaysItDoesNotKnow: a record the proof
// path creates on its own has never been told anything about the claim, so it
// must not report that the claim failed -- which is what a zero bool reports.
func TestSubmissionTracker_AProofWithNoClaimSaysItDoesNotKnow(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	tr := NewSubmissionTracker(zerolog.Nop(), client, time.Hour)
	const supplier = "pokt1unknown"

	require.NoError(t, tr.TrackProofSubmission(ctx, supplier, 110, "sess-proof-first",
		"0xproof", "0xprooftx", true, "", 105, 110, true, ""))

	record, err := tr.GetRecord(ctx, supplier, 110, "sess-proof-first")
	require.NoError(t, err)
	require.Equal(t, "", record.ClaimBroadcastOutcome,
		"LINK claim-unknown: no claim was recorded, so the outcome is unknown, not failed")

	// A claim that was recorded says so in both fields, so a binary that only
	// knows the bool still reads it right.
	seedClaim(t, tr, supplier, "sess-claim-first", "0xtx")
	record, err = tr.GetRecord(ctx, supplier, 110, "sess-claim-first")
	require.NoError(t, err)
	require.Equal(t, ClaimBroadcastAccepted, record.ClaimBroadcastOutcome)
	require.True(t, record.ClaimSuccess, "LINK claim-mirror: the bool mirrors a known outcome for older readers")

	require.NoError(t, tr.TrackClaimSubmission(ctx, supplier, "svc-a", "pokt1app", "sess-rejected",
		100, 110, "0xaa", "", false, "mempool full", 105, 110, 50, 100, false, ""))
	record, err = tr.GetRecord(ctx, supplier, 110, "sess-rejected")
	require.NoError(t, err)
	require.Equal(t, ClaimBroadcastRejected, record.ClaimBroadcastOutcome)
	require.False(t, record.ClaimSuccess, "LINK claim-mirror: and a rejection too")
}

// TestSubmissionTracker_AnOutcomeWithNoRecordIsCounted: the reconciler resolves
// a tx once and never polls it again, so an outcome that finds no record to
// annotate is lost for good. It cannot be a Debug line nobody counts.
func TestSubmissionTracker_AnOutcomeWithNoRecordIsCounted(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	tr := NewSubmissionTracker(zerolog.Nop(), client, time.Hour)
	const supplier = "pokt1orphan"

	seedClaim(t, tr, supplier, "sess-a", "0xtx-a")
	missed := testutil.ToFloat64(trackingOutcomesWithoutRecord.WithLabelValues("claim"))

	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier: supplier, TxHash: "0xtx-nobody", Outcome: "on_chain_found", InclusionHeight: 111,
	}))
	require.Equal(t, missed+1, testutil.ToFloat64(trackingOutcomesWithoutRecord.WithLabelValues("claim")),
		"LINK outcome-without-record: an outcome that annotated nothing is counted")

	// Control: the outcome that does find its record is not counted as missing.
	require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
		Supplier: supplier, TxHash: "0xtx-a", Outcome: "on_chain_found", InclusionHeight: 111,
	}))
	require.Equal(t, missed+1, testutil.ToFloat64(trackingOutcomesWithoutRecord.WithLabelValues("claim")))
}
