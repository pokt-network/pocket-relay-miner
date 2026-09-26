//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// What a miner signs is anchored on the chain's block time, read at startup.
// The seed has to reach the object the transaction client reads, not the one
// next to it: the block adapter keeps its own last block and nobody asks it for
// the anchor.
func TestSeedChainState_ReachesTheProviderTheTxClientReads(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	w := &SupplierWorker{
		redisBlockSubscriber:    cache.NewRedisBlockSubscriber(logger, nil, nil),
		redisBlockClientAdapter: cache.NewRedisBlockClientAdapter(logger, cache.NewRedisBlockSubscriber(logger, nil, nil), nil),
	}
	require.True(t, w.blockTimeProvider().LatestBlockTime().IsZero(), "premise: nothing is known before the seed")

	seeded := time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)
	w.seedChainState(43, seeded)

	require.Equal(t, seeded, w.blockTimeProvider().LatestBlockTime(),
		"LINK seed-reaches-the-provider: the object wired as BlockTimeProvider is the one that was seeded")
	require.Equal(t, int64(43), w.redisBlockClientAdapter.LastBlock(context.Background()).Height(),
		"the height went to the block adapter")
}

// The zero clock does not only anchor badly: reusable() answers no with it, so
// a restarted miner that HAS the signed bytes of a proof refuses to re-inject
// them and signs again -- which is the path that was rejected 238 times. With
// the startup seed the same bytes are re-injected.
func TestSeedChainState_LetsARestartedMinerReinjectWhatItAlreadySigned(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	subscriber := cache.NewRedisBlockSubscriber(logger, nil, nil)
	chainNow := time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)
	cached := tx.SignedTxPayload{Bytes: []byte("already-signed"), TimeoutAt: chainNow.Add(5 * time.Minute)}

	require.False(t, reusable(cached, subscriber.LatestBlockTime()),
		"premise: with no block time the signed bytes are thrown away and signed again")

	w := &SupplierWorker{
		redisBlockSubscriber:    subscriber,
		redisBlockClientAdapter: cache.NewRedisBlockClientAdapter(logger, cache.NewRedisBlockSubscriber(logger, nil, nil), nil),
	}
	w.seedChainState(43, chainNow)

	require.True(t, reusable(cached, w.blockTimeProvider().LatestBlockTime()),
		"LINK seed-reinjects: with the chain's time seeded the miner re-injects the bytes it already signed")
}
