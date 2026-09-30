//go:build test

package client

import (
	"context"
	"fmt"
	"testing"
	"time"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// These tests pin the leader's single reader of blocks against the two ways a
// node fails it on beta: a websocket that stays open and delivers nothing (the
// node drops events for a client it considers slow, and the client never
// closes its channel), and a node that announces a height before it can serve
// it -- behind a load balancer, the read lands on a node one block behind and
// answers "height N must be less than or equal to the current blockchain height
// N-1". Both used to cost a block: the event was dropped and the height never
// published.

func startSingleReader(t *testing.T, mock *mockCometBFTServer, poll time.Duration) (*BlockSubscriber, <-chan *SimpleBlock) {
	t.Helper()
	sub, err := NewBlockSubscriber(logging.NewLoggerFromConfig(logging.DefaultConfig()), BlockSubscriberConfig{
		RPCEndpoint:  mock.server.URL,
		PollInterval: poll,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sub.Close()
	})
	require.NoError(t, sub.Start(ctx))
	return sub, sub.Subscribe(ctx, 100)
}

func receiveBlock(t *testing.T, ch <-chan *SimpleBlock) *SimpleBlock {
	t.Helper()
	select {
	case blk := <-ch:
		return blk
	case <-time.After(10 * time.Second):
		t.Fatal("no block published within 10s")
		return nil
	}
}

// TestBlockSubscriber_PublishesEveryHeightInOrderThroughASilentWebsocketAndANodeBehind
// is the beta failure end to end: the websocket says nothing, the chain moves
// three blocks, and the first reads of the new heights answer "not yet". Every
// height must still be published, in order, with its own hash.
func TestBlockSubscriber_PublishesEveryHeightInOrderThroughASilentWebsocketAndANodeBehind(t *testing.T) {
	mock := newMockCometBFTServer(t)
	mock.silentWS.Store(true)
	_, blocks := startSingleReader(t, mock, 20*time.Millisecond)

	mock.notYetAnswers.Store(2)
	mock.incrementHeight()
	mock.incrementHeight()
	mock.incrementHeight()

	for want := int64(101); want <= 103; want++ {
		blk := receiveBlock(t, blocks)
		require.Equal(t, want, blk.Height(), "heights must be published in order, none skipped")
		require.Equal(t, fmt.Sprintf("ABCD%016X", want), fmt.Sprintf("%X", blk.Hash()),
			"each height carries the hash read at THAT height")
	}
	require.LessOrEqual(t, mock.notYetAnswers.Load(), int64(0), "the node's not-yet answers must have been exercised")
}

// TestBlockSubscriber_ReplacesAWebsocketThatWentSilent pins the watchdog: when
// the poll keeps publishing heights the websocket never reported, the
// websocket client is thrown away and a new one subscribes.
func TestBlockSubscriber_ReplacesAWebsocketThatWentSilent(t *testing.T) {
	mock := newMockCometBFTServer(t)
	_, blocks := startSingleReader(t, mock, 20*time.Millisecond)
	require.Eventually(t, func() bool { return mock.subscribeCount.Load() == 1 }, 10*time.Second, 10*time.Millisecond)

	mock.silentWS.Store(true)
	for i := 0; i < wsStaleBlocks; i++ {
		mock.incrementHeight()
		receiveBlock(t, blocks)
	}

	require.Eventually(t, func() bool { return mock.subscribeCount.Load() >= 2 }, 10*time.Second, 10*time.Millisecond,
		"a websocket that reported none of %d published heights must be replaced", wsStaleBlocks)
}

// TestBlockSubscriber_AWebsocketEventPublishesWithoutThePoll shows the
// websocket half works alone: with the poll effectively off, one
// NewBlockHeader event publishes its height, read from the node. The event is
// handed to handleBlockEvent directly because the mock cannot produce frames
// the CometBFT client decodes.
func TestBlockSubscriber_AWebsocketEventPublishesWithoutThePoll(t *testing.T) {
	mock := newMockCometBFTServer(t)
	sub, blocks := startSingleReader(t, mock, time.Hour)

	height := mock.incrementHeight()
	require.NoError(t, sub.handleBlockEvent(&coretypes.ResultEvent{
		Query: newBlockHeaderQuery,
		Data:  types.EventDataNewBlockHeader{Header: types.Header{Height: height}},
	}))

	blk := receiveBlock(t, blocks)
	require.Equal(t, height, blk.Height())
	require.Equal(t, fmt.Sprintf("ABCD%016X", height), fmt.Sprintf("%X", blk.Hash()))
}

// TestBlockReader_RetriesOnlyNotYet: a read waits out a node that does not
// have the height yet, and returns any other error at once.
func TestBlockReader_RetriesOnlyNotYet(t *testing.T) {
	mock := newMockCometBFTServer(t)
	reader, err := NewBlockReader(mock.server.URL, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mock.notYetAnswers.Store(2)
	before := mock.blockRequests.Load()
	blk, err := reader.BlockAtHeight(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, int64(100), blk.Height())
	require.Equal(t, int64(3), mock.blockRequests.Load()-before, "two not-yet answers, then the block")

	mock.failNextBlock.Store(true)
	before = mock.blockRequests.Load()
	_, err = reader.BlockAtHeight(ctx, 100)
	require.Error(t, err)
	require.Equal(t, int64(1), mock.blockRequests.Load()-before, "an error that is not not-yet must not be retried")
}

// TestBlockSubscriber_HeightsThatKeepFailingDoNotStopTheOnesAfterThem: heights
// whose reads fail for a reason other than "not yet" are each skipped after
// maxReadFailures passes, two in a row included, and the next height is
// published.
func TestBlockSubscriber_HeightsThatKeepFailingDoNotStopTheOnesAfterThem(t *testing.T) {
	mock := newMockCometBFTServer(t)
	mock.silentWS.Store(true)
	mock.brokenFrom.Store(101)
	mock.brokenTo.Store(102)
	_, blocks := startSingleReader(t, mock, 5*time.Millisecond)

	mock.incrementHeight()
	mock.incrementHeight()
	mock.incrementHeight()

	blk := receiveBlock(t, blocks)
	require.Equal(t, int64(103), blk.Height(), "101 and 102 keep failing; 103 must still be published")
}

// TestBlockReader_RefusesABlockThatIsNotTheOneAskedFor: an answer for another
// height is not the block at this height.
func TestBlockReader_RefusesABlockThatIsNotTheOneAskedFor(t *testing.T) {
	mock := newMockCometBFTServer(t)
	mock.wrongHeightFor.Store(100)
	reader, err := NewBlockReader(mock.server.URL, false)
	require.NoError(t, err)

	_, err = reader.BlockAtHeight(context.Background(), 100)
	require.ErrorContains(t, err, "no usable block for height 100")
}

// TestBlockSubscriber_OnlyAWebsocketThatSaysNothingIsStale: a websocket that
// lags a block but keeps reporting is never stale, nor is a catch-up that
// publishes heights below the one just announced; wsStaleBlocks heights
// published past a websocket that says nothing make it stale.
func TestBlockSubscriber_OnlyAWebsocketThatSaysNothingIsStale(t *testing.T) {
	sub, err := NewBlockSubscriber(logging.NewLoggerFromConfig(logging.DefaultConfig()), BlockSubscriberConfig{RPCEndpoint: "http://127.0.0.1:1"})
	require.NoError(t, err)
	t.Cleanup(sub.Close)
	announce := func(height int64) {
		require.NoError(t, sub.handleBlockEvent(&coretypes.ResultEvent{
			Data: types.EventDataNewBlockHeader{Header: types.Header{Height: height}},
		}))
	}

	for h := int64(100); h < 110; h++ {
		sub.notePublished(h)
		announce(h - 1) // the websocket's node is a block behind the poll's
		require.Less(t, sub.publishedPastWS.Load(), int64(wsStaleBlocks), "a lagging websocket that reports is not stale")
	}

	announce(120)
	for h := int64(110); h <= 120; h++ {
		sub.notePublished(h)
	}
	require.Zero(t, sub.publishedPastWS.Load(), "a catch-up below the announced height is not silence")

	sub.notePublished(121)
	sub.notePublished(122)
	require.GreaterOrEqual(t, sub.publishedPastWS.Load(), int64(wsStaleBlocks), "two heights past a silent websocket")
}
