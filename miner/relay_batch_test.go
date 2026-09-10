//go:build test

package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pokt-network/smt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// syncBuffer is a log sink several goroutines can write to under -race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// batchWorker is one miner's view of one supplier, wired the way
// addSupplierWithData wires it -- real session store, SMST manager,
// deduplicator, stream consumer and relay batch -- against the shared REAL
// Redis. The stream is real because acknowledgement is what the batch changes,
// and the only honest check that an entry was acknowledged is asking the server.
type batchWorker struct {
	t            *testing.T
	ctx          context.Context
	client       *redisutil.Client
	supplier     string
	stream       string
	group        string
	consumerName string
	store        *RedisSessionStore
	smst         *RedisSMSTManager
	dedup        *RedisDeduplicator
	consumer     *redisutil.StreamsConsumer
	batch        *relayBatch
	mgr          *SupplierManager
	state        *SupplierState
	worker       *SupplierWorker
	logs         *syncBuffer
}

func newBatchWorker(t *testing.T, client *redisutil.Client, supplier, consumerName string) *batchWorker {
	t.Helper()
	ctx := context.Background()
	logs := &syncBuffer{}
	logger := zerolog.New(logs).Level(zerolog.DebugLevel)

	store := NewRedisSessionStore(logger, client, SessionStoreConfig{SupplierAddress: supplier})
	coordinator := NewSessionCoordinator(logger, store, SMSTRecoveryConfig{SupplierAddress: supplier})
	smstMgr := NewRedisSMSTManager(logger, client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	dedup := NewRedisDeduplicator(logger, client, DeduplicatorConfig{TTLBlocks: 10, BlockTimeSeconds: 30})

	stream := transport.SupplierStreamName(client.KB().StreamPrefix(), supplier)
	group := client.KB().ConsumerGroup()
	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		require.Contains(t, err.Error(), "BUSYGROUP", "a second worker on the same stream finds the group made")
	}
	consumer, err := redisutil.NewStreamsConsumer(logger, client, transport.ConsumerConfig{
		StreamPrefix:            client.KB().StreamPrefix(),
		SupplierOperatorAddress: supplier,
		ConsumerGroup:           group,
		ConsumerName:            consumerName,
		ClaimIdleTimeout:        60000,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = consumer.Close() })

	batch := newRelayBatch(logger, client, supplier, store, dedup, smstMgr, coordinator, consumer)
	require.NotNil(t, batch, "a Redis deduplicator must get a batch")

	mgr := &SupplierManager{
		logger:       logger,
		suppliers:    xsync.NewMap[string, *SupplierState](),
		deduplicator: dedup,
	}
	state := &SupplierState{
		OperatorAddr:       supplier,
		Consumer:           consumer,
		SessionStore:       store,
		SessionCoordinator: coordinator,
		SMSTManager:        smstMgr,
		relayBatch:         batch,
	}
	state.StoreStatus(SupplierStatusActive)
	mgr.suppliers.Store(supplier, state)

	worker := &SupplierWorker{logger: logger, config: SupplierWorkerConfig{Logger: logger}}
	worker.supplierManager = mgr
	mgr.onRelay = worker.handleRelay

	return &batchWorker{
		t: t, ctx: ctx, client: client, supplier: supplier,
		stream: stream, group: group, consumerName: consumerName,
		store: store, smst: smstMgr, dedup: dedup, consumer: consumer,
		batch: batch, mgr: mgr, state: state, worker: worker, logs: logs,
	}
}

// publish adds n entries to the supplier's stream and delivers them to this
// worker's consumer, returning their IDs in order.
func (w *batchWorker) publish(n int) []string {
	w.t.Helper()
	for i := 0; i < n; i++ {
		require.NoError(w.t, w.client.XAdd(w.ctx, &redis.XAddArgs{
			Stream: w.stream, Values: map[string]any{"data": "x"},
		}).Err())
	}
	read, err := w.client.XReadGroup(w.ctx, &redis.XReadGroupArgs{
		Group: w.group, Consumer: w.consumerName, Streams: []string{w.stream, ">"}, Count: int64(n),
	}).Result()
	require.NoError(w.t, err)
	require.Len(w.t, read[0].Messages, n, "premise: every entry is delivered and unacknowledged")
	ids := make([]string, n)
	for i, m := range read[0].Messages {
		ids[i] = m.ID
	}
	return ids
}

// msg builds a FRESH delivery of one relay: handleStreamMessage returns the
// message to its pool, so a delivered object must never be delivered again.
func (w *batchWorker) msg(id, sessionID, payload string, cu uint64) transport.StreamMessage {
	m := newStreamMessage(w.supplier, sessionID, payload, cu)
	m.ID = id
	m.StreamName = w.stream
	return *m
}

