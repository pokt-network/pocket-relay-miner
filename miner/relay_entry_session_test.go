//go:build test

package miner

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// The relay path no longer reads the session for every relay. These tests pin
// what replaces that read: a relay of a session whose tree this process deleted
// is rejected at the entry, a session the store confirmed is not asked about
// again, and a session that could not be confirmed is asked about again.

// namedCommandCounter counts the commands a client sends, by name, piped or
// not, so a test can say which round trips a path made.
type namedCommandCounter struct {
	mu    sync.Mutex
	names map[string]int
}

func newNamedCommandCounter(client redis.UniversalClient) *namedCommandCounter {
	c := &namedCommandCounter{names: map[string]int{}}
	client.AddHook(c)
	return c
}

func (c *namedCommandCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *namedCommandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.add(cmd.Name())
		return next(ctx, cmd)
	}
}

func (c *namedCommandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			c.add(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

func (c *namedCommandCounter) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names[name]++
}

// take returns what was counted since the last take, and forgets it.
func (c *namedCommandCounter) take() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.names
	c.names = map[string]int{}
	return out
}

func TestRelayEntry_ARelayOfADeletedSessionIsDroppedBeforeATreeExists(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, "pokt1entry_deleted", "consumer-a")
	const sessionID = "sess-entry-deleted"
	ids := w.publish(2)

	w.deliver(w.msg(ids[0], sessionID, "entry-deleted-0", 100))
	w.batch.FlushAll(w.ctx)
	require.EqualValues(t, 1, w.snapshot(sessionID).RelayCount, "CONTROL: the first relay is counted")

	// What the lifecycle does when the session reaches a terminal state.
	require.NoError(t, w.smst.DeleteTree(w.ctx, sessionID))

	sealed := relaysRejected.WithLabelValues(w.supplier, "session_sealed", "svc-1")
	before := testutil.ToFloat64(sealed)
	w.deliver(w.msg(ids[1], sessionID, "entry-deleted-1", 100))
	require.Equal(t, before+1, testutil.ToFloat64(sealed),
		"the relay of a session whose tree was deleted must be rejected as session_sealed")

	w.batch.FlushAll(w.ctx)
	nodes, err := client.Exists(w.ctx, client.KB().SMSTNodesKey(w.supplier, sessionID)).Result()
	require.NoError(t, err)
	require.Zero(t, nodes, "no tree may be started under the deleted keys")
	require.EqualValues(t, 1, w.snapshot(sessionID).RelayCount, "the rejected relay is not counted")
	require.Zero(t, w.pending(), "the rejection is acknowledged")
}

func TestRelayEntry_TheSecondRelayOfAConfirmedSessionSendsNoCommand(t *testing.T) {
	client, _ := newTestRedis(t)
	counter := newNamedCommandCounter(client)
	w := newBatchWorker(t, client, "pokt1entry_confirmed", "consumer-a")
	const sessionID = "sess-entry-confirmed"
	ids := w.publish(2)

	counter.take()
	w.deliver(w.msg(ids[0], sessionID, "entry-confirmed-0", 100))
	first := counter.take()
	require.NotZero(t, first["hgetall"], "CONTROL: the first relay asks the store about its session, sent %v", first)

	w.deliver(w.msg(ids[1], sessionID, "entry-confirmed-1", 100))
	second := counter.take()
	require.Empty(t, second,
		"the second relay of a confirmed session must send no command before the flush: no session read, no node write")

	w.batch.FlushAll(w.ctx)
	require.EqualValues(t, 2, w.snapshot(sessionID).RelayCount)
	require.Zero(t, w.pending())
}

func TestRelayEntry_ASessionThatCouldNotBeCreatedIsAskedForAgain(t *testing.T) {
	client, _ := newTestRedis(t)
	fail := testredis.NewFailSwitch(client)
	w := newBatchWorker(t, client, "pokt1entry_retry", "consumer-a")
	const sessionID = "sess-entry-retry"
	ids := w.publish(2)

	fail.Fail("injected: redis unreachable for the first relay")
	w.deliver(w.msg(ids[0], sessionID, "entry-retry-0", 100))
	fail.Clear()
	require.Nil(t, w.snapshot(sessionID), "CONTROL: the first relay could not create the session")
	require.Equal(t, 1, w.held(sessionID), "CONTROL: the first relay itself is in the tree and in the batch")

	w.deliver(w.msg(ids[1], sessionID, "entry-retry-1", 100))
	require.NotNil(t, w.snapshot(sessionID), "the next relay must create the session the first one could not")

	w.batch.FlushAll(w.ctx)
	require.EqualValues(t, 2, w.snapshot(sessionID).RelayCount)
	require.Zero(t, w.pending())
}

func TestRelayEntry_ACorruptionEvictionDoesNotMarkTheSessionDeleted(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, "pokt1entry_evicted", "consumer-a")
	const sessionID = "sess-entry-evicted"
	ids := w.publish(2)

	w.deliver(w.msg(ids[0], sessionID, "entry-evicted-0", 100))
	w.batch.FlushAll(w.ctx)

	w.smst.evictCorruptSession(w.ctx, sessionID, "test_eviction")
	require.False(t, w.smst.SessionDeleted(sessionID), "an evicted session keeps going: it is not deleted")

	w.deliver(w.msg(ids[1], sessionID, "entry-evicted-1", 100))
	require.Equal(t, 1, w.held(sessionID), "the relay of an evicted session is processed, not rejected")

	w.batch.FlushAll(w.ctx)
	require.EqualValues(t, 2, w.snapshot(sessionID).RelayCount)
}

