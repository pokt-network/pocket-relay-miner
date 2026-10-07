//go:build test

package miner

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// The relayCommitter contract, run against every committer: Redis and the
// embedded store. Every case calls CommitSession or AckRejected directly, not
// through relayBatch, so it pins what any committer must do and not what one
// flush path happens to need.

// commitHarness is one backend under the contract.
type commitHarness interface {
	commit() relayCommitter
	createSession(sessionID string, state SessionState)
	// publish queues n entries and returns their IDs in order.
	publish(n int) []string
	// marked counts the session's dedup marks.
	marked(sessionID string) int64
	// markDone marks a relay as finished by someone else.
	markDone(sessionID string, hash []byte)
	// queued counts the entries still in the supplier's queue.
	queued() int64
	snapshot(sessionID string) *SessionSnapshot
	// refusing returns a committer that refuses the session's batch before
	// writing anything.
	refusing(sessionID string) relayCommitter
}

// eachCommitter runs body once per backend.
func eachCommitter(t *testing.T, supplier string, body func(t *testing.T, h commitHarness)) {
	t.Run("redis", func(t *testing.T) { body(t, newRedisCommitHarness(t, supplier)) })
	t.Run("pebble", func(t *testing.T) { body(t, newPebbleCommitHarness(t, supplier)) })
}

type redisCommitHarness struct{ w *batchWorker }

func newRedisCommitHarness(t testing.TB, supplier string) *redisCommitHarness {
	t.Helper()
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, supplier, "a")
	require.NotNil(t, w.batch.commit, "premise: a Redis deduplicator gets a committer")
	return &redisCommitHarness{w: w}
}

func (h *redisCommitHarness) commit() relayCommitter { return h.w.batch.commit }
func (h *redisCommitHarness) createSession(id string, state SessionState) {
	h.w.createSession(id, state)
}
func (h *redisCommitHarness) publish(n int) []string              { return h.w.publish(n) }
func (h *redisCommitHarness) marked(id string) int64              { return h.w.marked(id) }
func (h *redisCommitHarness) snapshot(id string) *SessionSnapshot { return h.w.snapshot(id) }
func (h *redisCommitHarness) queued() int64 {
	h.w.t.Helper()
	require.Equal(h.w.t, h.w.streamLen(), h.w.pending(), "premise: every queued entry was delivered")
	return h.w.streamLen()
}
func (h *redisCommitHarness) markDone(id string, hash []byte) {
	require.NoError(h.w.t, h.w.client.SAdd(h.w.ctx, h.w.dedup.sessionKey(id), hashMember(hash)).Err())
}
func (h *redisCommitHarness) refusing(string) relayCommitter {
	// The stream key holds a string: the script refuses before its first write.
	notAStream := h.w.stream + ":not-a-stream"
	require.NoError(h.w.t, h.w.client.Set(h.w.ctx, notAStream, "x", 0).Err())
	redisCommit, ok := h.w.batch.commit.(*redisRelayCommitter)
	require.True(h.w.t, ok, "premise: the Redis committer")
	refusing := *redisCommit
	refusing.streamName = notAStream
	return &refusing
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
	eachCommitter(t, "pokt1commit_counts", func(t *testing.T, h commitHarness) {
		const sessionID = "sess-commit-counts"
		h.createSession(sessionID, SessionStateActive)
		relays := relaysFor(h.publish(3), "h-0", "h-1", "h-2")

		res, err := h.commit().CommitSession(context.Background(), sessionID, relays)

		require.NoError(t, err)
		require.Equal(t, relayBatchResult{status: 0, newRelays: 3, newComputeUnits: 600, freshDups: 0}, res)
		require.Equal(t, int64(3), h.marked(sessionID), "every hash marked")
		require.Zero(t, h.queued(), "every entry acknowledged and removed")
		snap := h.snapshot(sessionID)
		require.Equal(t, int64(3), snap.RelayCount)
		require.Equal(t, uint64(600), snap.TotalComputeUnits, "each relay adds its own compute units")
	})
}