func (w *batchWorker) deliver(m transport.StreamMessage) bool {
	return w.mgr.handleStreamMessage(w.ctx, w.state, m)
}

func (w *batchWorker) pending() int64 {
	w.t.Helper()
	res, err := w.client.XPending(w.ctx, w.stream, w.group).Result()
	require.NoError(w.t, err)
	return res.Count
}

func (w *batchWorker) streamLen() int64 {
	w.t.Helper()
	n, err := w.client.XLen(w.ctx, w.stream).Result()
	require.NoError(w.t, err)
	return n
}

func (w *batchWorker) marked(sessionID string) int64 {
	w.t.Helper()
	n, err := w.client.SCard(w.ctx, w.dedup.sessionKey(sessionID)).Result()
	require.NoError(w.t, err)
	return n
}

func (w *batchWorker) held(sessionID string) int {
	w.batch.mu.Lock()
	defer w.batch.mu.Unlock()
	if sb := w.batch.sessions[sessionID]; sb != nil {
		return len(sb.relays)
	}
	return 0
}

func (w *batchWorker) snapshot(sessionID string) *SessionSnapshot {
	w.t.Helper()
	snap, err := w.store.Get(w.ctx, sessionID)
	require.NoError(w.t, err)
	return snap
}

// leavesAfterRestart is how many relays a miner that starts NOW finds in the
// session's tree: a fresh SMST manager resumes from what Redis holds and seals.
func leavesAfterRestart(t *testing.T, client *redisutil.Client, supplier, sessionID string) uint64 {
	t.Helper()
	fresh := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier})
	root, err := fresh.FlushTree(context.Background(), sessionID)
	require.NoError(t, err)
	count, err := smt.MerkleSumRoot(root).Count()
	require.NoError(t, err)
	return count
}

// supplierSeriesSum adds up every series of a collector whose "supplier" label
// is supplier: counter values, or histogram sample counts when statusCode
// narrows it to one status_code.
func supplierSeriesSum(t *testing.T, c prometheus.Collector, supplier, statusCode string) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() { c.Collect(ch); close(ch) }()
	total := 0.0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["supplier"] != supplier || (statusCode != "" && labels["status_code"] != statusCode) {
			continue
		}
		if pb.GetCounter() != nil {
			total += pb.GetCounter().GetValue()
		}
		if pb.GetHistogram() != nil {
			total += float64(pb.GetHistogram().GetSampleCount())
		}
	}
	return total
}

func indexOf(ids []string, id string) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return -1
}

// TestRelayBatch_ACutBetweenCheckpointAndScriptLosesNothing is T1: the money
// invariant of the flush. The script acknowledges the entries, and an
// acknowledged entry is never delivered again -- so the tree must already be
// reachable from a stored root holding those relays.
//
// Seven relays, fewer than the per-relay checkpoint interval of 10, so before
// the flush the stored live_root holds only the first. The flush is cut between
// its two steps, the miner "dies", and a new one takes the pending entries.
// If the steps ran in the other order the cut would land after the
// acknowledgement: nothing left to redeliver, and a restart resumes one leaf.
func TestRelayBatch_ACutBetweenCheckpointAndScriptLosesNothing(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID, n = "pokt1batch_cut", "sess-cut", 7

	a := newBatchWorker(t, client, supplier, "a")
	ids := a.publish(n)
	for i, id := range ids {
		a.deliver(a.msg(id, sessionID, fmt.Sprintf("relay-%d", i), 100))
	}
	require.Equal(t, n, a.held(sessionID), "premise: every relay is in the tree and waiting in the batch")

	a.batch.hook = func(p flushPoint, _ string) error {
		if p == flushPointBeforeScript {
			return errors.New("miner killed between the flush's two round trips")
		}
		return nil
	}
	a.batch.FlushAll(a.ctx)

	// A new miner reclaims what the dead one left pending and processes it.
	b := newBatchWorker(t, client, supplier, "b")
	reclaimed, err := client.XClaimJustID(b.ctx, &redis.XClaimArgs{
		Stream: b.stream, Group: b.group, Consumer: "b", MinIdle: 0, Messages: ids,
	}).Result()
	require.NoError(t, err)
	for _, id := range reclaimed {
		m := b.msg(id, sessionID, fmt.Sprintf("relay-%d", indexOf(ids, id)), 100)
		m.IsReclaim = true
		b.deliver(m)
	}
	b.batch.FlushAll(b.ctx)

	leaves := leavesAfterRestart(t, client, supplier, sessionID)
	require.Equal(t, uint64(n), leaves,
		"%d relays were served and %d are in the tree a restarted miner finds: the rest were "+
			"acknowledged before a live_root covered them, so no redelivery could put them back", n, leaves)
	snap := b.snapshot(sessionID)
	require.NotNil(t, snap)
	require.Equal(t, int64(n), snap.RelayCount, "every relay counted exactly once")
	require.Equal(t, uint64(n*100), snap.TotalComputeUnits)
	require.Zero(t, b.streamLen(), "every entry deleted from the stream")
	require.Zero(t, b.pending(), "none left pending")
}

