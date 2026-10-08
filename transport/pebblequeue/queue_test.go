package pebblequeue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	testPrefix   = "ha:relays"
	testSupplier = "pokt1supplier"
)

type fixture struct {
	t        *testing.T
	dir      string
	store    *pebblestore.Store
	broker   *Broker
	pub      *Publisher
	consumer *Consumer
	ch       <-chan transport.StreamMessage
	cancel   context.CancelFunc
}

func open(t *testing.T, dir string) *fixture {
	t.Helper()
	return openWith(t, dir, nil)
}

// openWith opens a fixture whose consumer setup configures before Consume.
func openWith(t *testing.T, dir string, setup func(*Consumer)) *fixture {
	t.Helper()
	store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: dir, SyncInterval: time.Hour})
	require.NoError(t, err)
	b := NewBroker(zerolog.Nop(), store, nil, testPrefix)
	c, err := b.Consumer(transport.ConsumerConfig{SupplierOperatorAddress: testSupplier})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{t: t, dir: dir, store: store, broker: b, pub: b.Publisher(0), consumer: c, cancel: cancel}
	if setup != nil {
		setup(c)
	}
	f.ch = c.Consume(ctx)
	t.Cleanup(f.close)
	return f
}

func (f *fixture) close() {
	f.cancel()
	_ = f.consumer.Close()
	_ = f.store.Close()
}

// restart closes everything and opens the same directory again: what a
// process restart sees.
func (f *fixture) restart() *fixture {
	f.close()
	return open(f.t, f.dir)
}

func relay(session string, n int) *transport.MinedRelayMessage {
	return &transport.MinedRelayMessage{
		SupplierOperatorAddress: testSupplier,
		ServiceId:               "svc",
		SessionId:               session,
		SessionEndHeight:        100,
		RelayBytes:              []byte(fmt.Sprintf("relay-%d", n)),
	}
}

func (f *fixture) publish(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		require.NoError(f.t, f.pub.Publish(context.Background(), relay("s1", i)))
	}
}

func (f *fixture) receive(n int) []transport.StreamMessage {
	f.t.Helper()
	out := make([]transport.StreamMessage, 0, n)
	for len(out) < n {
		select {
		case m, ok := <-f.ch:
			require.True(f.t, ok, "delivery stopped early")
			f.consumer.MarkDelivered(m)
			out = append(out, m)
		case <-time.After(5 * time.Second):
			f.t.Fatalf("received %d of %d", len(out), n)
		}
	}
	return out
}

// nothingDelivered is the control that delivery is idle: one entry published
// after the check arrives, and arrives first.
func (f *fixture) nothingDelivered() {
	f.t.Helper()
	require.NoError(f.t, f.pub.Publish(context.Background(), relay("probe", 999)))
	got := f.receive(1)
	require.Equal(f.t, "probe", got[0].Message.SessionId, "something else was still being delivered")
	require.NoError(f.t, f.consumer.AckMessage(context.Background(), got[0]))
}

func TestQueue_DeliversInOrderWithIncreasingIDs(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(5)

	got := f.receive(5)

	var prevMS, prevSeq uint64
	for i, m := range got {
		require.Equal(t, fmt.Sprintf("relay-%d", i), string(m.Message.RelayBytes), "in publish order, decoded")
		require.False(t, m.IsReclaim)
		require.Equal(t, f.consumer.StreamName(), m.StreamName)
		ms, seq, err := parseID(m.ID)
		require.NoError(t, err)
		if i > 0 {
			require.True(t, ms > prevMS || (ms == prevMS && seq > prevSeq), "ids increase: %s", m.ID)
		}
		prevMS, prevSeq = ms, seq
	}
	last, err := f.consumer.LastGeneratedID(context.Background())
	require.NoError(t, err)
	require.Equal(t, got[4].ID, last)
}

