package cache

import (
	"context"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-version"
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
		logger:          logging.ForComponent(logger, "redis_block_client_adapter"),
		redisSubscriber: redisSubscriber,
		blockEventsCh:   make(chan client.Block, 100),
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

// GetChainVersion returns nil - chain version is not cached.
// Relayers receive block events from Redis and don't need chain version.
func (a *RedisBlockClientAdapter) GetChainVersion() *version.Version {
	return nil
}

// CommittedBlocksSequence returns nil - not implemented.
// Relayers use event-driven updates from Redis, not observable sequences.
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
	a.logger.Info().Msg("redis block client adapter closed")
}

// Ensure adapter implements BlockClient interface
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
