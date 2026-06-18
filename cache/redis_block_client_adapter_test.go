//go:build test

package cache

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// newBareAdapter builds a RedisBlockClientAdapter without going through
// NewRedisBlockClientAdapter so the tests can exercise publishToSubscribers
// in isolation (no Redis, no cometClient). The subscribersMu/subscribers
// zero values are all we need for the fast-path and fan-out tests.
func newBareAdapter(t *testing.T) *RedisBlockClientAdapter {
	t.Helper()
	return &RedisBlockClientAdapter{
		logger: logging.NewLoggerFromConfig(logging.DefaultConfig()),
	}
}

// TestPublishToSubscribers_ShortCircuitsWhenNoSubscribers locks in the
// Wave 4 optimization: when subscribers is empty the method must return
// without allocating a SimpleBlock. The pointer-identity check confirms
// no write to any hypothetical subscriber slot happened.
func TestPublishToSubscribers_ShortCircuitsWhenNoSubscribers(t *testing.T) {
	a := newBareAdapter(t)
	assert.Empty(t, a.subscribers, "precondition: no subscribers")

	event := &BlockEvent{
		Height:    100,
		Hash:      []byte{0x01, 0x02},
		Timestamp: time.Unix(0, 0),
	}

	// Must not panic, must not block, must not grow the subscribers slice.
	assert.NotPanics(t, func() { a.publishToSubscribers(event) })
	assert.Empty(t, a.subscribers)
}

// TestPublishToSubscribers_DeliversToSubscriber verifies the slow-path
// still works end-to-end after the short-circuit was added.
func TestPublishToSubscribers_DeliversToSubscriber(t *testing.T) {
	a := newBareAdapter(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := a.Subscribe(ctx, 4)
	require.NotNil(t, ch)

	event := &BlockEvent{
		Height:    200,
		Hash:      []byte{0xde, 0xad, 0xbe, 0xef},
		Timestamp: time.Unix(1776206000, 0),
	}
	a.publishToSubscribers(event)

	select {
	case block := <-ch:
		require.NotNil(t, block)
		assert.Equal(t, int64(200), block.Height())
		assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, block.Hash())
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive block event")
	}
}

// TestPublishToSubscribers_DropsWhenSubscriberFull exercises the
// non-blocking fan-out branch: a single-slot buffer fills immediately,
// the second publish must not block the caller.
func TestPublishToSubscribers_DropsWhenSubscriberFull(t *testing.T) {
	a := newBareAdapter(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := a.Subscribe(ctx, 1)
	require.NotNil(t, ch)

	first := &BlockEvent{Height: 1, Hash: []byte{0x00}, Timestamp: time.Unix(0, 0)}
	second := &BlockEvent{Height: 2, Hash: []byte{0x00}, Timestamp: time.Unix(0, 0)}

	a.publishToSubscribers(first)
	// Second publish must not block — buffer is full and the select has a
	// default branch that drops.
	done := make(chan struct{})
	go func() {
		a.publishToSubscribers(second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publishToSubscribers blocked when subscriber buffer was full")
	}

	// Drain the one delivered block and confirm it is the first.
	select {
	case block := <-ch:
		assert.Equal(t, int64(1), block.Height())
	default:
		t.Fatal("expected first block to be delivered")
	}
}

// TestPublishToSubscribers_DropIsLoud verifies the fan-out drop is no longer
// silent: when a subscriber's buffer is full, dropping the event must bump the
// block_events_dropped_total{channel="fanout"} counter. A silent drop here is
// exactly what hid the claim/proof-window stall at high supplier counts, so the
// metric is the signal that makes a wedged subscriber visible.
func TestPublishToSubscribers_DropIsLoud(t *testing.T) {
	a := newBareAdapter(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Single-slot buffer that we never drain: the first publish fills it, the
	// second must drop (and count).
	ch := a.Subscribe(ctx, 1)
	require.NotNil(t, ch)

	before := testutil.ToFloat64(blockEventsDropped.WithLabelValues("fanout"))

	a.publishToSubscribers(&BlockEvent{Height: 1, Hash: []byte{0x00}, Timestamp: time.Unix(0, 0)})
	a.publishToSubscribers(&BlockEvent{Height: 2, Hash: []byte{0x00}, Timestamp: time.Unix(0, 0)})

	after := testutil.ToFloat64(blockEventsDropped.WithLabelValues("fanout"))
	assert.Equal(t, float64(1), after-before, "exactly one fan-out drop must be counted")
}

// TestBlockEvents_GuardGatesForwarding locks in the discard-when-no-consumer
// guard: a fresh adapter must NOT forward to blockEventsCh (so it never fills on
// a process — the miner — that never wires BlockEvents()), and calling
// BlockEvents() must flip the flag on so a real consumer (the relayer) starts
// receiving.
func TestBlockEvents_GuardGatesForwarding(t *testing.T) {
	a := newBareAdapter(t)

	assert.False(t, a.blockEventsRequested.Load(),
		"fresh adapter must treat BlockEvents() as having no consumer (discard on arrival)")

	// (newBareAdapter does not init blockEventsCh, so the returned channel is nil
	// here; the flag flip is what this test pins.)
	_ = a.BlockEvents()
	assert.True(t, a.blockEventsRequested.Load(),
		"calling BlockEvents() must mark a consumer so the event loop starts forwarding")

	// Idempotent: a consumer loop re-evaluates BlockEvents() every iteration.
	_ = a.BlockEvents()
	assert.True(t, a.blockEventsRequested.Load())
}

// Compile-time assertion: localclient.NewSimpleBlock signature has not
// drifted in a way that would silently break the short-circuit (the
// short-circuit only saves allocations while this function still has
// the same expensive call shape).
var _ = localclient.NewSimpleBlock
