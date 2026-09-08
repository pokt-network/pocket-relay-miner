//go:build test

package miner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
)

// The big ProofQueryClient interface is embedded as nil on purpose: only the two
// methods the reconciler needs are defined, so any OTHER method this path
// reached would panic naming itself instead of quietly returning a zero value.
type inclusionProbe struct {
	pocktclient.ProofQueryClient
}

func (inclusionProbe) GetSupplierClaimSessions(_ context.Context, _ string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (inclusionProbe) GetSupplierProvenSessions(_ context.Context, _ string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// The submission tracker's error does not rise: recordClaimOutcome returns nil
// after updating it, so no caller can see the failure and none can act on it.
// Discarding it therefore made the loss total -- the chain remembers the claim,
// our ledger simply never learned its outcome, and nothing said so.
//
// The assertion is on the log because the log is the only record that exists on
// this path by construction. Reaching the closure directly is deliberate: it is
// what reconcilePhase.recordOutcome is wired to, so this exercises the real one
// rather than a stand-in.
func TestRecordOutcome_TrackerFailureIsReported(t *testing.T) {
	rc, _ := newTestRedis(t)
	buf := &syncBuf{}

	m := &SupplierManager{
		logger: zerolog.New(buf).Level(zerolog.TraceLevel),
		config: SupplierManagerConfig{
			RedisClient:           rc,
			ProofQueryClient:      inclusionProbe{},
			BlockClient:           &mockBlockClient{},
			SubmissionTrackingTTL: time.Hour,
		},
	}
	// A block client is REQUIRED for the reconciler to be built at all: without
	// a per-block trigger it would persist rebroadcast entries nothing ever
	// reads, so the manager declines to build it. This one delivers no blocks;
	// the loop it starts is stopped by the cleanup below.
	m.ensureSharedTrackers()
	t.Cleanup(func() {
		if m.reconcilerCancel != nil {
			m.reconcilerCancel()
		}
		m.reconcilerWG.Wait()
	})
	require.NotNil(t, m.inclusionReconciler, "the reconciler must exist, or the phases were never built")

	// Take Redis away so the tracker update is the thing that fails.
	require.NoError(t, rc.Close())

	err := m.inclusionReconciler.claimPhase.recordOutcome(
		context.Background(), rebroadcastEntry{TxHash: "hash-1", OrigTxHash: "hash-1"},
		"supplier-1", 100, "session-1", inclusionMissing, 0)
	require.NoError(t, err, "the closure absorbs the tracker error by design; that is WHY it has to log it")

	require.True(t, strings.Contains(buf.String(), "not recorded in the submission tracker"),
		"a submission outcome that never reached the tracker must say so: no caller sees this error, so the log is the only record; got: %s", buf.String())
}

// The proof twin, and it exists because its absence was found by injection: with
// only the claim test above, disabling the Warn on the proof side left every
// test green. Two sites written together drift apart the moment only one of them
// is held, which is the shape this whole commit is about -- introducing it here
// would have been the sixth instance of it today.
func TestRecordOutcome_TrackerFailureIsReportedOnTheProofSideToo(t *testing.T) {
	rc, _ := newTestRedis(t)
	buf := &syncBuf{}

	m := &SupplierManager{
		logger: zerolog.New(buf).Level(zerolog.TraceLevel),
		config: SupplierManagerConfig{
			RedisClient:           rc,
			ProofQueryClient:      inclusionProbe{},
			BlockClient:           &mockBlockClient{},
			SubmissionTrackingTTL: time.Hour,
		},
	}
	// A block client is REQUIRED for the reconciler to be built at all: without
	// a per-block trigger it would persist rebroadcast entries nothing ever
	// reads, so the manager declines to build it. This one delivers no blocks;
	// the loop it starts is stopped by the cleanup below.
	m.ensureSharedTrackers()
	t.Cleanup(func() {
		if m.reconcilerCancel != nil {
			m.reconcilerCancel()
		}
		m.reconcilerWG.Wait()
	})
	require.NotNil(t, m.inclusionReconciler, "the reconciler must exist, or the phases were never built")

	require.NoError(t, rc.Close())

	err := m.inclusionReconciler.proofPhase.recordOutcome(
		context.Background(), rebroadcastEntry{TxHash: "hash-1", OrigTxHash: "hash-1"},
		"supplier-1", 100, "session-1", inclusionMissing, 0)
	require.NoError(t, err, "recordProofOutcome has no error path at all; the log is the ONLY record")

	require.True(t, strings.Contains(buf.String(), "proof on-chain outcome observed but not recorded"),
		"the proof side must report its own tracker failure, not rely on the claim side being tested; got: %s", buf.String())
}

// TestEnsureSharedTrackers_NoBlockClientBuildsNothing is the assertion whose
// absence let a SIGSEGV reach a gate. Making Subscribe a compile-time
// requirement removed a runtime type-assert that had been doing TWO jobs: it
// asked whether the client could subscribe, and — because a type-assert on a
// nil interface returns ok=false — it also asked whether there WAS one. The
// type covers the first and cannot cover the second, so the loop dereferenced
// nil one line in and took the whole test binary down: no test was marked
// failed, the package simply died.
//
// The assertion that carries the reason is the one on the STORE. Declining to
// build the reconciler is not about avoiding a panic — it is that a store
// without a per-block loop keeps persisting rebroadcast entries nothing will
// ever read, which age out at their TTL. That is the state this whole change
// set exists to make impossible, and a nil block client reaches it by a
// different door than the missing-capability one it closed.
func TestEnsureSharedTrackers_NoBlockClientBuildsNothing(t *testing.T) {
	rc, _ := newTestRedis(t)

	m := &SupplierManager{
		logger: zerolog.New(&syncBuf{}).Level(zerolog.TraceLevel),
		config: SupplierManagerConfig{
			RedisClient:           rc,
			ProofQueryClient:      inclusionProbe{},
			SubmissionTrackingTTL: time.Hour,
		},
	}

	// Called bare, and NOT wrapped in require.NotPanics, which could not see
	// this failure anyway: the dereference happens inside the goroutine
	// startReconcilerBlockLoop spawns, and no recover on this goroutine reaches
	// it. The failure signal is the binary dying -- which is precisely why the
	// assertions below are about state and not about panicking.
	m.ensureSharedTrackers()

	require.Nil(t, m.inclusionReconciler,
		"without a per-block trigger the reconciler cannot verify or rebroadcast anything")
	require.Nil(t, m.rebroadcastStore,
		"a live store with no loop to read it writes entries that only expire: leaving it nil is what keeps those writes from happening at all")
	require.NotNil(t, m.sharedSubmissionTracker,
		"the submission tracker is independent of the reconciler and must still be built")
}
