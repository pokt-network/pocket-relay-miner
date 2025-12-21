package cache

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

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
	redisClient *redisutil.Client,
	logger logging.Logger,
	cacheType string,
	handler func(ctx context.Context, payload string) error,
) error {
	channel := redisClient.KB().EventChannel(cacheType, "invalidate")

	logger.Info().
		Str(logging.FieldCacheType, cacheType).
		Str("channel", channel).
		Msg("starting cache invalidation subscription with reconnection")

	// Spawn goroutine with reconnection handling
	go func() {
		reconnectLoop := redisutil.NewReconnectionLoop(
			logger,
			fmt.Sprintf("pubsub_%s", cacheType),
			// connectFn: Test Redis connection
			func(ctx context.Context) error {
				return redisClient.Ping(ctx).Err()
			},
			// runFn: Subscribe and process messages until disconnect
			func(ctx context.Context) error {
				return runPubSubLoop(ctx, redisClient, logger, channel, cacheType, handler)
			},
		)

		reconnectLoop.Run(ctx)
	}()

	return nil
}

// runPubSubLoop runs the pub/sub listener until disconnect or error.
// Returns error to trigger reconnection via the reconnection loop.
func runPubSubLoop(
	ctx context.Context,
	redisClient *redisutil.Client,
	logger logging.Logger,
	channel string,
	cacheType string,
	handler func(ctx context.Context, payload string) error,
) error {
	pubsub := redisClient.Subscribe(ctx, channel)
	defer func() { _ = pubsub.Close() }()

	// Verify subscription
	if _, err := pubsub.Receive(ctx); err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", channel, err)
	}

	logger.Info().
		Str(logging.FieldCacheType, cacheType).
		Msg("pub/sub subscription active")

	// Process messages until disconnect or context cancellation
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case msg := <-pubsub.Channel():
			// Check if msg is nil (a channel closed - Redis disconnected)
			if msg == nil {
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
	redisClient *redisutil.Client,
	logger logging.Logger,
	cacheType string,
	payload string,
) error {
	channel := redisClient.KB().EventChannel(cacheType, "invalidate")

	if err := redisClient.Publish(ctx, channel, payload).Err(); err != nil {
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
