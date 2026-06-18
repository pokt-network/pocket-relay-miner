//go:build test

package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/stretchr/testify/require"
)

// blk is a tiny SimpleBlock constructor for tests.
func blk(height int64) *localclient.SimpleBlock {
	return localclient.NewSimpleBlock(height, nil, time.Time{})
}

// dropNewestSend mirrors the production fan-out publisher
// (cache.RedisBlockClientAdapter.publishToSubscribers): a non-blocking send that
// drops the NEWEST event when the subscriber channel is full. This is the
// behaviour that, paired with a slow consumer, strands the consumer on stale
// heights.
func dropNewestSend(ch chan *localclient.SimpleBlock, b *localclient.SimpleBlock) {
	select {
	case ch <- b:
	default: // full — drop (newest)
	}
}

// naiveBlockLoop reproduces the PRE-FIX consumer: it reads a block and runs the
// (slow) per-height work synchronously in the same loop, so it cannot read the
// next block until the current one finishes processing.
func naiveBlockLoop(ctx context.Context, ch <-chan *localclient.SimpleBlock, onHeight func(int64)) {
	last := int64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			h := b.Height()
			if h <= last {
				continue
			}
			last = h
			onHeight(h)
		}
	}
}

// REPRODUCES THE BUG. With a slow consumer and a drop-newest producer, the naive
// synchronous loop is stranded on stale heights: while it is busy processing an
// early block every later block is dropped, so it never advances to the chain
// head — exactly the production failure where claim/proof windows stopped firing
// at ~594 suppliers. This test asserts the broken behaviour so the fix below has
// something concrete to beat.
func TestNaiveBlockLoop_StrandedOnStaleHead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *localclient.SimpleBlock, 2) // small buffer, like the real fan-out cushion
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)

	var mu sync.Mutex
	var processed []int64
	onHeight := func(h int64) {
		mu.Lock()
		processed = append(processed, h)
		mu.Unlock()
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate // simulate a processing pass slower than block arrival
	}

	go naiveBlockLoop(ctx, ch, onHeight)

	// Block 1 arrives and the consumer starts (slowly) processing it.
	ch <- blk(1)
	<-entered // consumer is now stuck inside onHeight(1)

	// The chain races ahead while the consumer is busy. Buffer (2) keeps 2 and 3;
	// every later height is dropped (newest-dropped).
	for h := int64(2); h <= 100; h++ {
		dropNewestSend(ch, blk(h))
	}
	close(gate) // let processing proceed at full speed

	// The naive loop can only ever process what survived in the buffer — it is
	// permanently behind the head (100) and never recovers.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(processed) == 3
	}, 2*time.Second, 5*time.Millisecond, "naive loop should drain only the buffered stale heights")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int64{1, 2, 3}, processed, "naive loop is stranded on stale heights")
	require.NotContains(t, processed, int64(100), "naive loop never reaches the chain head — the bug")
}

// VALIDATES THE FIX. Same slow consumer, same drop-newest producer: the
// coalescing loop's reader drains the channel on arrival (so nothing is dropped)
// and the processor always advances to the LATEST height. It coalesces the
// intermediate ticks but reaches the head — recovering the windows the naive
// loop lost.
func TestCoalescingBlockLoop_ReachesHeadUnderSlowProcessor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *localclient.SimpleBlock, 2)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)

	var mu sync.Mutex
	var processed []int64
	onHeight := func(h int64) {
		mu.Lock()
		processed = append(processed, h)
		mu.Unlock()
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
	}

	go runCoalescingBlockLoop(ctx, ch, onHeight)

	ch <- blk(1)
	<-entered // processor is stuck inside onHeight(1); the READER keeps draining

	// Feed the rest while the processor is blocked. Pace each emit until the
	// reader has drained it (channel empties) so the producer never needs to drop
	// — proving the reader keeps the channel clear independent of processor speed.
	for h := int64(2); h <= 100; h++ {
		dropNewestSend(ch, blk(h))
		require.Eventually(t, func() bool { return len(ch) == 0 }, time.Second, time.Millisecond,
			"reader must drain the channel on arrival")
	}
	close(gate)

	// The processor advances to the chain head (100), unlike the naive loop.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(processed) > 0 && processed[len(processed)-1] == 100
	}, 2*time.Second, 5*time.Millisecond, "coalescing loop must reach the chain head")

	mu.Lock()
	defer mu.Unlock()
	// Heights are strictly increasing (never reprocessed, never backwards) and
	// intermediate ticks were coalesced (far fewer than 100 passes).
	for i := 1; i < len(processed); i++ {
		require.Greater(t, processed[i], processed[i-1], "heights must be strictly increasing")
	}
	require.Less(t, len(processed), 100, "redundant ticks must be coalesced, not processed one-by-one")
}

// The reader must never block the producer, even when the processor is wedged
// forever: a burst far larger than the buffer is accepted without the producer
// stalling, and the loop still tracks the latest height.
func TestCoalescingBlockLoop_NeverBlocksProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *localclient.SimpleBlock, 4)
	wedge := make(chan struct{}) // never closed: the processor is stuck on the first pass

	var seen int64
	go runCoalescingBlockLoop(ctx, ch, func(h int64) {
		storeIfGreater(&seen, h)
		<-wedge
	})

	// A large burst (>> buffer). Each send must complete promptly because the
	// reader drains on arrival; if the reader ever blocked, this would deadlock
	// and the test would time out.
	const burst = 5000
	done := make(chan struct{})
	go func() {
		for h := int64(1); h <= burst; h++ {
			ch <- blk(h) // blocking send — relies on the reader draining
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked — the reader is not draining the channel")
	}
}

// VALIDATES THE CLOSE-PATH FIX. When the block channel closes (the source ended,
// not a ctx cancel) with a newer height already recorded, the processor must run
// onHeight for that final height before returning — not pseudo-randomly pick the
// readerDone case over a pending wake and drop it. Without the fix the last block
// (e.g. the height that opens a claim window) could be silently skipped on source
// shutdown, the exact "drops the final event" gap the loop's contract forbids.
func TestCoalescingBlockLoop_ProcessesFinalHeightOnClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *localclient.SimpleBlock, 64)

	var mu sync.Mutex
	var processed []int64
	done := make(chan struct{})
	go func() {
		runCoalescingBlockLoop(ctx, ch, func(h int64) {
			mu.Lock()
			processed = append(processed, h)
			mu.Unlock()
		})
		close(done)
	}()

	// Feed a burst, then close the channel WITHOUT cancelling ctx (the block
	// source ended). latest is recorded as 100 before the close is observed.
	for h := int64(1); h <= 100; h++ {
		ch <- blk(h)
	}
	close(ch)

	// The loop must exit on the close and must have processed up to the final
	// height — the last block is never lost.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit after channel close")
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, processed, "loop must process at least the final height")
	require.Equal(t, int64(100), processed[len(processed)-1],
		"final coalesced height must be processed before exit on channel close")
	for i := 1; i < len(processed); i++ {
		require.Greater(t, processed[i], processed[i-1], "heights must be strictly increasing")
	}
}

// storeIfGreater stores h into *p if it is larger (test helper; the loop's reader
// is the single writer, so a plain compare-and-store is sufficient and race-free).
func storeIfGreater(p *int64, h int64) {
	if h > *p {
		*p = h
	}
}
