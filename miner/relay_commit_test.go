//go:build test

package miner

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// The relayCommitter contract, run against the Redis committer. Every case
// calls CommitSession or AckRejected directly, not through relayBatch, so it
// pins what any committer must do and not what one flush path happens to need.

func newCommitterFixture(t *testing.T, supplier string) (*batchWorker, relayCommitter) {
	t.Helper()
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, supplier, "a")
	require.NotNil(t, w.batch.commit, "premise: a Redis deduplicator gets a committer")
	return w, w.batch.commit
}

func (w *batchWorker) createSession(sessionID string, state SessionState) {
	w.t.Helper()
	created, err := w.store.CreateIfAbsent(w.ctx, &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: w.supplier,
		ServiceID:               "svc-1",
		State:                   state,
	})
	require.NoError(w.t, err)
	require.True(w.t, created, "premise: the session is new")
}

// relaysFor pairs each published entry with its own hash and compute units:
// 100, 200, 300... so a counter that adds one relay's units for another, or a
// flat amount, gives a different total.
func relaysFor(ids []string, hashes ...string) []batchedRelay {
	relays := make([]batchedRelay, len(ids))
	for i, id := range ids {
		relays[i] = batchedRelay{id: id, hash: []byte(hashes[i]), computeUnits: uint64(100 * (i + 1))}
	}
	return relays
}

func TestRelayCommitter_CountsMarksAndAcksEveryNewRelay(t *testing.T) {
	const sessionID = "sess-commit-counts"
	w, commit := newCommitterFixture(t, "pokt1commit_counts")
	w.createSession(sessionID, SessionStateActive)
	relays := relaysFor(w.publish(3), "h-0", "h-1", "h-2")

	res, err := commit.CommitSession(w.ctx, sessionID, relays)

	require.NoError(t, err)
	require.Equal(t, relayBatchResult{status: 0, newRelays: 3, newComputeUnits: 600, freshDups: 0}, res)
	require.Equal(t, int64(3), w.marked(sessionID), "every hash marked")
	require.Zero(t, w.pending(), "every entry acknowledged")
	require.Zero(t, w.streamLen(), "and removed")
	snap := w.snapshot(sessionID)
	require.Equal(t, int64(3), snap.RelayCount)
	require.Equal(t, uint64(600), snap.TotalComputeUnits, "each relay adds its own compute units")
}

func TestRelayCommitter_ARerunAfterALostAnswerChangesNothing(t *testing.T) {
	const sessionID = "sess-commit-rerun"
	w, commit := newCommitterFixture(t, "pokt1commit_rerun")
	w.createSession(sessionID, SessionStateActive)
	relays := relaysFor(w.publish(2), "h-0", "h-1")
	_, err := commit.CommitSession(w.ctx, sessionID, relays)
	require.NoError(t, err, "premise: the first run succeeds, its answer is what gets lost")

	res, err := commit.CommitSession(w.ctx, sessionID, relays)

	require.NoError(t, err)
	require.Equal(t, relayBatchResult{status: 0}, res, "nothing new, nothing counted, no duplicate: its own entries are gone")
	snap := w.snapshot(sessionID)
	require.Equal(t, int64(2), snap.RelayCount, "counted once")
	require.Equal(t, uint64(300), snap.TotalComputeUnits)
	require.Equal(t, int64(2), w.marked(sessionID))
}

func TestRelayCommitter_ARelayMarkedElsewhereIsAFreshDuplicateNotCounted(t *testing.T) {
	const sessionID = "sess-commit-dup"
	w, commit := newCommitterFixture(t, "pokt1commit_dup")
	w.createSession(sessionID, SessionStateActive)
	relays := relaysFor(w.publish(2), "h-new", "h-finished")
	require.NoError(t, w.client.SAdd(w.ctx, w.dedup.sessionKey(sessionID), hashMember([]byte("h-finished"))).Err(),
		"premise: another consumer finished the second relay; its entry is still pending here")

	res, err := commit.CommitSession(w.ctx, sessionID, relays)

	require.NoError(t, err)
	require.Equal(t, relayBatchResult{status: 0, newRelays: 1, newComputeUnits: 100, freshDups: 1}, res)
	require.Equal(t, int64(1), w.snapshot(sessionID).RelayCount, "only the new relay is counted")
	require.Zero(t, w.pending(), "the duplicate's entry is acknowledged too")
}