func TestQueue_AnAckedEntryIsNeverDeliveredAgain(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(2)
	got := f.receive(2)
	require.NoError(t, f.consumer.AckMessage(context.Background(), got[0]))

	f = f.restart()

	again := f.receive(1)
	require.Equal(t, got[1].ID, again[0].ID, "only the unacknowledged entry comes back")
	// Live, as XREADGROUP ">" delivers an entry no consumer holds: a reclaim
	// would not advance the claim gate's watermark, so a restart with a
	// backlog and no new traffic would hold every claim to its cap height.
	require.False(t, again[0].IsReclaim, "an entry found at startup is delivered live")
	f.nothingDelivered()
}

// A released entry waits for the release delay, as an XNACK'd entry waits for
// the sweep: retried at once, a relay the store refuses would spin.
func TestQueue_AReleasedEntryIsNotRedeliveredAtOnce(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(1)
	got := f.receive(1)

	require.NoError(t, f.consumer.ReleaseMessage(context.Background(), got[0]))

	f.nothingDelivered()
}

func TestQueue_AReleasedEntryIsDueAfterTheDelayAsAReclaim(t *testing.T) {
	store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	b := NewBroker(zerolog.Nop(), store, nil, testPrefix)
	c, err := b.Consumer(transport.ConsumerConfig{SupplierOperatorAddress: testSupplier, ClaimIdleTimeout: 1000})
	require.NoError(t, err)
	require.NoError(t, b.Publisher(0).Publish(context.Background(), relay("s1", 0)))
	now := time.Now()
	first, _, err := c.nextBatch(now)
	require.NoError(t, err)
	require.Len(t, first, 1)

	released := time.Now()
	require.NoError(t, c.ReleaseMessage(context.Background(), first[0]))

	early, due, err := c.nextBatch(released)
	require.NoError(t, err)
	require.Empty(t, early)
	require.False(t, due.Before(released.Add(time.Second)), "due one release delay later")
	late, due, err := c.nextBatch(due)
	require.NoError(t, err)
	require.Len(t, late, 1)
	require.Equal(t, first[0].ID, late[0].ID)
	require.True(t, late[0].IsReclaim)
	require.True(t, due.IsZero(), "nothing else released")
}

// gate is a store health signal a test opens and closes.
type gate struct {
	mu       sync.Mutex
	operable bool
	changed  chan struct{}
}

func (g *gate) Operable() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.operable
}

func (g *gate) Changed() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.changed
}

func (g *gate) set(operable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.operable = operable
	close(g.changed)
	g.changed = make(chan struct{})
}

// Nothing is delivered while the store cannot take what the miner writes for
// a relay, as the Redis consumer does not read then.
func TestQueue_NothingIsDeliveredWhileTheStoreIsNotOperable(t *testing.T) {
	g := &gate{changed: make(chan struct{})}
	f := openWith(t, t.TempDir(), func(c *Consumer) { c.SetStoreHealth(g) })
	f.publish(1)

	select {
	case m := <-f.ch:
		t.Fatalf("delivered %s while the store was not operable", m.ID)
	case <-time.After(100 * time.Millisecond):
	}
	g.set(true)

	got := f.receive(1)
	require.Equal(t, "relay-0", string(got[0].Message.RelayBytes))
}

func TestQueue_EachOwnPendingVisitsWhatIsDeliveredAndNotAcked(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(3)
	got := f.receive(3)
	require.NoError(t, f.consumer.AckMessage(context.Background(), got[1]))
	f.consumer.Stop()

	var visited []string
	require.NoError(t, f.consumer.EachOwnPending(context.Background(), func(m transport.StreamMessage) {
		require.True(t, m.IsReclaim)
		visited = append(visited, m.ID)
	}))
	require.Equal(t, []string{got[0].ID, got[2].ID}, visited, "oldest first, the acked one left out")
}

func TestQueue_AckInBatchAcknowledgesWithTheCommit(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(2)
	got := f.receive(2)

	b := f.store.DB().NewBatch()
	f.consumer.AckInBatch(b, []string{got[0].ID, got[1].ID})
	require.NoError(t, f.store.Commit(b))
	f.consumer.Acked([]string{got[0].ID, got[1].ID})

	for _, m := range got {
		exists, err := f.consumer.Exists(m.ID)
		require.NoError(t, err)
		require.False(t, exists)
	}
	f.consumer.Stop()
	visited := 0
	require.NoError(t, f.consumer.EachOwnPending(context.Background(), func(transport.StreamMessage) { visited++ }))
	require.Zero(t, visited)
}