// Another miner that claimed the session stored its claimed_root; this miner
// resumes its tree from it, and the claimed tree refuses the relay, as it did
// with the per-relay session read.
func TestRelayEntry_ASessionAnotherMinerClaimedIsRejectedAsSealed(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1entry_failover_claimed", "sess-entry-failover-claimed"
	a := newBatchWorker(t, client, supplier, "consumer-a")
	b := newBatchWorker(t, client, supplier, "consumer-b")

	idsA := a.publish(1)
	a.deliver(a.msg(idsA[0], sessionID, "failover-claimed-0", 100))
	a.batch.FlushAll(a.ctx)
	_, err := a.smst.FlushTree(a.ctx, sessionID)
	require.NoError(t, err)

	sealed := relaysRejected.WithLabelValues(supplier, "session_sealed", "svc-1")
	before := testutil.ToFloat64(sealed)
	idsB := b.publish(1)
	b.deliver(b.msg(idsB[0], sessionID, "failover-claimed-1", 100))
	require.Equal(t, before+1, testutil.ToFloat64(sealed),
		"a tree resumed from the other miner's claimed_root must refuse the relay as session_sealed")

	b.batch.FlushAll(b.ctx)
	require.EqualValues(t, 1, b.snapshot(sessionID).RelayCount)
	require.Zero(t, b.pending())
}

// DIFFERENCE from the per-relay session read, pinned on purpose. Another miner
// took the session to a terminal state and deleted every key of its tree. This
// process holds no mark of that, so the relay goes into a new tree and is not
// rejected -- the read rejected it as session_sealed. It is still not counted:
// the relay batch's script finds the session terminal.
func TestRelayEntry_ASessionAnotherMinerDeletedIsNotCountedAndNotRejected(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1entry_failover_deleted", "sess-entry-failover-deleted"
	a := newBatchWorker(t, client, supplier, "consumer-a")
	b := newBatchWorker(t, client, supplier, "consumer-b")

	idsA := a.publish(1)
	a.deliver(a.msg(idsA[0], sessionID, "failover-deleted-0", 100))
	a.batch.FlushAll(a.ctx)
	require.NoError(t, a.store.UpdateState(a.ctx, sessionID, SessionStateClaimSkipped))
	require.NoError(t, a.smst.DeleteTree(a.ctx, sessionID))

	sealed := relaysRejected.WithLabelValues(supplier, "session_sealed", "svc-1")
	before := testutil.ToFloat64(sealed)
	idsB := b.publish(1)
	b.deliver(b.msg(idsB[0], sessionID, "failover-deleted-1", 100))
	require.Equal(t, before, testutil.ToFloat64(sealed), "this process cannot know the other miner deleted the tree")
	require.Equal(t, 1, b.held(sessionID), "the relay goes into a new tree and the batch")

	b.batch.FlushAll(b.ctx)
	require.EqualValues(t, 1, b.snapshot(sessionID).RelayCount, "the script finds the session terminal and does not count it")
	require.Zero(t, b.pending(), "it is acknowledged")
}

// DIFFERENCE from the per-relay session read, pinned on purpose. A claim that
// failed to broadcast takes the session to claim_tx_error, whose callback
// deletes the tree; the chain is then seen to hold the claim and the session
// goes back to claimed. The read saw "claimed", which is not terminal, and the
// relay was counted into a new tree no claim would ever use. The deleted mark
// rejects it instead.
func TestRelayEntry_ARelayAfterAClaimIsReactivatedIsRejectedAsSealed(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, "pokt1entry_reactivated", "consumer-a")
	const sessionID = "sess-entry-reactivated"
	ids := w.publish(2)

	w.deliver(w.msg(ids[0], sessionID, "entry-reactivated-0", 100))
	w.batch.FlushAll(w.ctx)
	require.NoError(t, w.store.UpdateState(w.ctx, sessionID, SessionStateClaimTxError))
	require.NoError(t, w.smst.DeleteTree(w.ctx, sessionID))
	reactivated, err := w.store.ReactivateClaimed(w.ctx, sessionID, bytes.Repeat([]byte{1}, SMSTRootLen), "claim-tx")
	require.NoError(t, err)
	require.True(t, reactivated, "CONTROL: the session is back to claimed")

	sealed := relaysRejected.WithLabelValues(w.supplier, "session_sealed", "svc-1")
	before := testutil.ToFloat64(sealed)
	w.deliver(w.msg(ids[1], sessionID, "entry-reactivated-1", 100))
	require.Equal(t, before+1, testutil.ToFloat64(sealed))

	w.batch.FlushAll(w.ctx)
	require.EqualValues(t, 1, w.snapshot(sessionID).RelayCount)
	require.Zero(t, w.pending())
}
