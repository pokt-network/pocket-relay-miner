//go:build test

package client

import (
	"context"
	"testing"
	"time"

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

	require.Equal(t, int64(100), sub.LastBlock(context.Background()).Height(),
		"the start must be the committed height, not the saved one")
	requireNothingPublished(t, blocks)

	mock.committedHeight.Store(101)
	require.Equal(t, int64(101), receiveBlock(t, blocks).Height())
}

// With abci_info failing, the poll still advances, one height behind status:
// a node saves a block only after committing the one before.
func TestBlockSubscriber_WithoutAbciInfoPublishesTheHeightBeforeStatus(t *testing.T) {
	mock := newMockCometBFTServer(t)
	mock.silentWS.Store(true)
	mock.abciDown.Store(true)
	mock.incrementHeight() // status says 101

	sub, blocks := startSingleReader(t, mock, 20*time.Millisecond)
	require.Eventually(t, func() bool { return mock.abciRequests.Load()+mock.statusRequests.Load() >= 5 }, 10*time.Second, 5*time.Millisecond)

	require.Equal(t, int64(100), sub.LastBlock(context.Background()).Height())
	requireNothingPublished(t, blocks)

	mock.incrementHeight() // status says 102: 101 is committed
	require.Equal(t, int64(101), receiveBlock(t, blocks).Height())
}