func TestRelayCommitter_AMissingOrTerminalSessionIsMarkedAndAckedNotCounted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  SessionState // "" = no session
		status int64
	}{
		{name: "missing", status: 1},
		{name: "terminal", state: SessionStateProved, status: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := "sess-commit-" + tc.name
			w, commit := newCommitterFixture(t, "pokt1commit_"+tc.name)
			if tc.state != "" {
				w.createSession(sessionID, tc.state)
			}
			relays := relaysFor(w.publish(2), "h-0", "h-1")

			res, err := commit.CommitSession(w.ctx, sessionID, relays)

			require.NoError(t, err)
			require.Equal(t, relayBatchResult{status: tc.status, newRelays: 2, newComputeUnits: 300}, res)
			require.Equal(t, int64(2), w.marked(sessionID), "marked, so a redelivery is not counted later")
			require.Zero(t, w.pending(), "acknowledged")
			if tc.state != "" {
				require.Zero(t, w.snapshot(sessionID).RelayCount, "a terminal session's counters do not move")
			}
		})
	}
}

// A refusal is reported as errCommitRefused, and leaves nothing written: a mark
// with no count and no acknowledgement would be a permanent under-count.
func TestRelayCommitter_ARefusalWritesNothing(t *testing.T) {
	const sessionID = "sess-commit-refused"
	w, commit := newCommitterFixture(t, "pokt1commit_refused")
	w.createSession(sessionID, SessionStateActive)
	relays := relaysFor(w.publish(2), "h-0", "h-1")
	notAStream := w.stream + ":not-a-stream"
	require.NoError(t, w.client.Set(w.ctx, notAStream, "x", 0).Err())
	redisCommit, ok := commit.(*redisRelayCommitter)
	require.True(t, ok, "premise: the Redis committer")
	refusing := *redisCommit
	refusing.streamName = notAStream

	_, err := refusing.CommitSession(w.ctx, sessionID, relays)

	require.ErrorIs(t, err, errCommitRefused)
	require.Zero(t, w.marked(sessionID), "nothing marked")
	require.Zero(t, w.snapshot(sessionID).RelayCount, "nothing counted")
	require.Equal(t, int64(2), w.pending(), "nothing acknowledged")
}

// An error raised by the server after the commit started writing is "outcome
// unknown": it must NOT wrap errCommitRefused, or the batch would finish the
// relays one at a time over a half-applied commit instead of retrying it.
//
// The failure is a counter that is not an integer, so HINCRBY fails after the
// SADD. What the half-applied run leaves -- marks written, nothing counted,
// entries still pending -- is asserted too: a retry then finds the marks and
// counts nothing. That under-count predates this test (the script's "nothing
// after the writes refuses" does not hold for HINCRBY); it is pinned here so a
// change to it is seen, not because it is right.
func TestRelayCommitter_AFailureAfterTheFirstWriteIsNotARefusal(t *testing.T) {
	const sessionID = "sess-commit-unknown"
	w, commit := newCommitterFixture(t, "pokt1commit_unknown")
	w.createSession(sessionID, SessionStateActive)
	require.NoError(t, w.client.HSet(w.ctx, w.store.sessionKey(sessionID), "relay_count", "not-a-number").Err())
	relays := relaysFor(w.publish(2), "h-0", "h-1")

	_, err := commit.CommitSession(w.ctx, sessionID, relays)

	require.Error(t, err)
	require.False(t, errors.Is(err, errCommitRefused), "a failure after a write is retried, not refused: %v", err)
	require.Equal(t, int64(2), w.marked(sessionID), "the SADD before the failure stayed written")
	require.Equal(t, int64(2), w.pending(), "nothing acknowledged: the entries come back")
}

func TestRelayCommitter_AckRejectedAcksWithoutMarkingOrCounting(t *testing.T) {
	const sessionID = "sess-commit-rejected"
	w, commit := newCommitterFixture(t, "pokt1commit_rejected")
	w.createSession(sessionID, SessionStateActive)
	ids := w.publish(3)

	require.NoError(t, commit.AckRejected(w.ctx, ids))

	require.Zero(t, w.pending())
	require.Zero(t, w.streamLen())
	require.Zero(t, w.marked(sessionID))
	require.Zero(t, w.snapshot(sessionID).RelayCount)
}

// notRedisDedup is a Deduplicator that is not the Redis one.
type notRedisDedup struct{ Deduplicator }

// Without a committer the supplier finishes every relay on its own. The nil
// must be an untyped one: a typed nil would pass the batch's nil check and
// panic on every use -- inside a flush, which its recover turns into the
// per-relay path, and in AckRejected, which no flush recover covers.
func TestRelayCommitter_NoneForANonRedisDeduplicatorMeansNoBatch(t *testing.T) {
	w, _ := newCommitterFixture(t, "pokt1commit_none")

	commit := newRedisRelayCommitter(w.client, w.store, notRedisDedup{}, w.consumer)

	require.True(t, commit == nil, "an untyped nil, got %#v", commit)
	require.Nil(t, newRelayBatch(w.batch.logger, w.supplier, notRedisDedup{}, w.smst, w.batch.coordinator, w.consumer, commit))
}