// TestRelayBatchScript_RefusesALegacySessionBeforeWriting is T2. A Lua script
// that fails halfway is not rolled back, so the script refuses a pre-hash
// session BEFORE its first write. Refusing after the SADD would leave the hashes
// marked with nothing counted and nothing acknowledged: the redelivery would
// find them marked and skip the counter for good.
func TestRelayBatchScript_RefusesALegacySessionBeforeWriting(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_legacy_raw", "sess-legacy-raw"
	w := newBatchWorker(t, client, supplier, "a")

	legacy, err := json.Marshal(SessionSnapshot{SessionID: sessionID, SupplierOperatorAddress: supplier, State: SessionStateActive})
	require.NoError(t, err)
	require.NoError(t, client.Set(w.ctx, w.store.sessionKey(sessionID), legacy, 0).Err())

	ids := w.publish(2)
	sb := &sessionBatch{
		session: relaySession{sessionID: sessionID, supplier: supplier, serviceID: "svc-1"},
		relays:  []batchedRelay{{id: ids[0], hash: []byte("hash-0"), computeUnits: 100}, {id: ids[1], hash: []byte("hash-1"), computeUnits: 100}},
	}
	_, err = w.batch.runScript(w.ctx, sb)
	require.True(t, isLegacyKeyErr(err), "the script must refuse a legacy session, got %v", err)

	require.Zero(t, w.marked(sessionID),
		"nothing may be marked by a refused script: a mark with no count and no ack is a permanent under-count")
	require.Equal(t, int64(2), w.pending(), "and nothing acknowledged")
	keyType, err := client.Type(w.ctx, w.store.sessionKey(sessionID)).Result()
	require.NoError(t, err)
	require.Equal(t, "string", keyType, "the refused script leaves the legacy key as it was")
}

// TestRelayBatch_MigratesALegacySessionAndCounts is T2's other half: the flush
// keeps IncrementRelayCount's rescue -- rewrite the legacy key, run again.
func TestRelayBatch_MigratesALegacySessionAndCounts(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_legacy", "sess-legacy"
	w := newBatchWorker(t, client, supplier, "a")

	legacy, err := json.Marshal(SessionSnapshot{
		SessionID: sessionID, SupplierOperatorAddress: supplier, ServiceID: "svc-1",
		SessionStartHeight: 1, SessionEndHeight: 10, State: SessionStateActive,
	})
	require.NoError(t, err)
	require.NoError(t, client.Set(w.ctx, w.store.sessionKey(sessionID), legacy, 0).Err())

	ids := w.publish(2)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("legacy-%d", i), 100))
	}
	w.batch.FlushAll(w.ctx)

	keyType, err := client.Type(w.ctx, w.store.sessionKey(sessionID)).Result()
	require.NoError(t, err)
	require.Equal(t, "hash", keyType, "the flush migrated the session")
	snap := w.snapshot(sessionID)
	require.Equal(t, int64(2), snap.RelayCount)
	require.Equal(t, uint64(200), snap.TotalComputeUnits)
	require.Zero(t, w.pending())
}

// TestRelayBatch_CountsOnlyNewMembersWithTheirOwnComputeUnits is T3. One session
// can hold relays mined at different CUPRs, and part of a batch can already be
// marked -- another consumer processed those copies. The delta is the members
// the SADD added, each with its OWN compute units: not the batch's length, and
// not the count times the first relay's units.
func TestRelayBatch_CountsOnlyNewMembersWithTheirOwnComputeUnits(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_cupr", "sess-cupr"
	w := newBatchWorker(t, client, supplier, "a")

	cus := []uint64{100, 250, 100, 400}
	ids := w.publish(len(cus))
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("cupr-%d", i), cus[i]))
	}

	// Relays 0 and 2 were already marked by another consumer.
	for _, i := range []int{0, 2} {
		m := newStreamMessage(supplier, sessionID, fmt.Sprintf("cupr-%d", i), cus[i])
		added, err := w.dedup.MarkProcessed(w.ctx, m.Message.RelayHash, sessionID)
		require.NoError(t, err)
		require.True(t, added)
	}

	w.batch.FlushAll(w.ctx)

	snap := w.snapshot(sessionID)
	require.Equal(t, int64(2), snap.RelayCount,
		"only relays 1 and 3 were new; %d means the batch counted its length", len(cus))
	require.Equal(t, uint64(250+400), snap.TotalComputeUnits,
		"the new relays' own units (250+400); 200 would be 2 x the first relay's CUPR")
	require.Equal(t, int64(len(cus)), w.marked(sessionID))
	require.Zero(t, w.pending(), "the already-marked copies are acknowledged too")
}

