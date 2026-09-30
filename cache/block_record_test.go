//go:build test

package cache

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/poktroll/pkg/client"
)

// The leader is the one reader of blocks; workers and relayers consume what it
// published. These tests pin the Redis half of that: the leader records each
// block before announcing it, a consumer serves a block at a height from that
// record without any node, and a consumer whose pub/sub channel went silent
// still sees the chain move.

// TestPublishBlockEvent_AConsumerWokenByTheEventFindsTheRecord: the record and
// the latest height are written with the event, so the consumer that hears N
// can always read hash(N).
func TestPublishBlockEvent_AConsumerWokenByTheEventFindsTheRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	redisClient := newTestRedis(t)

	sub := NewRedisBlockSubscriber(testLogger(), redisClient, nil)
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Close() })
	events := sub.Subscribe(ctx)

	pub := NewRedisBlockPublisher(testLogger(), redisClient)
	hash := []byte{0xAB, 0xCD, 0x01}
	// A subscriber registers on the server asynchronously; publish until the
	// first event arrives.
	var got BlockEvent
	require.Eventually(t, func() bool {
		require.NoError(t, pub.PublishBlockHeight(ctx, BlockEvent{Height: 500, Hash: hash, Timestamp: time.Unix(1, 0)}))
		select {
		case got = <-events:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, int64(500), got.Height)

	record, found, err := readBlockRecord(ctx, redisClient, 500)
	require.NoError(t, err)
	require.True(t, found, "the record must exist by the time the event is heard")
	require.Equal(t, hash, record.Hash)

	latest, found, err := readLatestPublishedHeight(ctx, redisClient)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(500), latest)
}

// countingReader is a node that answers every height with a hash of its own
// and counts the reads.
type countingReader struct{ reads atomic.Int64 }

func (r *countingReader) BlockAtHeight(_ context.Context, height int64) (client.Block, error) {
	r.reads.Add(1)
	return localclient.NewSimpleBlock(height, []byte("from-the-node"), time.Time{}), nil
}

// TestAdapter_GetBlockAtHeightReadsTheRecordThenItsOwnNode: the leader's record
// serves the hash without a node; when the record is missing (a refused write,
// a deleted key) the worker's own node serves it; with no node it is an error.
func TestAdapter_GetBlockAtHeightReadsTheRecordThenItsOwnNode(t *testing.T) {
	ctx := context.Background()
	redisClient := newTestRedis(t)
	node := &countingReader{}
	adapter := NewRedisBlockClientAdapter(testLogger(), NewRedisBlockSubscriber(testLogger(), redisClient, nil), node)

	hash := []byte{0x60, 0x06}
	pub := NewRedisBlockPublisher(testLogger(), redisClient)
	require.NoError(t, pub.PublishBlockHeight(ctx, BlockEvent{Height: 600, Hash: hash}))

	blk, err := adapter.GetBlockAtHeight(ctx, 600)
	require.NoError(t, err)
	require.Equal(t, hash, blk.Hash(), "the leader's record")
	require.Zero(t, node.reads.Load(), "a recorded height must not reach the node")

	blk, err = adapter.GetBlockAtHeight(ctx, 601)
	require.NoError(t, err)
	require.Equal(t, []byte("from-the-node"), blk.Hash(), "no record: the worker's own node")
	require.Equal(t, int64(1), node.reads.Load())

	// A record with no hash is no record: the node answers.
	data, err := json.Marshal(BlockEvent{Height: 602})
	require.NoError(t, err)
	require.NoError(t, redisClient.Set(ctx, redisClient.KB().BlockHashAtHeightKey(602), data, time.Minute).Err())
	blk, err = adapter.GetBlockAtHeight(ctx, 602)
	require.NoError(t, err)
	require.Equal(t, []byte("from-the-node"), blk.Hash(), "an empty recorded hash must not seed a proof")

	relayerLike := NewRedisBlockClientAdapter(testLogger(), NewRedisBlockSubscriber(testLogger(), redisClient, nil), nil)
	_, err = relayerLike.GetBlockAtHeight(ctx, 601)
	require.Error(t, err, "no record and no node")
}

// oomOnSet makes Redis behave as at maxmemory under noeviction for SET: a
// plain SET is refused with Redis's OOM error, and a MULTI that queues one is
// aborted whole, its PUBLISH included, as Redis answers EXECABORT.
type oomOnSet struct{}

func (oomOnSet) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (oomOnSet) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (oomOnSet) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		oom := errors.New("OOM command not allowed when used memory > 'maxmemory'.")
		transaction, hasSet := false, false
		for _, cmd := range cmds {
			switch cmd.Name() {
			case "multi":
				transaction = true
			case "set":
				hasSet = true
			}
		}
		if transaction && hasSet {
			abort := errors.New("EXECABORT Transaction discarded because of previous errors.")
			for _, cmd := range cmds {
				cmd.SetErr(abort)
			}
			return abort
		}
		kept := cmds[:0:0]
		for _, cmd := range cmds {
			if cmd.Name() == "set" {
				cmd.SetErr(oom)
				continue
			}
			kept = append(kept, cmd)
		}
		return next(ctx, kept)
	}
}

