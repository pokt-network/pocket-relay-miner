package cache

import (
	"context"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-version"
	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

// RedisBlockClientAdapter adapts RedisBlockSubscriber to implement client.BlockClient.
// This enables relayer components to receive block events from Redis pub/sub
// (synchronized with the miner's view) while satisfying the client.BlockClient interface.
//
// Used in relayers to:
// 1. Subscribe to Redis pub/sub for block events from miner
// 2. Maintain local cache of last block (for LastBlock() calls)
// 3. Provide BlockEvents() channel for components expecting it
type RedisBlockClientAdapter struct {
	logger          logging.Logger
	redisSubscriber *RedisBlockSubscriber
	lastBlock       atomic.Pointer[simpleBlock]
	blockEventsCh   chan client.Block
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup

	// Fan-out subscribers for Subscribe() method
	subscribersMu sync.RWMutex
	subscribers   []chan *localclient.SimpleBlock
}

// NewRedisBlockClientAdapter creates an adapter for relayer block client.
// The adapter subscribes to Redis events and implements client.BlockClient interface.
//
// Parameters:
// - logger: Logger for the adapter
// - redisSubscriber: RedisBlockSubscriber that receives events from Redis pub/sub
func NewRedisBlockClientAdapter(
	logger logging.Logger,
	redisSubscriber *RedisBlockSubscriber,
) *RedisBlockClientAdapter {
	return &RedisBlockClientAdapter{
		logger:          logging.ForComponent(logger, logging.ComponentRedisBlockClientAdapter),
		redisSubscriber: redisSubscriber,
		blockEventsCh:   make(chan client.Block, 2000),
	}
}

// Start begins forwarding Redis block events to the local cache and channel.
func (a *RedisBlockClientAdapter) Start(ctx context.Context) error {
	a.ctx, a.cancel = context.WithCancel(ctx)

	// Subscribe to Redis block events
	eventsCh := a.redisSubscriber.Subscribe(a.ctx)

	// Convert BlockEvent → simpleBlock and forward to blockEventsCh
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				a.logger.Error().
					Interface("panic", r).
					Str("stack", string(debug.Stack())).
					Msg("recovered from panic in redis block client adapter")
			}
		}()

		for {
			select {
			case <-a.ctx.Done():
				a.logger.Info().Msg("redis block client adapter stopped")
				return

			case event, ok := <-eventsCh:
				if !ok {
					a.logger.Warn().Msg("redis events channel closed")
					return
				}

				// Update cached last block atomically
				block := &simpleBlock{
					height: event.Height,
					hash:   event.Hash,
				}
				a.lastBlock.Store(block)

				// Forward to blockEventsCh for components using BlockEvents()
				// Non-blocking send to prevent Redis event loop from blocking
				select {
				case a.blockEventsCh <- block:
					// Event forwarded successfully
				case <-a.ctx.Done():
					return
				default:
					// Drop if full - component is slow
					a.logger.Warn().
						Int64("height", event.Height).
						Msg("block events channel full, dropping event")
				}

				// Fan-out to Subscribe() subscribers
				a.publishToSubscribers(&event)
			}
		}
	}()

	a.logger.Info().Msg("redis block client adapter started")
	return nil
}

// LastBlock returns the last known block from Redis events.
// This is cached locally and updated atomically as events arrive.
func (a *RedisBlockClientAdapter) LastBlock(ctx context.Context) client.Block {
	block := a.lastBlock.Load()
	if block == nil {
		// Return zero block if no events received yet
		return &simpleBlock{height: 0, hash: nil}
	}
	return block
}

// GetChainVersion returns nil - not used in production.
//
// This method exists solely for poktroll client.BlockClient interface compliance.
// Relayers receive block events from Redis pub/sub (synchronized with miner's view)
// and don't need chain version information.
//
// Interface: github.com/pokt-network/poktroll/pkg/client.BlockClient
// Used by: Tests only (never called in production code)
func (a *RedisBlockClientAdapter) GetChainVersion() *version.Version {
	return nil
}

// CommittedBlocksSequence returns nil - not used in production.
//
// This method exists solely for poktroll client.BlockClient interface compliance.
// The interface expects an observable-based block replay pattern, but relayers
// use event-driven updates from Redis pub/sub via BlockEvents() instead.
//
// Interface: github.com/pokt-network/poktroll/pkg/client.BlockClient
// Used by: Tests only (never called in production code)
func (a *RedisBlockClientAdapter) CommittedBlocksSequence(ctx context.Context) client.BlockReplayObservable {
	return nil
}

// BlockEvents returns a channel that receives block events from Redis.
// This provides backward compatibility for components expecting BlockEvents().
//
// NOTE: This channel is populated from Redis pub/sub events, ensuring
// all relayers see the same block progression as the miner.
func (a *RedisBlockClientAdapter) BlockEvents() <-chan client.Block {
	return a.blockEventsCh
}

// Close stops the adapter and waits for goroutine cleanup.
func (a *RedisBlockClientAdapter) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	close(a.blockEventsCh)

	// Close all subscriber channels
	a.subscribersMu.Lock()
	for _, ch := range a.subscribers {
		close(ch)
	}
	a.subscribers = nil
	a.subscribersMu.Unlock()

	a.logger.Info().Msg("redis block client adapter closed")
}

// Subscribe creates a new subscription channel for block events.
// This implements the fan-out pattern expected by SessionLifecycleManager.
// Each subscriber gets their own channel with the specified buffer size.
func (a *RedisBlockClientAdapter) Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock {
	ch := make(chan *localclient.SimpleBlock, bufferSize)

	a.subscribersMu.Lock()
	a.subscribers = append(a.subscribers, ch)
	a.subscribersMu.Unlock()

	// Clean up when context is cancelled
	go func() {
		<-ctx.Done()
		a.removeSubscriber(ch)
	}()

	return ch
}

// publishToSubscribers fans out a block event to all active subscribers.
func (a *RedisBlockClientAdapter) publishToSubscribers(event *BlockEvent) {
	// Convert to SimpleBlock for subscribers
	block := localclient.NewSimpleBlock(event.Height, event.Hash, event.Timestamp)

	a.subscribersMu.RLock()
	defer a.subscribersMu.RUnlock()

	for _, ch := range a.subscribers {
		select {
		case ch <- block:
			// Sent successfully
		default:
			// Channel full, skip (non-blocking)
		}
	}
}

// removeSubscriber removes a subscriber channel from the list.
func (a *RedisBlockClientAdapter) removeSubscriber(ch chan *localclient.SimpleBlock) {
	a.subscribersMu.Lock()
	defer a.subscribersMu.Unlock()

	for i, sub := range a.subscribers {
		if sub == ch {
			// Remove by swapping with last and truncating
			a.subscribers[i] = a.subscribers[len(a.subscribers)-1]
			a.subscribers = a.subscribers[:len(a.subscribers)-1]
			close(ch)
			break
		}
	}
}

// Ensure RedisBlockClientAdapter implements BlockClient interface from poktroll.
//
// Note: Some methods (CommittedBlocksSequence, GetChainVersion) are required by the
// interface but not used in production. They exist solely for interface compliance.
// See individual method documentation for details.
//
// Interface: github.com/pokt-network/poktroll/pkg/client.BlockClient
var _ client.BlockClient = (*RedisBlockClientAdapter)(nil)

// simpleBlock is a minimal implementation of client.Block for cached blocks.
type simpleBlock struct {
	height int64
	hash   []byte
}

func (b *simpleBlock) Height() int64 {
	return b.height
}

func (b *simpleBlock) Hash() []byte {
	return b.hash
}