// TestRelayBatch_TwoConsumersCountOnce is T4: a slow-but-alive consumer and one
// that reclaimed its entries both process the same relays and both flush, in
// either order. The count stays N because each counts only what its SADD added.
func TestRelayBatch_TwoConsumersCountOnce(t *testing.T) {
	for _, order := range []string{"slow_first", "reclaimer_first"} {
		t.Run(order, func(t *testing.T) {
			client, _ := newTestRedis(t)
			supplier := "pokt1batch_two_" + order
			const sessionID, n = "sess-two", 5

			slow := newBatchWorker(t, client, supplier, "slow")
			ids := slow.publish(n)
			for i, id := range ids {
				slow.deliver(slow.msg(id, sessionID, fmt.Sprintf("two-%d", i), 100))
			}

			reclaimer := newBatchWorker(t, client, supplier, "reclaimer")
			reclaimed, err := client.XClaimJustID(reclaimer.ctx, &redis.XClaimArgs{
				Stream: reclaimer.stream, Group: reclaimer.group, Consumer: "reclaimer", MinIdle: 0, Messages: ids,
			}).Result()
			require.NoError(t, err)
			require.Len(t, reclaimed, n)
			for _, id := range reclaimed {
				m := reclaimer.msg(id, sessionID, fmt.Sprintf("two-%d", indexOf(ids, id)), 100)
				m.IsReclaim = true
				reclaimer.deliver(m)
			}

			first, second := slow, reclaimer
			if order == "reclaimer_first" {
				first, second = reclaimer, slow
			}
			first.batch.FlushAll(first.ctx)
			second.batch.FlushAll(second.ctx)

			snap := slow.snapshot(sessionID)
			require.Equal(t, int64(n), snap.RelayCount, "both consumers flushed the same %d relays", n)
			require.Equal(t, uint64(n*100), snap.TotalComputeUnits)
			require.Zero(t, slow.pending())
			require.Zero(t, slow.streamLen())
		})
	}
}

// The next four re-express, through the BATCH path, the invariants the
// per-relay tests pin (redelivery_session_creation_test.go and
// TestHandleRelay_OnRelayProcessedError_AcksAfterSmstAndDedup). With a batch
// wired those tests no longer run the code production runs.

func TestRelayBatch_RedeliveryStillCreatesTheSession(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_redelivery", "sess-redelivered"
	w := newBatchWorker(t, client, supplier, "a")

	id := w.publish(1)[0]
	m := w.msg(id, sessionID, "relay-redelivered", 100)
	added, err := w.dedup.MarkProcessed(w.ctx, m.Message.RelayHash, sessionID)
	require.NoError(t, err)
	require.True(t, added)
	require.Nil(t, w.snapshot(sessionID), "premise: the session does not exist")

	m.IsReclaim = true
	require.True(t, w.deliver(m), "a reclaimed duplicate is acknowledged at once")
	w.batch.FlushAll(w.ctx)

	snap := w.snapshot(sessionID)
	require.NotNil(t, snap, "the session MUST exist after the redelivery, or nothing claims the tree")
	require.Equal(t, int64(10), snap.SessionEndHeight)
	require.Zero(t, w.pending())
}

func TestRelayBatch_RedeliveryDoesNotDoubleCount(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_nodouble", "sess-no-double"
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(3)
	// The same relay as three entries: twice in one batch, once after a flush.
	w.deliver(w.msg(ids[0], sessionID, "relay-once", 100))
	w.deliver(w.msg(ids[1], sessionID, "relay-once", 100))
	w.batch.FlushAll(w.ctx)
	w.deliver(w.msg(ids[2], sessionID, "relay-once", 100))
	w.batch.FlushAll(w.ctx)

	snap := w.snapshot(sessionID)
	require.Equal(t, int64(1), snap.RelayCount, "one relay, three deliveries, one count")
	require.Equal(t, uint64(100), snap.TotalComputeUnits)
	require.Zero(t, w.pending(), "every copy acknowledged")
}

