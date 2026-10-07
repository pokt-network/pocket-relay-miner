package pebblequeue

import (
	"context"
	"fmt"
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
	store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: dir, SyncInterval: time.Hour})
	require.NoError(t, err)
	b := NewBroker(zerolog.Nop(), store, testPrefix)
	c, err := b.Consumer(transport.ConsumerConfig{SupplierOperatorAddress: testSupplier})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{t: t, dir: dir, store: store, broker: b, pub: b.Publisher(), consumer: c, cancel: cancel}
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
	require.True(t, again[0].IsReclaim, "an entry from a previous run is a reclaim")
	f.nothingDelivered()
}

func TestQueue_ReleaseRedeliversAsAReclaim(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(1)
	got := f.receive(1)

	require.NoError(t, f.consumer.ReleaseMessage(context.Background(), got[0]))

	again := f.receive(1)
	require.Equal(t, got[0].ID, again[0].ID)
	require.True(t, again[0].IsReclaim)
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
