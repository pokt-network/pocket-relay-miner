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

// BlockEventPublisher is the one thing BlockPublisher needs from its Redis
// endpoint: somewhere to put a block event. It is an interface rather than
// *RedisBlockSubscriber because a constructor that DEMANDS a subscriber makes
// every caller build one — which is how the leader ended up running a
// pub/sub receive loop nobody consumed, double-counting
// ha_cache_block_events_received_total on the leader and making
// "published == received" unassertable.
//
// Narrowing removes the requirement, NOT the possibility: *RedisBlockSubscriber
// carries this exact method, so it still satisfies this interface and handing
// one over still compiles. Measured 2026-08-27 with a temporary
// `var _ BlockEventPublisher = (*RedisBlockSubscriber)(nil)`, which go vet
// accepted. What actually guards the wiring is leader_controller.go holding a
// *RedisBlockPublisher field, plus the live gate asserting published ==
// received per process (scripts/gates/live.sh, "block events published ==
// received").
type BlockEventPublisher interface {
	// PublishBlockHeight publishes a new block height to all subscribers.
	PublishBlockHeight(ctx context.Context, event BlockEvent) error
}

// BlockPublisher watches the blockchain for new blocks and publishes events.
// This should run on a single instance (leader) to avoid duplicate events.
type BlockPublisher struct {
	logger          logging.Logger
	blockSubscriber *localclient.BlockSubscriber
	publisher       BlockEventPublisher

	mu       sync.Mutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewBlockPublisher creates a new watcher that publishes block events.
func NewBlockPublisher(
	logger logging.Logger,
	blockSubscriber *localclient.BlockSubscriber,
	publisher BlockEventPublisher,
) *BlockPublisher {
	return &BlockPublisher{
		logger:          logger.With().Str("component", blockPublisherComponentName).Logger(),
		blockSubscriber: blockSubscriber,
		publisher:       publisher,
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

			if err := w.publisher.PublishBlockHeight(ctx, event); err != nil {
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