func TestRelayBatch_OriginalCopyAfterAReclaimStillCreatesTheSession(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_original", "sess-original-copy"
	w := newBatchWorker(t, client, supplier, "a")

	id := w.publish(1)[0]
	m := w.msg(id, sessionID, "relay-original", 100)
	added, err := w.dedup.MarkProcessed(w.ctx, m.Message.RelayHash, sessionID)
	require.NoError(t, err)
	require.True(t, added)

	w.deliver(m) // IsReclaim=false: past the reclaim guard, into the batch
	w.batch.FlushAll(w.ctx)

	snap := w.snapshot(sessionID)
	require.NotNil(t, snap, "the session must exist however the duplicate reached us")
	require.Zero(t, snap.RelayCount, "the copy was already counted elsewhere")
	require.Zero(t, w.pending())
}

// TestRelayBatch_ASessionThatCannotBeCountedIsStillMarkedAndAcked mirrors the
// per-relay contract that a counter failure does not hold the relay: the tree
// and the dedup set are the truth, the counter is best-effort.
func TestRelayBatch_ASessionThatCannotBeCountedIsStillMarkedAndAcked(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_nocount", "sess-no-count"
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(2)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("nocount-%d", i), 100))
	}
	require.NoError(t, client.Del(w.ctx, w.store.sessionKey(sessionID)).Err(), "the session hash vanishes before the flush")

	w.batch.FlushAll(w.ctx)

	require.Equal(t, int64(2), w.marked(sessionID), "marked, as the per-relay path marks before counting")
	require.Zero(t, w.pending(), "and acknowledged")
	require.Nil(t, w.snapshot(sessionID), "the script does not recreate a session it cannot count into")
}

// TestRelayBatch_APanicInTheFlushLosesOnlyTheRelayThatPanics is T7 (Jorge,
// 2026-09-10): a panic in the flush sends the batch down the per-relay path,
// where a panic loses only the relay that causes it -- counted as lost and
// acknowledged -- and every other relay is counted and acknowledged.
func TestRelayBatch_APanicInTheFlushLosesOnlyTheRelayThatPanics(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID, n = "pokt1batch_panic", "sess-panic", 4
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(n)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("panic-%d", i), 100))
	}

	batchPanics := relayBatchPanicsTotal.WithLabelValues(supplier)
	lost := relaysLostTotal.WithLabelValues(supplier, "svc-1", "panic_recovered")
	batchPanicsBefore, lostBefore := testutil.ToFloat64(batchPanics), testutil.ToFloat64(lost)

	poisoned := ids[2]
	w.batch.hook = func(p flushPoint, id string) error {
		switch {
		case p == flushPointBeforeScript:
			panic("the flush blew up")
		case p == flushPointFallbackRelay && id == poisoned:
			panic("this relay blew up")
		}
		return nil
	}
	w.batch.FlushAll(w.ctx)

	snap := w.snapshot(sessionID)
	require.Equal(t, int64(n-1), snap.RelayCount, "every relay but the one that panicked is counted")
	require.Equal(t, batchPanicsBefore+1, testutil.ToFloat64(batchPanics), "one batch fell back because of a panic")
	require.Equal(t, lostBefore+1, testutil.ToFloat64(lost), "exactly one relay lost to a panic")
	require.Equal(t, int64(n-1), w.marked(sessionID))
	require.Zero(t, w.pending(), "all acknowledged, the lost one included: a deterministic panic must not loop")
	require.Zero(t, w.held(sessionID))
	require.Contains(t, w.logs.String(), "PANIC RECOVERED flushing a relay batch")
}

// TestHandleStreamMessage_ABatchedRelayIsNeitherAckedNorReleasedNorAFailure
// pins the sentinel. handleRelay returns ErrRelayBatched when the batch owns
// the rest of the relay; read as an ordinary error, handleStreamMessage would
// hand the entry back, record a failed relay and log it -- for a relay that
// succeeded.
func TestHandleStreamMessage_ABatchedRelayIsNeitherAckedNorReleasedNorAFailure(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_sentinel", "sess-sentinel"
	w := newBatchWorker(t, client, supplier, "a")

	id := w.publish(1)[0]
	acked := w.deliver(w.msg(id, sessionID, "relay-sentinel", 100))

	require.False(t, acked, "not acknowledged by handleStreamMessage: the batch will")
	require.Equal(t, 1, w.held(sessionID), "the batch holds it")
	ext, err := client.XPendingExt(w.ctx, &redis.XPendingExtArgs{
		Stream: w.stream, Group: w.group, Start: "-", End: "+", Count: 10,
	}).Result()
	require.NoError(t, err)
	require.Len(t, ext, 1)
	require.Equal(t, "a", ext[0].Consumer, "still owned by this consumer, not handed back")
	require.Zero(t, supplierSeriesSum(t, relaysRejected, supplier, ""), "no rejection recorded")
	require.Zero(t, supplierSeriesSum(t, relayProcessingLatency, supplier, "error"), "no failed processing recorded")
	require.NotContains(t, w.logs.String(), `"level":"error"`)
	require.NotContains(t, w.logs.String(), "failed to process relay")

	w.batch.FlushAll(w.ctx)
	require.Zero(t, w.pending(), "the flush acknowledges it")
}

