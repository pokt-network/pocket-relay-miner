//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A claim that was resent must say so in the ledger the operator reads.
//
// The counter and its metric (claim_rebroadcasts_total) already existed; what
// did not was persisting it, so `redis submissions` reported zero resends for a
// claim that had been resent and the ledger disagreed with the metric.
//
// THIS TEST GOES THROUGH THE PRODUCTION CLOSURE ON PURPOSE. The defect was never
// in the tracker: UpdateClaimOnChainOutcome would have copied the field the
// moment anyone passed it. It was in the caller, which had the entry in hand and
// did not read it. A test that called the tracker directly with Rebroadcasts: 2
// would pass against the defect untouched -- it would asserts that the tracker
// copies what it is given, which was never in doubt. So the call below is
// claimPhase.recordOutcome, the closure ensureSharedTrackers actually wires.
func TestRecordOutcome_ClaimResendsReachTheLedger(t *testing.T) {
	rc, _ := newTestRedis(t)

	m := &SupplierManager{
		logger: zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.TraceLevel),
		config: SupplierManagerConfig{
			RedisClient:           rc,
			ProofQueryClient:      inclusionProbe{},
			BlockClient:           &mockBlockClient{},
			SubmissionTrackingTTL: time.Hour,
		},
	}
	m.ensureSharedTrackers()
	t.Cleanup(func() {
		if m.reconcilerCancel != nil {
			m.reconcilerCancel()
		}
		m.reconcilerWG.Wait()
	})
	require.NotNil(t, m.inclusionReconciler, "the reconciler must exist, or the phases were never built")
	require.NotNil(t, m.sharedSubmissionTracker, "without the tracker there is no ledger to assert on")

	ctx := context.Background()
	seedClaim(t, m.sharedSubmissionTracker, "pokt1test", "sess-1", "txhash-abc")

	// Two resends, and the ORIGINAL hash: a rebroadcast changes the latest hash
	// while the record stays keyed by the first one, which is why the closure
	// looks the record up by OrigTxHash.
	err := m.inclusionReconciler.claimPhase.recordOutcome(
		ctx,
		rebroadcastEntry{TxHash: "txhash-resent", OrigTxHash: "txhash-abc", Rebroadcasts: 2},
		"pokt1test", 110, "sess-1", inclusionMissing, 0)
	require.NoError(t, err)

	got, err := m.sharedSubmissionTracker.GetRecord(ctx, "pokt1test", 110, "sess-1")
	require.NoError(t, err)
	require.Equal(t, 2, got.ClaimRebroadcasts,
		"the claim was resent twice and the ledger says %d: an operator reading `redis submissions` concludes it was never resent", got.ClaimRebroadcasts)

	// The outcome itself must still land -- a test that only watched the new
	// field would pass with the rest of the update broken.
	require.Equal(t, inclusionMissing, got.ClaimOnChainOutcome)
}

// A claim that was NEVER resent must not report resends. Without this, writing
// the field unconditionally -- or writing a constant -- passes the test above.
func TestRecordOutcome_AClaimNeverResentReportsNoResends(t *testing.T) {
	rc, _ := newTestRedis(t)

	m := &SupplierManager{
		logger: zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.TraceLevel),
		config: SupplierManagerConfig{
			RedisClient:           rc,
			ProofQueryClient:      inclusionProbe{},
			BlockClient:           &mockBlockClient{},
			SubmissionTrackingTTL: time.Hour,
		},
	}
	m.ensureSharedTrackers()
	t.Cleanup(func() {
		if m.reconcilerCancel != nil {
			m.reconcilerCancel()
		}
		m.reconcilerWG.Wait()
	})

	ctx := context.Background()
	seedClaim(t, m.sharedSubmissionTracker, "pokt1test", "sess-2", "txhash-clean")

	err := m.inclusionReconciler.claimPhase.recordOutcome(
		ctx,
		rebroadcastEntry{TxHash: "txhash-clean", OrigTxHash: "txhash-clean"},
		"pokt1test", 110, "sess-2", inclusionMissing, 0)
	require.NoError(t, err)

	got, err := m.sharedSubmissionTracker.GetRecord(ctx, "pokt1test", 110, "sess-2")
	require.NoError(t, err)
	require.Zero(t, got.ClaimRebroadcasts, "nothing was resent, so the ledger must not claim otherwise")
}