func TestRelayCommitter_ARerunAfterALostAnswerChangesNothing(t *testing.T) {
	eachCommitter(t, "pokt1commit_rerun", func(t *testing.T, h commitHarness) {
		const sessionID = "sess-commit-rerun"
		h.createSession(sessionID, SessionStateActive)
		relays := relaysFor(h.publish(2), "h-0", "h-1")
		_, err := h.commit().CommitSession(context.Background(), sessionID, relays)
		require.NoError(t, err, "premise: the first run succeeds, its answer is what gets lost")

		res, err := h.commit().CommitSession(context.Background(), sessionID, relays)

		require.NoError(t, err)
		require.Equal(t, relayBatchResult{status: 0}, res, "nothing new, nothing counted, no duplicate: its own entries are gone")
		snap := h.snapshot(sessionID)
		require.Equal(t, int64(2), snap.RelayCount, "counted once")
		require.Equal(t, uint64(300), snap.TotalComputeUnits)
		require.Equal(t, int64(2), h.marked(sessionID))
	})
}

func TestRelayCommitter_ARelayMarkedElsewhereIsAFreshDuplicateNotCounted(t *testing.T) {
	eachCommitter(t, "pokt1commit_dup", func(t *testing.T, h commitHarness) {
		const sessionID = "sess-commit-dup"
		h.createSession(sessionID, SessionStateActive)
		relays := relaysFor(h.publish(2), "h-new", "h-finished")
		h.markDone(sessionID, []byte("h-finished"))

		res, err := h.commit().CommitSession(context.Background(), sessionID, relays)

		require.NoError(t, err)
		require.Equal(t, relayBatchResult{status: 0, newRelays: 1, newComputeUnits: 100, freshDups: 1}, res)
		require.Equal(t, int64(1), h.snapshot(sessionID).RelayCount, "only the new relay is counted")
		require.Zero(t, h.queued(), "the duplicate's entry is acknowledged too")
	})
}

// The same relay twice in one batch counts once; the second copy is a
// duplicate whose entry this commit removes.
func TestRelayCommitter_TheSameRelayTwiceInOneBatchCountsOnce(t *testing.T) {
	eachCommitter(t, "pokt1commit_twice", func(t *testing.T, h commitHarness) {
		const sessionID = "sess-commit-twice"
		h.createSession(sessionID, SessionStateActive)
		relays := relaysFor(h.publish(2), "h-same", "h-same")

		res, err := h.commit().CommitSession(context.Background(), sessionID, relays)

		require.NoError(t, err)
		require.Equal(t, relayBatchResult{status: 0, newRelays: 1, newComputeUnits: 100, freshDups: 1}, res)
		require.Equal(t, int64(1), h.marked(sessionID))
		require.Zero(t, h.queued())
	})
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
			eachCommitter(t, "pokt1commit_"+tc.name, func(t *testing.T, h commitHarness) {
				sessionID := "sess-commit-" + tc.name
				if tc.state != "" {
					h.createSession(sessionID, tc.state)
				}
				relays := relaysFor(h.publish(2), "h-0", "h-1")

				res, err := h.commit().CommitSession(context.Background(), sessionID, relays)

				require.NoError(t, err)
				require.Equal(t, relayBatchResult{status: tc.status, newRelays: 2, newComputeUnits: 300}, res)
				require.Equal(t, int64(2), h.marked(sessionID), "marked, so a redelivery is not counted later")
				require.Zero(t, h.queued(), "acknowledged")
				if tc.state != "" {
					require.Zero(t, h.snapshot(sessionID).RelayCount, "a terminal session's counters do not move")
					require.Zero(t, h.snapshot(sessionID).TotalComputeUnits)
				}
			})
		})
	}
}