// TestRelayBatch_ARelayForASealedTreeIsDroppedAndCountedNotBatched (Jorge,
// 2026-09-10): a relay that arrives after its session's tree is sealed is
// acknowledged at once and counted as rejected, exactly as before the batch --
// it never enters the batch, which would acknowledge it without a trace.
func TestRelayBatch_ARelayForASealedTreeIsDroppedAndCountedNotBatched(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_sealed", "sess-sealed"
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(2)
	w.deliver(w.msg(ids[0], sessionID, "before-seal", 100))
	w.batch.FlushAll(w.ctx)
	_, err := w.smst.FlushTree(w.ctx, sessionID)
	require.NoError(t, err, "the claim seals the tree")

	sealed := relaysRejected.WithLabelValues(supplier, "session_sealed", "svc-1")
	before := testutil.ToFloat64(sealed)

	acked := w.deliver(w.msg(ids[1], sessionID, "after-seal", 100))
	require.Zero(t, w.held(sessionID), "a sealed tree's relay must not wait in the batch")
	require.True(t, acked, "acknowledged at once")
	require.Equal(t, before+1, testutil.ToFloat64(sealed), "and it is counted as rejected")
	require.Zero(t, w.pending())
}

// TestRelayBatch_ConcurrentAddAndFlushCountEveryRelayOnce runs the consume loop
// and the lifecycle's claim-transition flush at once, under -race: the batch
// serialises them, so nothing is lost or counted twice.
func TestRelayBatch_ConcurrentAddAndFlushCountEveryRelayOnce(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID, n = "pokt1batch_concurrent", "sess-concurrent", 200
	w := newBatchWorker(t, client, supplier, "a")
	ids := w.publish(n)

	done := make(chan struct{})
	var flusher sync.WaitGroup
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		for {
			select {
			case <-done:
				return
			default:
				w.batch.FlushSessions(w.ctx, []string{sessionID})
			}
		}
	}()

	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("concurrent-%d", i), 100))
	}
	close(done)
	flusher.Wait()
	w.batch.FlushAll(w.ctx)

	snap := w.snapshot(sessionID)
	require.Equal(t, int64(n), snap.RelayCount)
	require.Equal(t, uint64(n*100), snap.TotalComputeUnits)
	require.Equal(t, int64(n), w.marked(sessionID))
	require.Zero(t, w.pending())
	require.Zero(t, w.streamLen())
}

// TestRelayBatch_ARedisFailureKeepsTheBatchForTheNextFlush: a flush that fails
// with nothing known to be written keeps the batch; the next one counts it.
func TestRelayBatch_ARedisFailureKeepsTheBatchForTheNextFlush(t *testing.T) {
	client, _ := newTestRedis(t)
	fail := testredis.NewFailSwitch(client)
	const supplier, sessionID = "pokt1batch_retry", "sess-retry"
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(3)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("retry-%d", i), 100))
	}

	fail.Fail("redis unreachable")
	w.batch.FlushAll(w.ctx)
	fail.Clear()

	require.Equal(t, 3, w.held(sessionID), "kept, not dropped")
	require.Zero(t, w.marked(sessionID))
	require.Equal(t, int64(3), w.pending())

	w.batch.FlushAll(w.ctx)
	require.Equal(t, int64(3), w.snapshot(sessionID).RelayCount)
	require.Zero(t, w.pending())
	require.Zero(t, w.held(sessionID))
}

// TestRelayBatch_OnExitTheBatchIsReleasedNotFlushed (Jorge, 2026-09-10): on the
// way out the batch only lets go of what it holds. The supplier's context is
// already cancelled when this runs, so the release must detach from it. Nothing
// is counted or marked, the entries are claimable at once, and the consumer
// that takes them counts every relay exactly once.
func TestRelayBatch_OnExitTheBatchIsReleasedNotFlushed(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID, n = "pokt1batch_exit", "sess-exit", 3
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(n)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("exit-%d", i), 100))
	}

	supplierCtx, cancel := context.WithCancel(w.ctx)
	cancel()
	w.mgr.releaseRelayBatchOnExit(supplierCtx, w.state)

	require.Zero(t, w.snapshot(sessionID).RelayCount, "released, not flushed: relay_count untouched")
	claimed := requireReleased(t, w, sessionID, n)

	next := newBatchWorker(t, client, supplier, "other")
	for _, id := range claimed {
		m := next.msg(id, sessionID, fmt.Sprintf("exit-%d", indexOf(ids, id)), 100)
		m.IsReclaim = true
		next.deliver(m)
	}
	next.batch.FlushAll(next.ctx)
	require.Equal(t, int64(n), next.snapshot(sessionID).RelayCount, "the next consumer counts each relay once")
	require.Zero(t, next.pending())
}

