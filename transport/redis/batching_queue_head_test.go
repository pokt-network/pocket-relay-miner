//go:build test

package redis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fillOneStream enqueues n relays of one supplier. Each carries its position in
// ServiceId, so the order they leave the queue in can be read back.
func fillOneStream(t *testing.T, p *BatchingPublisher, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		msg := mined("pokt1head", "s1", i)
		msg.ServiceId = fmt.Sprintf("svc-%04d", i)
		require.NoError(t, p.Publish(context.Background(), msg))
	}
}

func servicesOf(chunk []queued) []string {
	out := make([]string, len(chunk))
	for i, q := range chunk {
		out[i] = q.service
	}
	return out
}

// TestTakeChunkAdvancesTheHeadAndZeroesWhatItTook: taking a chunk moves the head
// past it instead of copying the rest of the queue, and the slots it leaves hold
// nothing, so the relays already taken are not retained.
func TestTakeChunkAdvancesTheHeadAndZeroesWhatItTook(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)
	fillOneStream(t, p, 4*maxChunkCommands)

	chunk := p.takeChunk()
	require.Len(t, chunk, maxChunkCommands)

	p.mu.Lock()
	defer p.mu.Unlock()
	require.Equal(t, maxChunkCommands, p.head, "the head moves past the chunk")
	require.Len(t, p.queue, 4*maxChunkCommands, "the rest of the queue was not copied")
	for i := 0; i < p.head; i++ {
		require.Zero(t, p.queue[i], "slot %d still holds a taken relay", i)
	}
	require.Equal(t, fmt.Sprintf("svc-%04d", maxChunkCommands), p.queue[p.head].service)
}

// TestTheQueueStaysFirstInFirstOutAcrossCompaction: draining in chunks returns
// every relay once, in the order it was published, through the compactions that
// run once the taken slots are more than half the array.
func TestTheQueueStaysFirstInFirstOutAcrossCompaction(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)
	const n = 4*maxChunkCommands + 17
	fillOneStream(t, p, n)

	var got []string
	compacted := false
	for {
		chunk := p.takeChunk()
		if len(chunk) == 0 {
			break
		}
		got = append(got, servicesOf(chunk)...)
		p.mu.Lock()
		if p.head == 0 && len(p.queue) > 0 {
			compacted = true
		}
		p.mu.Unlock()
	}

	require.True(t, compacted, "premise: the drain went through a compaction")
	require.Len(t, got, n)
	for i, s := range got {
		require.Equal(t, fmt.Sprintf("svc-%04d", i), s, "relay %d left the queue out of order", i)
	}
	require.Zero(t, p.QueuedBytes())
}

// TestRequeueFrontPutsTheUnwrittenBackFirst: what a failed write gives back is
// taken again before anything behind it, whether it fits in the free slots in
// front of the head or needs a new array, and the queued bytes count it again.
func TestRequeueFrontPutsTheUnwrittenBackFirst(t *testing.T) {
	t.Run("into the free slots in front of the head", func(t *testing.T) {
		p, _, _ := newBatcher(t, time.Hour)
		fillOneStream(t, p, 3*maxChunkCommands)

		first := p.takeChunk()
		afterTake := p.QueuedBytes()
		p.requeueFront(first[100:])

		// Read under the lock and asserted outside it: a failed require stops the
		// test while holding the lock, and the cleanup's Close would wait for it
		// forever.
		p.mu.Lock()
		head := p.head
		p.mu.Unlock()
		require.Equal(t, 100, head, "the unwritten part goes back into the slots it came from")
		require.Equal(t, afterTake+bytes0(first[100:]), p.QueuedBytes())
		require.Equal(t, servicesOf(first[100:]), servicesOf(p.takeChunk()[:len(first)-100]))
	})

	t.Run("into a new array when the free slots are not enough", func(t *testing.T) {
		p, _, _ := newBatcher(t, time.Hour)
		fillOneStream(t, p, 3*maxChunkCommands)

		p.takeChunk()
		second := p.takeChunk()
		p.mu.Lock()
		head := p.head
		p.mu.Unlock()
		require.Zero(t, head, "premise: the second take compacted the queue, so no slot is free in front")
		afterTake := p.QueuedBytes()

		p.requeueFront(second)

		require.Equal(t, afterTake+bytes0(second), p.QueuedBytes())
		require.Equal(t, servicesOf(second), servicesOf(p.takeChunk()))
		require.Equal(t, fmt.Sprintf("svc-%04d", 2*maxChunkCommands), p.takeChunk()[0].service,
			"what was behind the requeued chunk follows it")
	})
}
