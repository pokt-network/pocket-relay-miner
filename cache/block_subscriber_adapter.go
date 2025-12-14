package cache

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

// BlockSubscriberAdapter wraps BlockSubscriber to implement BlockHeightSubscriber.
// Converts client.Block → BlockEvent for CacheOrchestrator compatibility.
//
// This adapter is used in the miner to bridge the WebSocket-based BlockSubscriber
// (which provides client.Block events) to the CacheOrchestrator (which expects
// BlockEvent via BlockHeightSubscriber interface).
type BlockSubscriberAdapter struct {
	logger          logging.Logger
	blockSubscriber interface {
		Subscribe(ctx context.Context, bufferSize int) <-chan client.Block
	}
	eventCh chan BlockEvent
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewBlockSubscriberAdapter creates an adapter for miner's CacheOrchestrator.
// The adapter subscribes to the WebSocket BlockSubscriber and converts
// client.Block events to BlockEvent format.
func NewBlockSubscriberAdapter(
	logger logging.Logger,
	blockSubscriber interface {
		Subscribe(ctx context.Context, bufferSize int) <-chan client.Block
	},
) *BlockSubscriberAdapter {
	return &BlockSubscriberAdapter{
		logger:          logging.ForComponent(logger, "block_subscriber_adapter"),
		blockSubscriber: blockSubscriber,
		eventCh:         make(chan BlockEvent, 100),
	}
}

// Start begins forwarding block events from WebSocket to CacheOrchestrator.
// It spawns a goroutine that:
// 1. Subscribes to the WebSocket BlockSubscriber
// 2. Converts each client.Block to BlockEvent
// 3. Forwards events to the eventCh channel (non-blocking)
// 4. Recovers from panics with stack trace logging
func (a *BlockSubscriberAdapter) Start(ctx context.Context) error {
	a.ctx, a.cancel = context.WithCancel(ctx)

	// Subscribe to WebSocket fan-out
	blockCh := a.blockSubscriber.Subscribe(a.ctx, 100)

	// Convert client.Block → BlockEvent with panic recovery
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				a.logger.Error().
					Interface("panic", r).
					Str("stack", string(debug.Stack())).
					Msg("recovered from panic in block event adapter")
			}
		}()

		for {
			select {
			case <-a.ctx.Done():
				a.logger.Info().Msg("block subscriber adapter stopped")
				return

			case block, ok := <-blockCh:
				if !ok {
					a.logger.Warn().Msg("block channel closed")
					return
				}

				event := BlockEvent{
					Height:    block.Height(),
					Hash:      block.Hash(),
					Timestamp: time.Now(),
				}

				// Non-blocking send: drop event if channel is full
				// This prevents slow CacheOrchestrator from blocking the adapter
				select {
				case a.eventCh <- event:
					// Event sent successfully
				case <-a.ctx.Done():
					return
				default:
					// Drop if full - CacheOrchestrator is slow
					a.logger.Warn().
						Int64("height", event.Height).
						Msg("event channel full, dropping block event")
				}
			}
		}
	}()

	a.logger.Info().Msg("block subscriber adapter started")
	return nil
}

// Subscribe returns channel for BlockEvent (implements BlockHeightSubscriber).
// This method satisfies the BlockHeightSubscriber interface required by
// CacheOrchestrator.
func (a *BlockSubscriberAdapter) Subscribe(ctx context.Context) <-chan BlockEvent {
	return a.eventCh
}

// PublishBlockHeight is not supported by the adapter (read-only).
// This method exists to satisfy the BlockHeightSubscriber interface,
// but the adapter only subscribes to events, it doesn't publish them.
func (a *BlockSubscriberAdapter) PublishBlockHeight(ctx context.Context, event BlockEvent) error {
	return fmt.Errorf("PublishBlockHeight not supported by BlockSubscriberAdapter (read-only adapter)")
}

// Close stops the adapter and waits for goroutine cleanup.
// It cancels the context, waits for the conversion goroutine to exit,
// and closes the event channel.
func (a *BlockSubscriberAdapter) Close() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	close(a.eventCh)
	a.logger.Info().Msg("block subscriber adapter closed")
	return nil
}
