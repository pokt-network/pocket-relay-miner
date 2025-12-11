package redis

import (
	"context"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// Reconnection delay constants (matching client/block_subscriber.go pattern)
	reconnectBaseDelay     = 1 * time.Second
	reconnectMaxDelay      = 30 * time.Second
	reconnectBackoffFactor = 2
)

// ReconnectionLoop provides exponential backoff reconnection logic for Redis operations.
// This pattern is copied from client/block_subscriber.go:145-194 which has proven
// reliable in production for WebSocket connections.
//
// Usage:
//
//	loop := NewReconnectionLoop(logger, "streams_consumer",
//	    func(ctx context.Context) error {
//	        // Test connection health (e.g., Redis Ping, ensure consumer group exists)
//	        return redisClient.Ping(ctx).Err()
//	    },
//	    func(ctx context.Context) error {
//	        // Main operation loop (e.g., consume messages, subscribe to pub/sub)
//	        // Return error to trigger reconnection
//	        return runMainLoop(ctx)
//	    },
//	)
//	loop.Run(ctx)
type ReconnectionLoop struct {
	logger        logging.Logger
	componentName string
	connectFn     func(context.Context) error
	runFn         func(context.Context) error
}

// NewReconnectionLoop creates a new reconnection loop.
//
// Parameters:
//   - logger: Logger for reconnection events
//   - component: Component name for logging and metrics (e.g., "streams_consumer", "pubsub_cache")
//   - connectFn: Function to test connection health before running main loop
//   - runFn: Main operation loop that runs until error or context cancellation
func NewReconnectionLoop(
	logger logging.Logger,
	component string,
	connectFn func(context.Context) error,
	runFn func(context.Context) error,
) *ReconnectionLoop {
	return &ReconnectionLoop{
		logger:        logger,
		componentName: component,
		connectFn:     connectFn,
		runFn:         runFn,
	}
}

// Run executes the reconnection loop with exponential backoff.
//
// The loop:
// 1. Attempts connection via connectFn
// 2. On connection failure: backs off exponentially (1s → 2s → 4s → max 30s)
// 3. On connection success: runs main loop via runFn
// 4. On main loop error/disconnect: immediately attempts reconnection
// 5. On context cancellation: exits gracefully
//
// This method blocks until context is cancelled.
func (r *ReconnectionLoop) Run(ctx context.Context) {
	reconnectDelay := reconnectBaseDelay

	for {
		select {
		case <-ctx.Done():
			r.logger.Debug().
				Str(logging.FieldComponent, r.componentName).
				Msg("reconnection loop shutting down")
			return
		default:
		}

		// Attempt connection
		redisReconnectionAttempts.WithLabelValues(r.componentName).Inc()

		if err := r.connectFn(ctx); err != nil {
			r.logger.Warn().
				Err(err).
				Str(logging.FieldComponent, r.componentName).
				Dur("retry_in", reconnectDelay).
				Msgf("%s: connection failed, will retry", r.componentName)

			// Exponential backoff before retry
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
				reconnectDelay = increaseBackoff(reconnectDelay)
				continue
			}
		}

		// Connection successful - reset backoff and track success
		reconnectDelay = reconnectBaseDelay
		redisReconnectionSuccess.WithLabelValues(r.componentName).Inc()

		r.logger.Info().
			Str(logging.FieldComponent, r.componentName).
			Msgf("%s: connection established", r.componentName)

		// Run main loop until disconnect or error
		err := r.runFn(ctx)

		// Check if we're shutting down (context cancelled)
		select {
		case <-ctx.Done():
			r.logger.Debug().
				Str(logging.FieldComponent, r.componentName).
				Msg("shutting down gracefully")
			return
		default:
			// Real disconnection - log and reconnect
			if err != nil {
				r.logger.Warn().
					Err(err).
					Str(logging.FieldComponent, r.componentName).
					Msgf("%s: disconnected, reconnecting...", r.componentName)
			} else {
				r.logger.Warn().
					Str(logging.FieldComponent, r.componentName).
					Msgf("%s: connection closed, reconnecting...", r.componentName)
			}
		}
	}
}

// increaseBackoff increases the reconnection delay with exponential backoff.
// Delay doubles each time until it reaches the maximum delay.
func increaseBackoff(current time.Duration) time.Duration {
	next := current * reconnectBackoffFactor
	if next > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return next
}