// IDs keep increasing across a restart even when every entry was acknowledged:
// the claim gate compares them, and an ID going back would read as "handled".
func TestQueue_IDsKeepIncreasingAcrossARestartWithAnEmptyQueue(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(1)
	first := f.receive(1)[0]
	require.NoError(t, f.consumer.AckMessage(context.Background(), first))
	// A clock that has not moved past the last ID must still give a larger one.
	f.broker.mu.Lock()
	st := f.broker.streams[f.consumer.StreamName()]
	st.lastMS += uint64(time.Hour.Milliseconds())
	f.broker.mu.Unlock()
	f.publish(1)
	second := f.receive(1)[0]
	require.NoError(t, f.consumer.AckMessage(context.Background(), second))

	f = f.restart()
	f.publish(1)
	third := f.receive(1)[0]

	sMS, sSeq, _ := parseID(second.ID)
	tMS, tSeq, _ := parseID(third.ID)
	require.True(t, tMS > sMS || (tMS == sMS && tSeq > sSeq), "%s after %s", third.ID, second.ID)
}

func TestQueue_TrimDeletesOldEntries(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(3)
	f.receive(3)

	trimmed, err := f.consumer.TrimStream(context.Background(), -time.Second)

	require.NoError(t, err)
	require.Equal(t, int64(3), trimmed)
	f.consumer.Stop()
	visited := 0
	require.NoError(t, f.consumer.EachOwnPending(context.Background(), func(transport.StreamMessage) { visited++ }))
	require.Zero(t, visited)
}

// A charge is written with the relay it was served with, and the meter reads
// the counter's new value back.
func TestQueue_ChargesAreWrittenWithThePublish(t *testing.T) {
	f := open(t, t.TempDir())
	ledger := redistransport.NewChargeLedger()
	var written []int64
	ledger.OnWritten(func(key string, amount, consumed int64) {
		written = append(written, consumed)
		ledger.FinishWrite(key, amount)
	})
	f.pub.SetChargeLedger(ledger)

	ledger.Add("meter:s1:sup", testSupplier, 40, time.Hour)
	f.publish(1)
	ledger.Add("meter:s1:sup", testSupplier, 2, time.Hour)
	f.publish(1)

	require.Equal(t, []int64{40, 42}, written)
	require.Zero(t, ledger.Pending("meter:s1:sup"))
	f = f.restart()
	store := kv.NewPebble(zerolog.Nop(), f.store, redistransport.NewKeyBuilder(config.RedisNamespaceConfig{}))
	defer func() { _ = store.Close() }()
	consumed, err := store.Get(context.Background(), "meter:s1:sup")
	require.NoError(t, err)
	require.Equal(t, "42", string(consumed), "the counter survives a restart, where the meter reads it")
}

func TestQueue_AClosedPublisherRefuses(t *testing.T) {
	f := open(t, t.TempDir())
	require.NoError(t, f.pub.Close())
	require.ErrorIs(t, f.pub.Publish(context.Background(), relay("s1", 0)), ErrClosed)
}

func TestQueue_AnInvalidRelayIsRefusedBeforeTheQueue(t *testing.T) {
	f := open(t, t.TempDir())
	bad := relay("", 0)
	require.Error(t, f.pub.Publish(context.Background(), bad))
	f.nothingDelivered()
}