// TestRelayBatch_OnKeyRemovalTheBatchIsAckedAsLost: with the signing key gone
// nobody in this fleet can claim these relays, so releasing them would leave
// them pending for a consumer that never comes. They are acknowledged and
// counted as dropped for want of a key -- what drainDeliveryBuffer does with the
// delivery buffer -- and neither marked nor counted.
func TestRelayBatch_OnKeyRemovalTheBatchIsAckedAsLost(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID, n = "pokt1batch_keyremoved", "sess-key-removed", 3
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(n)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("keyremoved-%d", i), 100))
	}

	noKey := relaysDroppedNoKey.WithLabelValues(supplier, "svc-1")
	before := testutil.ToFloat64(noKey)

	w.state.drainReason.Store(int32(drainKeyRemoved))
	supplierCtx, cancel := context.WithCancel(w.ctx)
	cancel()
	w.mgr.releaseRelayBatchOnExit(supplierCtx, w.state)

	require.Zero(t, w.pending(), "acknowledged, not left pending for a consumer that cannot exist")
	require.Zero(t, w.streamLen())
	require.Equal(t, before+float64(n), testutil.ToFloat64(noKey), "each relay counted as dropped for want of a key")
	require.Zero(t, w.snapshot(sessionID).RelayCount, "not counted")
	require.Zero(t, w.marked(sessionID), "not marked")
	require.Zero(t, w.held(sessionID))
}

// TestRelayBatch_ATreeNoLongerResidentHandsTheBatchBack: with no tree to cover
// the relays, acknowledging them could lose them; they are handed back.
func TestRelayBatch_ATreeNoLongerResidentHandsTheBatchBack(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1batch_gone", "sess-gone"
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(2)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("gone-%d", i), 100))
	}
	require.NoError(t, w.smst.DeleteTree(w.ctx, sessionID))
	w.batch.FlushAll(w.ctx)

	requireReleased(t, w, sessionID, 2)
}

// requireReleased checks the batch let go of n entries unacknowledged and
// unmarked, and returns them claimed by a consumer named "other".
func requireReleased(t *testing.T, w *batchWorker, sessionID string, n int) []string {
	t.Helper()
	require.Zero(t, w.held(sessionID), "no longer held")
	require.Zero(t, w.marked(sessionID), "never marked, so a redelivery counts them")
	require.Equal(t, int64(n), w.streamLen(), "not acknowledged: still in the stream")
	claimed, _, err := w.client.XAutoClaimJustID(w.ctx, &redis.XAutoClaimArgs{
		Stream: w.stream, Group: w.group, Consumer: "other", MinIdle: 30 * time.Second, Start: "0", Count: 10,
	}).Result()
	require.NoError(t, err)
	require.Len(t, claimed, n, "released, not merely pending: another consumer can take them now")
	return claimed
}

// TestRelayBatch_AtTheCapAFailedFlushSendsTheNewRelayOnItsOwn bounds retention:
// a session at relayBatchCap flushes before taking more, and when that flush
// fails the new relay is refused, to be finished on its own.
func TestRelayBatch_AtTheCapAFailedFlushSendsTheNewRelayOnItsOwn(t *testing.T) {
	client, _ := newTestRedis(t)
	fail := testredis.NewFailSwitch(client)
	const supplier, sessionID = "pokt1batch_cap", "sess-cap"
	w := newBatchWorker(t, client, supplier, "a")

	// One real relay makes the tree resident; the rest only fill the batch.
	w.deliver(w.msg(w.publish(1)[0], sessionID, "cap-real", 100))
	s := relaySession{sessionID: sessionID, supplier: supplier, serviceID: "svc-1"}
	for i := 1; i < relayBatchCap; i++ {
		require.True(t, w.batch.Add(w.ctx, s, batchedRelay{id: fmt.Sprintf("1-%d", i), hash: []byte(fmt.Sprintf("cap-%d", i)), computeUnits: 1}))
	}
	require.Equal(t, relayBatchCap, w.held(sessionID))

	fail.Fail("redis unreachable")
	took := w.batch.Add(w.ctx, s, batchedRelay{id: "2-1", hash: []byte("cap-over"), computeUnits: 1})
	fail.Clear()
	require.False(t, took, "at the cap with a failing flush the batch must refuse, not grow")
	require.Equal(t, relayBatchCap, w.held(sessionID))

	require.True(t, w.batch.Add(w.ctx, s, batchedRelay{id: "2-2", hash: []byte("cap-next"), computeUnits: 1}),
		"at the cap with a working flush it flushes and takes the relay")
	require.Equal(t, 1, w.held(sessionID))
	require.Equal(t, int64(relayBatchCap), w.snapshot(sessionID).RelayCount)
}

