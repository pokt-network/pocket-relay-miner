//go:build test

package client

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A node saves a block, and answers it on status and block, before its app
// commits it; until the commit, a transaction is simulated at the height
// before, so a claim or proof sent on the first block of its window is refused
// as outside it. On beta: "current block height (748062) is less than
// session's earliest proof commit height (748063)". These tests pin that the
// leader publishes, and starts from, only heights the node's app committed.

// requireNothingPublished fails if a block is waiting on blocks.
func requireNothingPublished(t *testing.T, blocks <-chan *SimpleBlock) {
	t.Helper()
	select {
	case blk := <-blocks:
		t.Fatalf("height %d was published before the node's app committed it", blk.Height())
	default:
	}
}

func TestBlockSubscriber_PublishesOnlyTheHeightTheAppCommitted(t *testing.T) {
	mock := newMockCometBFTServer(t)
	mock.silentWS.Store(true)
	mock.committedHeight.Store(100)
	mock.incrementHeight() // block 101 saved, not committed yet

	sub, blocks := startSingleReader(t, mock, 20*time.Millisecond)
	// Polls to either endpoint: a poll reading the wrong one is waited for too.
	require.Eventually(t, func() bool { return mock.abciRequests.Load()+mock.statusRequests.Load() >= 5 }, 10*time.Second, 5*time.Millisecond)

	requireNothingPublished(t, blocks)
	require.Equal(t, int64(100), sub.LastBlock(context.Background()).Height(),
		"the start must be the committed height, not the saved one")

	mock.committedHeight.Store(101)
	require.Equal(t, int64(101), receiveBlock(t, blocks).Height())
}

// With abci_info failing, the poll falls back to status, as before abci_info
// was read: the height before status would be a block late on every block.
func TestBlockSubscriber_WithoutAbciInfoFallsBackToStatus(t *testing.T) {
	mock := newMockCometBFTServer(t)
	mock.silentWS.Store(true)
	mock.abciDown.Store(true)
	mock.incrementHeight() // status says 101

	sub, blocks := startSingleReader(t, mock, 20*time.Millisecond)
	require.Eventually(t, func() bool { return mock.statusRequests.Load() >= 5 }, 10*time.Second, 5*time.Millisecond)

	requireNothingPublished(t, blocks)
	require.Equal(t, int64(101), sub.LastBlock(context.Background()).Height(),
		"without abci_info the start is status's height, not the one before")

	mock.incrementHeight() // status says 102
	require.Equal(t, int64(102), receiveBlock(t, blocks).Height())
}

// syncBuffer is a log sink the subscriber's goroutines write while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// count is how many log lines carry msg.
func (b *syncBuffer) count(msg string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), msg)
}

// The fallback is a Warn once abci_info keeps failing and an Info when it
// answers again, not a line per poll.
func TestBlockSubscriber_AbciInfoFallbackIsOneWarnAndOneInfo(t *testing.T) {
	mock := newMockCometBFTServer(t)
	mock.silentWS.Store(true)
	mock.abciDown.Store(true)

	logs := &syncBuffer{}
	sub, err := NewBlockSubscriber(zerolog.New(logs), BlockSubscriberConfig{
		RPCEndpoint:  mock.server.URL,
		PollInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sub.Close()
	})
	require.NoError(t, sub.Start(ctx))

	const warn = "abci_info keeps failing"
	const info = "abci_info answers again"
	require.Eventually(t, func() bool { return mock.abciRequests.Load() >= 4*abciFailuresBeforeWarn }, 10*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, logs.count(warn), "one Warn however many polls fall back")
	require.Zero(t, logs.count(info))

	mock.abciDown.Store(false)
	require.Eventually(t, func() bool { return logs.count(info) == 1 }, 10*time.Second, 5*time.Millisecond)
	recovered := mock.abciRequests.Load()
	require.Eventually(t, func() bool { return mock.abciRequests.Load() >= recovered+5 }, 10*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, logs.count(info), "one Info however many polls succeed")
	require.Equal(t, 1, logs.count(warn))
}
