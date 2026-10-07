package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// subscribeReadyTimeout bounds how long SubscribeToInvalidations waits for the
// initial SUBSCRIBE to be confirmed by the server. Redis pub/sub has no
// replay: an invalidation published before the subscription is registered is
// silently dropped, so callers must not proceed (and start populating L1)
// until the subscription is live. If Redis is unreachable at startup the wait
// gives up after this timeout and the reconnection loop keeps retrying in the
// background — the pre-existing degraded-boot behavior is preserved.
var subscribeReadyTimeout = 10 * time.Second

// SubscribeToInvalidations subscribes to cache invalidation events for a specific cache type.
// It spawns a goroutine with automatic reconnection that listens for messages on the
// invalidation channel and calls the provided handler function for each message.
//
// Channel naming is managed by KeyBuilder: {namespace}:events:cache:{cacheType}:invalidate
//
// The subscription uses exponential backoff reconnection (1s → 2s → 4s → max 30s) to
// handle Redis disconnections gracefully.
//
// Example usage:
//
//	err := SubscribeToInvalidations(ctx, redisClient, "application", func(ctx context.Context, payload string) error {
//	    // Handle invalidation event
//	    return handleApplicationInvalidation(payload)
//	})
func SubscribeToInvalidations(
	ctx context.Context,
	store kv.Store,
	logger logging.Logger,
	cacheType string,
	handler func(ctx context.Context, payload string) error,
) error {
	channel := store.KB().EventChannel(cacheType, "invalidate")

	logger.Info().
		Str(logging.FieldCacheType, cacheType).
		Str("channel", channel).
		Msg("starting cache invalidation subscription with reconnection")

	// Closed once, when the first SUBSCRIBE is confirmed by the server.
	ready := make(chan struct{})
	var readyOnce sync.Once
	signalReady := func() { readyOnce.Do(func() { close(ready) }) }

	// Spawn goroutine with reconnection handling
	go func() {
		reconnectLoop := redisutil.NewReconnectionLoop(
			logger,
			fmt.Sprintf("pubsub_%s", cacheType),
			// connectFn: Test Redis connection
			func(ctx context.Context) error {
				return store.Ping(ctx)
			},
			// runFn: Subscribe and process messages until disconnect
			func(ctx context.Context) error {
				return runPubSubLoop(ctx, store, logger, channel, cacheType, handler, signalReady)
			},
		)

		reconnectLoop.Run(ctx)
	}()

	// Do not return before the subscription is registered on the server:
	// callers start serving (and populating L1) as soon as Start() returns, and
	// an invalidation published before SUBSCRIBE lands is dropped forever.
	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(subscribeReadyTimeout):
		logger.Warn().
			Str(logging.FieldCacheType, cacheType).
			Dur("waited", subscribeReadyTimeout).
			Msg("invalidation subscription not confirmed yet; reconnection loop continues in background")
	}

	return nil
}

// runPubSubLoop runs the pub/sub listener until disconnect or error.
// Returns error to trigger reconnection via the reconnection loop.
func runPubSubLoop(
	ctx context.Context,
	store kv.Store,
	logger logging.Logger,
	channel string,
	cacheType string,
	handler func(ctx context.Context, payload string) error,
	signalReady func(),
) error {
	// Returns once the subscription is active.
	sub, err := store.Subscribe(ctx, channel)
	if err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", channel, err)
	}
	defer func() { _ = sub.Close() }()
	signalReady()

	logger.Info().
		Str(logging.FieldCacheType, cacheType).
		Msg("pub/sub subscription active")

	// Process messages until disconnect or context cancellation
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case msg, ok := <-sub.Messages():
			// A closed channel is a lost subscription (Redis disconnected).
			if !ok {
				return fmt.Errorf("pub/sub channel closed")
			}

			if err := handler(ctx, msg.Payload); err != nil {
				logger.Warn().
					Err(err).
					Str(logging.FieldCacheType, cacheType).
					Str("payload", msg.Payload).
					Msg("failed to handle invalidation event")
			} else {
				logger.Debug().
					Str(logging.FieldCacheType, cacheType).
					Str("payload", msg.Payload).
					Msg("handled invalidation event")
			}
		}
	}
}

// PublishInvalidation publishes a cache invalidation event to notify other instances
// that a specific cache entry should be invalidated.
//
// Channel naming is managed by KeyBuilder: {namespace}:events:cache:{cacheType}:invalidate
//
// Example usage:
//
//	payload := `{"address": "pokt1abc..."}`
//	err := PublishInvalidation(ctx, redisClient, logger, "application", payload)
func PublishInvalidation(
	ctx context.Context,
	store kv.Store,
	logger logging.Logger,
	cacheType string,
	payload string,
) error {
	channel := store.KB().EventChannel(cacheType, "invalidate")

	if err := store.Publish(ctx, channel, []byte(payload)); err != nil {
		logger.Error().
			Err(err).
			Str(logging.FieldCacheType, cacheType).
			Str("channel", channel).
			Msg("failed to publish invalidation event")
		return fmt.Errorf("failed to publish to %s: %w", channel, err)
	}

	logger.Debug().
		Str(logging.FieldCacheType, cacheType).
		Str("payload", payload).
		Msg("published invalidation event")

	return nil
}

// deleteByPrefix deletes every L2 key under prefix. A key that fails is logged
// and the rest are still deleted: InvalidateAll clears what it can.
func deleteByPrefix(ctx context.Context, store kv.Store, logger logging.Logger, prefix string) {
	keys, err := store.ScanPrefix(ctx, prefix)
	if err != nil {
		logger.Warn().Err(err).Str("prefix", prefix).Msg("failed to scan cache keys for invalidation")
	}
	for _, key := range keys {
		if err := store.Del(ctx, key); err != nil {
			logger.Warn().Err(err).Str("key", key).Msg("failed to delete cache key")
		}
	}
}