// TestPublishBlockEvent_TheEventGoesOutWhenRedisRefusesTheRecord: with Redis
// full the record cannot be written, and the event must still reach the
// fleet -- it is what moves the claims and proofs that free the memory.
func TestPublishBlockEvent_TheEventGoesOutWhenRedisRefusesTheRecord(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	redisClient, full := newTestRedisPair(t)

	sub := NewRedisBlockSubscriber(testLogger(), redisClient, nil)
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Close() })
	events := sub.Subscribe(ctx)

	full.AddHook(testredis.ProductCommands(oomOnSet{}))
	pub := NewRedisBlockPublisher(testLogger(), full)

	var got BlockEvent
	require.Eventually(t, func() bool {
		require.NoError(t, pub.PublishBlockHeight(ctx, BlockEvent{Height: 800, Hash: []byte{0x08}}))
		select {
		case got = <-events:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}, 5*time.Second, time.Millisecond, "the event must be published although the record was refused")
	require.Equal(t, int64(800), got.Height)
	_, found, err := readBlockRecord(ctx, redisClient, 800)
	require.NoError(t, err)
	require.False(t, found, "precondition: the hook refused the record")
}

// TestAdapter_ThePollDeliversAHeightThePubSubNeverCarried: the leader wrote a
// record and the latest height but the event never reached this process.
// LastBlock and the fan-out must move anyway.
func TestAdapter_ThePollDeliversAHeightThePubSubNeverCarried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	redisClient := newTestRedis(t)
	adapter := NewRedisBlockClientAdapter(testLogger(), NewRedisBlockSubscriber(testLogger(), redisClient, nil), nil)
	adapter.pollInterval = 20 * time.Millisecond
	require.NoError(t, adapter.Start(ctx))
	t.Cleanup(adapter.Close)
	blocks := adapter.Subscribe(ctx, 4)

	hash := []byte{0x07, 0x00}
	blockTime := time.Unix(1_790_000_000, 0).UTC()
	data, err := json.Marshal(BlockEvent{Height: 700, Hash: hash, Timestamp: blockTime})
	require.NoError(t, err)
	require.NoError(t, redisClient.Set(ctx, redisClient.KB().BlockHashAtHeightKey(700), data, time.Minute).Err())
	require.NoError(t, redisClient.Set(ctx, redisClient.KB().BlockLatestHeightKey(), 700, time.Minute).Err())

	select {
	case blk := <-blocks:
		require.Equal(t, int64(700), blk.Height())
		require.Equal(t, hash, blk.Hash())
	case <-ctx.Done():
		t.Fatal("the poll never delivered a height the pub/sub did not carry")
	}
	require.Equal(t, int64(700), adapter.LastBlock(ctx).Height())
	require.True(t, adapter.redisSubscriber.LatestBlockTime().Equal(blockTime),
		"the chain clock the tx client anchors timeouts on must move with the poll too")
}

// newTestRedisPair returns two clients on one namespace, so a hook installed
// on one breaks only that one.
func newTestRedisPair(t *testing.T) (*redisutil.Client, *redisutil.Client) {
	t.Helper()
	testredis.Client(t)
	prefix := testredis.Prefix(t)
	build := func() *redisutil.Client {
		c, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
			URL:       testredis.URL(),
			Namespace: config.RedisNamespaceConfig{BasePrefix: prefix},
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	return build(), build()
}

// TestAdapter_TwoDeliverersNeverSendALowerHeightAfterAHigherOne: the event loop
// and the latest-height poll both deliver. A height taken first must be sent
// before a higher one taken after it, or a relayer's height moves backwards.
func TestAdapter_TwoDeliverersNeverSendALowerHeightAfterAHigherOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adapter := NewRedisBlockClientAdapter(testLogger(), NewRedisBlockSubscriber(testLogger(), nil, nil), nil)
	adapter.ctx = ctx
	blocks := adapter.Subscribe(ctx, 4)

	adapter.afterAdvanceHook = func(height int64) {
		if height != 101 {
			return
		}
		// While 101 is taken but not sent, a second deliverer brings 103.
		done := make(chan struct{})
		go func() {
			adapter.deliver(BlockEvent{Height: 103})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}
	adapter.deliver(BlockEvent{Height: 101})

	first := <-blocks
	second := <-blocks
	require.Equal(t, []int64{101, 103}, []int64{first.Height(), second.Height()},
		"deliveries must reach consumers in the order their heights were taken")
}
