//go:build test

package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// startLoopAdapter runs a real adapter event loop fed by a real subscriber, with
// no Redis: handleBlockEvent is where the subscriber hands a pub/sub message to
// its subscribers, so calling it drives the adapter exactly as a published block
// would. The returned channel is a fan-out subscription, which is how the
// miner's lifecycle manager hears about blocks.
func startLoopAdapter(t *testing.T) (*RedisBlockClientAdapter, *RedisBlockSubscriber, <-chan *localclient.SimpleBlock) {
	t.Helper()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	sub := NewRedisBlockSubscriber(logger, nil, nil)
	a := NewRedisBlockClientAdapter(logger, sub, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, a.Start(ctx))
	blocks := a.Subscribe(ctx, 16)
	t.Cleanup(func() {
		cancel()
		a.Close()
	})
	return a, sub, blocks
}

func requireNextBlock(t *testing.T, blocks <-chan *localclient.SimpleBlock, height int64) {
	t.Helper()
	select {
	case b := <-blocks:
		require.Equal(t, height, b.Height(), "the next block the adapter fanned out")
	case <-time.After(5 * time.Second):
		t.Fatalf("no block fanned out; expected height %d", height)
	}
}

// TestAdapter_ALowerHeightAfterAHigherOneIsIgnored: the miner judges whether a
// relay's claim window has opened against this height, so taking a late lower
// event as current would admit relays for a window it already saw open.
func TestAdapter_ALowerHeightAfterAHigherOneIsIgnored(t *testing.T) {
	a, sub, blocks := startLoopAdapter(t)
	rewoundBefore := testutil.ToFloat64(blockEventsIgnored.WithLabelValues(blockIgnoredRewound))

	sub.handleBlockEvent(BlockEvent{Height: 120, Hash: []byte("h120")})
	requireNextBlock(t, blocks, 120)

	sub.handleBlockEvent(BlockEvent{Height: 105, Hash: []byte("h105")})
	// A later event is the point where the loop has certainly handled the lower
	// one, and it must be the next thing fanned out.
	sub.handleBlockEvent(BlockEvent{Height: 121, Hash: []byte("h121")})
	requireNextBlock(t, blocks, 121)

	require.Equal(t, rewoundBefore+1, testutil.ToFloat64(blockEventsIgnored.WithLabelValues(blockIgnoredRewound)),
		"the rewound event must be counted as rewound")
	require.Equal(t, int64(121), a.LastBlock(context.Background()).Height())
	require.Equal(t, []byte("h121"), a.LastBlock(context.Background()).Hash())
}

// TestAdapter_TheSameHeightAgainIsIgnored: a repeated delivery neither moves the
// height nor reaches the subscribers a second time.
func TestAdapter_TheSameHeightAgainIsIgnored(t *testing.T) {
	a, sub, blocks := startLoopAdapter(t)
	repeatedBefore := testutil.ToFloat64(blockEventsIgnored.WithLabelValues(blockIgnoredRepeated))

	sub.handleBlockEvent(BlockEvent{Height: 120, Hash: []byte("h120")})
	requireNextBlock(t, blocks, 120)
	sub.handleBlockEvent(BlockEvent{Height: 120, Hash: []byte("other")})
	sub.handleBlockEvent(BlockEvent{Height: 121})
	requireNextBlock(t, blocks, 121)

	require.Equal(t, repeatedBefore+1, testutil.ToFloat64(blockEventsIgnored.WithLabelValues(blockIgnoredRepeated)))
	require.Equal(t, int64(121), a.LastBlock(context.Background()).Height())
}

// TestAdapter_AJumpForwardIsTaken: switching from a lagging node to a synced one
// looks like a jump of any size, and refusing it would pin the miner to a stale
// height, admitting relays whose claim window has opened.
func TestAdapter_AJumpForwardIsTaken(t *testing.T) {
	a, sub, blocks := startLoopAdapter(t)
	const far = int64(120 + 1_000_000)

	sub.handleBlockEvent(BlockEvent{Height: 120})
	requireNextBlock(t, blocks, 120)
	sub.handleBlockEvent(BlockEvent{Height: far})
	requireNextBlock(t, blocks, far)

	require.Equal(t, far, a.LastBlock(context.Background()).Height())
}

// TestAdapter_SeedHeightFollowsTheSameRule: the startup seed moves an empty
// adapter, is not fanned out, is not counted as an ignored event, and a seed
// below what an event already brought moves nothing.
func TestAdapter_SeedHeightFollowsTheSameRule(t *testing.T) {
	a, sub, blocks := startLoopAdapter(t)
	ctx := context.Background()
	rewoundBefore := testutil.ToFloat64(blockEventsIgnored.WithLabelValues(blockIgnoredRewound))

	require.Equal(t, int64(0), a.LastBlock(ctx).Height(), "premise: nothing received yet")
	require.True(t, a.SeedHeight(500), "a seed on an empty adapter moves it")
	require.Equal(t, int64(500), a.LastBlock(ctx).Height())

	sub.handleBlockEvent(BlockEvent{Height: 400})
	sub.handleBlockEvent(BlockEvent{Height: 600})
	requireNextBlock(t, blocks, 600) // neither the seed nor the event below it was fanned out

	require.False(t, a.SeedHeight(550), "a seed below the height an event brought must not move it")
	require.Equal(t, int64(600), a.LastBlock(ctx).Height())
	require.Equal(t, rewoundBefore+1, testutil.ToFloat64(blockEventsIgnored.WithLabelValues(blockIgnoredRewound)),
		"only the event below the seed counts; a refused seed is not a block event")
}

// TestAdvance_AHigherHeightStoredMidWriteIsNotLost: the startup seed and the
// event loop write concurrently. When a higher height lands between one
// writer's read and its replace, the replace must notice and keep the higher
// one; a plain store there would put the lower height back.
func TestAdvance_AHigherHeightStoredMidWriteIsNotLost(t *testing.T) {
	a := newBareAdapter(t)
	require.True(t, a.SeedHeight(100))

	interleaved := false
	a.afterLoadHook = func() {
		if interleaved {
			return
		}
		interleaved = true
		// Another writer, landing in the window this writer has just opened.
		require.True(t, a.SeedHeight(200), "premise: the interleaved writer moves the height")
	}

	a.advance(150, nil)

	require.True(t, interleaved, "premise: the hook ran inside the write")
	require.Equal(t, int64(200), a.LastBlock(context.Background()).Height(),
		"the higher height written mid-write must survive the lower write that started before it")
}

// TestAdvance_ConcurrentWritersKeepTheHighest: the seed and the event loop write
// concurrently. Whatever the interleaving, the height held at the end is the
// highest one written.
func TestAdvance_ConcurrentWritersKeepTheHighest(t *testing.T) {
	a := newBareAdapter(t)
	// Many more writers than cores and many writes each: a load-then-store loses
	// the higher height only when a store lands between another writer's load and
	// store, so the test needs that window to be hit, not merely possible.
	const writers, perWriter = 32, 20000

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				// Each writer walks its own residue class, so every height is
				// written once and writers constantly overtake each other.
				a.advance(int64(i*writers+w+1), nil)
			}
		}(w)
	}
	wg.Wait()

	require.Equal(t, int64(writers*perWriter), a.LastBlock(context.Background()).Height())
}
