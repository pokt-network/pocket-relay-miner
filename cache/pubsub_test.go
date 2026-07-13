//go:build test

package cache

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestSubscribeToInvalidations_ActiveOnReturn pins the subscription liveness
// contract: when SubscribeToInvalidations returns, the SUBSCRIBE must already
// be registered on the server. Redis pub/sub has no replay, so any window
// between "caller proceeds" and "subscription active" silently drops
// invalidations published into it — the consumer's L1 then serves a stale
// entry until its TTL floor expires.
//
// The assertion is deliberately immediate (no Eventually): polling would
// re-open the very race this test exists to pin.
func TestSubscribeToInvalidations_ActiveOnReturn(t *testing.T) {
	redisClient := newTestRedis(t)
	log := logging.NewLoggerFromConfig(logging.DefaultConfig())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	err := SubscribeToInvalidations(ctx, redisClient, log, "service",
		func(ctx context.Context, payload string) error { return nil })
	require.NoError(t, err)

	channel := redisClient.KB().EventChannel("service", "invalidate")
	counts, err := redisClient.PubSubNumSub(context.Background(), channel).Result()
	require.NoError(t, err)
	require.GreaterOrEqual(t, counts[channel], int64(1),
		"subscription must be active on the server before SubscribeToInvalidations returns")
}

// TestSubscribeToInvalidations_DeliversMessagePublishedImmediately is the
// consumer-visible version of the same contract: a message published right
// after SubscribeToInvalidations returns must reach the handler.
func TestSubscribeToInvalidations_DeliversMessagePublishedImmediately(t *testing.T) {
	redisClient := newTestRedis(t)
	log := logging.NewLoggerFromConfig(logging.DefaultConfig())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var delivered atomic.Int64
	err := SubscribeToInvalidations(ctx, redisClient, log, "application",
		func(ctx context.Context, payload string) error {
			delivered.Add(1)
			return nil
		})
	require.NoError(t, err)

	require.NoError(t, PublishInvalidation(ctx, redisClient, log, "application", "x"))

	require.Eventually(t, func() bool { return delivered.Load() == 1 },
		5*time.Second, 5*time.Millisecond,
		"invalidation published immediately after subscribe must be delivered, not dropped")
}