// A refusal is reported as errCommitRefused, and leaves nothing written: a mark
// with no count and no acknowledgement would be a permanent under-count.
func TestRelayCommitter_ARefusalWritesNothing(t *testing.T) {
	eachCommitter(t, "pokt1commit_refused", func(t *testing.T, h commitHarness) {
		const sessionID = "sess-commit-refused"
		h.createSession(sessionID, SessionStateActive)
		relays := relaysFor(h.publish(2), "h-0", "h-1")
		refusing := h.refusing(sessionID)

		_, err := refusing.CommitSession(context.Background(), sessionID, relays)

		require.ErrorIs(t, err, errCommitRefused)
		require.Zero(t, h.marked(sessionID), "nothing marked")
		require.Equal(t, int64(2), h.queued(), "nothing acknowledged")
	})
}

// An error raised by the server after the commit started writing is "outcome
// unknown": it must NOT wrap errCommitRefused, or the batch would finish the
// relays one at a time over a half-applied commit instead of retrying it.
//
// Redis only: a Lua script is not rolled back, a Pebble batch has no half.
// The failure is a counter that is not an integer, so HINCRBY fails after the
// SADD. What the half-applied run leaves -- marks written, nothing counted,
// entries still pending -- is asserted too: a retry then finds the marks and
// counts nothing. That under-count predates this test (the script's "nothing
// after the writes refuses" does not hold for HINCRBY); it is pinned here so a
// change to it is seen, not because it is right.
func TestRelayCommitter_AFailureAfterTheFirstWriteIsNotARefusal(t *testing.T) {
	const sessionID = "sess-commit-unknown"
	h := newRedisCommitHarness(t, "pokt1commit_unknown")
	w := h.w
	w.createSession(sessionID, SessionStateActive)
	require.NoError(t, w.client.HSet(w.ctx, w.store.sessionKey(sessionID), "relay_count", "not-a-number").Err())
	relays := relaysFor(w.publish(2), "h-0", "h-1")

	_, err := h.commit().CommitSession(w.ctx, sessionID, relays)

	require.Error(t, err)
	require.False(t, errors.Is(err, errCommitRefused), "a failure after a write is retried, not refused: %v", err)
	require.Equal(t, int64(2), w.marked(sessionID), "the SADD before the failure stayed written")
	require.Equal(t, int64(2), w.pending(), "nothing acknowledged: the entries come back")
}

func TestRelayCommitter_AckRejectedAcksWithoutMarkingOrCounting(t *testing.T) {
	eachCommitter(t, "pokt1commit_rejected", func(t *testing.T, h commitHarness) {
		const sessionID = "sess-commit-rejected"
		h.createSession(sessionID, SessionStateActive)
		ids := h.publish(3)

		require.NoError(t, h.commit().AckRejected(context.Background(), ids))

		require.Zero(t, h.queued())
		require.Zero(t, h.marked(sessionID))
		require.Zero(t, h.snapshot(sessionID).RelayCount)
	})
}

// notRedisDedup is a Deduplicator that is not the Redis one.
type notRedisDedup struct{ Deduplicator }

// Without a committer the supplier finishes every relay on its own. The nil
// must be an untyped one: a typed nil would pass the batch's nil check and
// panic on every use -- inside a flush, which its recover turns into the
// per-relay path, and in AckRejected, which no flush recover covers.
func TestRelayCommitter_NoneForANonRedisDeduplicatorMeansNoBatch(t *testing.T) {
	w := newRedisCommitHarness(t, "pokt1commit_none").w

	commit := newRedisRelayCommitter(w.client, w.store, notRedisDedup{}, w.consumer)

	require.True(t, commit == nil, "an untyped nil, got %#v", commit)
	require.Nil(t, newRelayBatch(w.batch.logger, w.supplier, notRedisDedup{}, w.smst, w.batch.coordinator, w.consumer, commit))
}

// The embedded store gives no batch either to a deduplicator other than its
// own: the committer's marks would be invisible to it.
func TestPebbleBackend_NoCommitterForAForeignDeduplicator(t *testing.T) {
	h := newPebbleCommitHarness(t, "pokt1commit_none_pebble")

	stores, err := h.backend.forSupplier("pokt1commit_none_pebble_2", notRedisDedup{})

	require.NoError(t, err)
	require.True(t, stores.commit == nil, "an untyped nil, got %#v", stores.commit)
}
