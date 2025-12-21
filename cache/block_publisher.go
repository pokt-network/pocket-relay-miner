package cache

import (
	"context"
	"fmt"
	"sync"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	blockPublisherComponentName = "block_publisher"
)

// BlockPublisher watches the blockchain for new blocks and publishes events.
// This should run on a single instance (leader) to avoid duplicate events.
type BlockPublisher struct {
	logger          logging.Logger
	blockSubscriber *localclient.BlockSubscriber
	redisSubscriber *RedisBlockSubscriber

	mu       sync.Mutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewBlockPublisher creates a new watcher that publishes block events.
func NewBlockPublisher(
	logger logging.Logger,
	blockSubscriber *localclient.BlockSubscriber,
	redisSubscriber *RedisBlockSubscriber,
) *BlockPublisher {
	return &BlockPublisher{
		logger:          logger.With().Str("component", blockPublisherComponentName).Logger(),
		blockSubscriber: blockSubscriber,
		redisSubscriber: redisSubscriber,
	}
}

// Start begins watching for new blocks and publishing events.
func (w *BlockPublisher) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("watcher is closed")
	}

	ctx, w.cancelFn = context.WithCancel(ctx)
	w.mu.Unlock()

	w.wg.Add(1)
	go w.watchLoop(ctx)

	w.logger.Info().Msg("block publisher started")
	return nil
}

// watchLoop watches for new blocks and publishes events.
// Uses event-driven notifications via Subscribe() method.
func (w *BlockPublisher) watchLoop(ctx context.Context) {
	defer w.wg.Done()

	// Subscribe to block events with 2000-block buffer for publishing to Redis
	blockCh := w.blockSubscriber.Subscribe(ctx, 2000)
	w.logger.Info().Msg("using event-driven block notifications (Subscribe)")

	for {
		select {
		case <-ctx.Done():
			return

		case blk, ok := <-blockCh:
			if !ok {
				// Channel closed, block subscriber shut down
				w.logger.Error().Msg("block events channel closed")
				return
			}

			event := BlockEvent{
				Height:    blk.Height(),
				Hash:      blk.Hash(),
				Timestamp: blk.Time(),
			}

			if err := w.redisSubscriber.PublishBlockHeight(ctx, event); err != nil {
				w.logger.Error().
					Err(err).
					Int64("height", event.Height).
					Msg("failed to publish block event")
			}
		}
	}
}

// Close gracefully shuts down the watcher.
func (w *BlockPublisher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}

	w.closed = true

	if w.cancelFn != nil {
		w.cancelFn()
	}

	w.wg.Wait()

	w.logger.Info().Msg("block height watcher closed")
	return nil
}