// chargeFixture is a queue whose publisher writes charges for the relay meter.
func chargeFixture(t *testing.T, interval time.Duration) (*fixture, *kv.Pebble, *redistransport.ChargeLedger, chan int64) {
	t.Helper()
	f := open(t, t.TempDir())
	counters := kv.NewPebble(zerolog.Nop(), f.store, redistransport.NewKeyBuilder(config.RedisNamespaceConfig{}))
	t.Cleanup(func() { _ = counters.Close() })
	f.broker.counters = counters
	f.pub = f.broker.Publisher(interval)
	ledger := redistransport.NewChargeLedger()
	written := make(chan int64, 16)
	ledger.OnWritten(func(key string, amount, consumed int64) {
		ledger.FinishWrite(key, amount)
		written <- consumed
	})
	f.pub.SetChargeLedger(ledger)
	return f, counters, ledger, written
}

// A relay served and not mined still spends its budget: its charge is written
// on the next tick, with no publish to carry it.
func TestQueue_ChargesWithNoPublishAreWrittenOnTheTick(t *testing.T) {
	_, counters, ledger, written := chargeFixture(t, 10*time.Millisecond)

	ledger.Add("meter:s1:sup", testSupplier, 7, time.Hour)

	select {
	case consumed := <-written:
		require.Equal(t, int64(7), consumed)
	case <-time.After(5 * time.Second):
		t.Fatal("the charge was never written")
	}
	value, err := counters.Get(context.Background(), "meter:s1:sup")
	require.NoError(t, err)
	require.Equal(t, "7", string(value))
}

// Close writes the charges no publish carried, as the batcher's final flush
// does: a graceful restart must read them.
func TestQueue_CloseWritesTheChargesLeft(t *testing.T) {
	f, _, ledger, _ := chargeFixture(t, 0)
	ledger.Add("meter:s1:sup", testSupplier, 9, time.Hour)

	require.NoError(t, f.pub.Close())

	f = f.restart()
	counters := kv.NewPebble(zerolog.Nop(), f.store, redistransport.NewKeyBuilder(config.RedisNamespaceConfig{}))
	defer func() { _ = counters.Close() }()
	value, err := counters.Get(context.Background(), "meter:s1:sup")
	require.NoError(t, err)
	require.Equal(t, "9", string(value))
}

// The meter's Del of a counter (a session cleared) cannot land between the
// counter's read and its rewrite: if it did, the rewrite would bring back the
// value the Del removed. A Del after the rewrite leaves no counter, as DEL
// after INCRBY does on Redis.
func TestQueue_AClearedCounterDoesNotComeBack(t *testing.T) {
	f, counters, ledger, _ := chargeFixture(t, 0)
	ctx := context.Background()
	ledger.Add("meter:s1:sup", testSupplier, 100, time.Hour)
	f.publish(1)

	deleted := make(chan struct{})
	f.broker.mu.Lock()
	f.broker.afterCountersRead = func() {
		f.broker.afterCountersRead = nil
		go func() {
			_ = counters.Del(ctx, "meter:s1:sup")
			close(deleted)
		}()
		// Give an unserialized Del the time to land before the write.
		select {
		case <-deleted:
		case <-time.After(100 * time.Millisecond):
		}
	}
	f.broker.mu.Unlock()
	ledger.Add("meter:s1:sup", testSupplier, 5, time.Hour)
	f.publish(1)
	<-deleted

	_, err := counters.Get(ctx, "meter:s1:sup")
	require.ErrorIs(t, err, kv.ErrNotFound, "the Del ran after the rewrite, so nothing is left")
}

// A read of the queue that fails is tried again on its own: the entries it
// left unread may have no later publish to wake delivery.
func TestQueue_AFailedReadIsRetriedWithoutANewPublish(t *testing.T) {
	var reads atomic.Int32
	f := openWith(t, t.TempDir(), func(c *Consumer) {
		require.NoError(t, c.b.Publisher(0).Publish(context.Background(), relay("s1", 0)))
		// The publish's own wake-up is spent here: what is left to wake
		// delivery after the failed read is the retry alone.
		select {
		case <-c.st.notify:
		default:
		}
		c.failRead = func() error {
			if reads.Add(1) == 1 {
				return errors.New("injected read failure")
			}
			return nil
		}
	})

	got := f.receive(1)

	require.Len(t, got, 1)
	require.GreaterOrEqual(t, reads.Load(), int32(2), "premise: the first read failed")
}
