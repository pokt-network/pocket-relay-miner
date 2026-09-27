//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// With a claimer -- every manager past Start -- an operator removing a signing
// key reaches the teardown through Release(triggerKeyRemoval) and
// onSupplierReleased. That callback mapped every trigger but shutdown to a
// rebalance, so drainKeyRemoved was reachable only with no claimer at all: the
// relays held by the batch and the delivery buffer were released to a fleet
// that cannot sign them, and relays_dropped_no_key never counted.

// keyRemovalFixture is one supplier with two relays in its batch and two in its
// delivery buffer, and a stand-in for its consume loop holding state.wg. On
// cancellation the stand-in runs what consumeForSupplier's ctx.Done branch runs,
// in the same order, so the teardown reaches the real exit code.
type keyRemovalFixture struct {
	w     *batchWorker
	total int
}

func newKeyRemovalFixture(t *testing.T, supplier string) *keyRemovalFixture {
	t.Helper()
	client, _ := newTestRedis(t)
	w := newBatchWorker(t, client, supplier, "a")
	w.mgr.config.RedisClient = client
	w.mgr.config.MinerID = "instance-key-removal"

	const sessionID = "sess-key-removal"
	ids := w.publish(4)
	for i, id := range ids[:2] {
		w.deliver(w.msg(id, sessionID, fmt.Sprintf("batched-%d", i), 100))
	}
	require.Equal(t, 2, w.held(sessionID), "premise: two relays wait in the batch")

	buffer := make(chan transport.StreamMessage, 2)
	for i, id := range ids[2:] {
		buffer <- w.msg(id, sessionID, fmt.Sprintf("buffered-%d", i), 100)
	}
	close(buffer)

	ctx, cancel := context.WithCancel(w.ctx)
	w.state.cancelFn = cancel
	w.state.wg.Add(1)
	go func() {
		defer w.state.wg.Done()
		<-ctx.Done()
		w.mgr.releaseRelayBatchOnExit(ctx, w.state)
		w.mgr.drainDeliveryBuffer(ctx, w.state, buffer)
	}()

	require.Equal(t, int64(4), w.pending(), "premise: all four are delivered and unacknowledged")
	return &keyRemovalFixture{w: w, total: 4}
}

// claimWith gives the manager a claimer that holds the supplier's lease and a
// key manager holding the given keys, as after Start.
func (f *keyRemovalFixture) claimWith(t *testing.T, keysHeld ...string) {
	t.Helper()
	claimer := NewSupplierClaimer(zerolog.Nop(), f.w.client, f.w.mgr.config.MinerID, SupplierClaimerConfig{})
	claimer.SetCallbacks(func(context.Context, string) error { return nil }, f.w.mgr.onSupplierReleased)
	require.True(t, claimer.TryClaim(f.w.ctx, f.w.supplier), "premise: this instance holds the lease")
	f.w.mgr.claimer = claimer
	f.w.mgr.keyManager = &fakeKeyManager{addrs: keysHeld}
}

func (f *keyRemovalFixture) release(t *testing.T, trigger string) {
	t.Helper()
	require.NoError(t, f.w.mgr.onSupplierReleased(context.Background(), f.w.supplier, trigger))
	f.w.mgr.waitDrains()
}

func TestOnSupplierReleased_KeyRemovalAcksTheBatchAndTheBufferAsLost(t *testing.T) {
	f := newKeyRemovalFixture(t, "pokt1keyremoval_lost")
	noKey := relaysDroppedNoKey.WithLabelValues(f.w.supplier, "svc-1")
	before := testutil.ToFloat64(noKey)

	f.release(t, triggerKeyRemoval)

	require.Zero(t, f.w.pending(),
		"a removed key: batch and buffer are acknowledged, not released to a fleet that cannot sign them")
	require.Zero(t, f.w.streamLen())
	require.Equal(t, before+float64(f.total), testutil.ToFloat64(noKey), "each one counted as dropped for want of a key")
}

// The control: any other release hands the same four back for a peer to finish.
func TestOnSupplierReleased_RebalanceReleasesTheBatchAndTheBuffer(t *testing.T) {
	f := newKeyRemovalFixture(t, "pokt1keyremoval_control")
	noKey := relaysDroppedNoKey.WithLabelValues(f.w.supplier, "svc-1")
	before := testutil.ToFloat64(noKey)

	f.release(t, triggerRebalanceRelease)

	f.requireReleasedToPeer(t)
	require.Equal(t, before, testutil.ToFloat64(noKey))
}

// requireReleasedToPeer: all of the fixture's entries are still in the stream
// and another consumer can take them now.
func (f *keyRemovalFixture) requireReleasedToPeer(t *testing.T) {
	t.Helper()
	require.Equal(t, int64(f.total), f.w.streamLen(), "released, not acknowledged: still in the stream")
	claimed, _, err := f.w.client.XAutoClaimJustID(f.w.ctx, &redis.XAutoClaimArgs{
		Stream: f.w.stream, Group: f.w.group, Consumer: "peer", MinIdle: 30 * time.Second, Start: "0", Count: 10,
	}).Result()
	require.NoError(t, err)
	require.Len(t, claimed, f.total, "and a peer can take all of them now")
}

// The reconcile drops a claimed supplier the staking filter no longer returns.
// With its key gone -- the retry of a key-change release that kept its claim --
// it is a key removal too.
func TestReleaseUnconfigured_AKeylessSupplierIsTornDownAsAKeyRemoval(t *testing.T) {
	f := newKeyRemovalFixture(t, "pokt1unconfigured_keyless")
	f.claimWith(t /* no key held */)
	noKey := relaysDroppedNoKey.WithLabelValues(f.w.supplier, "svc-1")
	before := testutil.ToFloat64(noKey)

	f.w.mgr.releaseUnconfigured(f.w.ctx, nil)
	f.w.mgr.waitDrains()

	require.Zero(t, f.w.pending(), "no key: acknowledged as lost, not released to a fleet that cannot sign them")
	require.Equal(t, before+float64(f.total), testutil.ToFloat64(noKey))
}

// The control: unstaked, but the key is still here, so the work is handed over.
func TestReleaseUnconfigured_AnUnstakedSupplierWithItsKeyIsHandedOver(t *testing.T) {
	f := newKeyRemovalFixture(t, "pokt1unconfigured_keyed")
	f.claimWith(t, f.w.supplier)
	noKey := relaysDroppedNoKey.WithLabelValues(f.w.supplier, "svc-1")
	before := testutil.ToFloat64(noKey)

	f.w.mgr.releaseUnconfigured(f.w.ctx, nil)
	f.w.mgr.waitDrains()

	f.requireReleasedToPeer(t)
	require.Equal(t, before, testutil.ToFloat64(noKey))
}