// countSeenCallback records the relay_count each session carries when the claim
// transition hands it to the claim callback.
type countSeenCallback struct {
	SessionLifecycleCallback
	seen map[string]int64
}

func (c *countSeenCallback) OnSessionsNeedClaim(_ context.Context, snapshots []*SessionSnapshot) (ClaimCycleResult, error) {
	for _, s := range snapshots {
		c.seen[s.SessionID] = s.RelayCount
	}
	return ClaimCycleResult{}, nil
}

// TestClaimTransition_FlushesTheBatchBeforeReadingTheCounters: the claim
// transition refreshes relay_count from Redis before the claim, and the relays
// still in the batch must be in it -- otherwise the claim-time comparison of
// leaves against relays counted reads the batch's relays as missing.
func TestClaimTransition_FlushesTheBatchBeforeReadingTheCounters(t *testing.T) {
	client, _ := newTestRedis(t)
	const supplier, sessionID, n = "pokt1batch_claim", "sess-claim", 3
	w := newBatchWorker(t, client, supplier, "a")

	ids := w.publish(n)
	for i, id := range ids {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("claim-%d", i), 100))
	}
	require.Equal(t, n, w.held(sessionID), "premise: the relays are counted only when the batch flushes")

	cb := &countSeenCallback{seen: map[string]int64{}}
	m := &SessionLifecycleManager{
		logger:         zerolog.Nop(),
		sessionStore:   w.store,
		callback:       cb,
		config:         SessionLifecycleConfig{SupplierAddress: supplier},
		activeSessions: xsync.NewMap[string, *SessionSnapshot](),
	}
	m.SetPendingRelayFlusher(w.batch.FlushSessions)

	m.executeBatchedClaimTransition(w.ctx, []*SessionSnapshot{{SessionID: sessionID, State: SessionStateClaiming}})

	require.Equal(t, int64(n), cb.seen[sessionID],
		"the claim saw relay_count %d with %d relays in the tree", cb.seen[sessionID], n)
	require.Zero(t, w.pending())
}

// TestValidateRelayBatchFlushInterval pins the ceiling at exactly a quarter of
// the reclaim's idle timeout (Jorge, 2026-09-10: 15 s, "vamos al tope").
func TestValidateRelayBatchFlushInterval(t *testing.T) {
	require.NoError(t, validateRelayBatchFlushInterval(15*time.Second, 60*time.Second), "15 s with 60 s is the ceiling and passes")
	require.Error(t, validateRelayBatchFlushInterval(15*time.Second+time.Nanosecond, 60*time.Second), "one nanosecond over fails")
	require.Error(t, validateRelayBatchFlushInterval(15*time.Second, 40*time.Second), "a lowered idle timeout makes 15 s too long")
	require.Error(t, validateRelayBatchFlushInterval(0, 60*time.Second))

	cfg := &Config{}
	require.Equal(t, DefaultRelayBatchFlushInterval, cfg.GetRelayBatchFlushInterval())
	require.Equal(t, 15*time.Second, DefaultRelayBatchFlushInterval)
}

// TestLuaIsTerminalMatchesIsTerminal asks the Lua is_terminal and
// SessionState.IsTerminal() about EVERY SessionState constant, read from the
// source rather than listed here -- a list here would miss the state someone
// adds next, which is the state this test exists for. Both scripts are built by
// prefixing luaIsTerminal, so checking the fragment checks them both.
func TestLuaIsTerminalMatchesIsTerminal(t *testing.T) {
	client, _ := newTestRedis(t)

	file, err := parser.ParseFile(token.NewFileSet(), "session_store.go", nil, 0)
	require.NoError(t, err)
	var states []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if ident, ok := spec.Type.(*ast.Ident); !ok || ident.Name != "SessionState" {
			return true
		}
		for _, v := range spec.Values {
			if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				states = append(states, s)
			}
		}
		return true
	})
	// Control: the walk must have found the states we know exist, or an empty
	// list would pass this test by asking nothing.
	require.Contains(t, states, string(SessionStateActive))
	require.Contains(t, states, string(SessionStateProved))
	require.GreaterOrEqual(t, len(states), 12)

	for _, s := range states {
		got, err := client.Eval(context.Background(),
			luaIsTerminal+"\nreturn is_terminal(ARGV[1]) and 1 or 0", nil, s).Int64()
		require.NoError(t, err)
		require.Equalf(t, SessionState(s).IsTerminal(), got == 1,
			"state %q: Lua is_terminal and IsTerminal() disagree", s)
	}
}
