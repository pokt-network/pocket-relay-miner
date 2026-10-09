package cache

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-version"
	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

// RedisBlockClientAdapter adapts RedisBlockSubscriber to implement client.BlockClient.
// This enables relayer components to receive block events from Redis pub/sub
// (synchronized with the miner's view) while satisfying the client.BlockClient interface.
//
// Used in relayers and miner workers to:
//  1. Subscribe to Redis pub/sub for block events from the leader
//  2. Poll the leader's latest published height, so a silent pub/sub channel
//     costs latency and not blocks
//  3. Maintain local cache of last block (for LastBlock() calls)
//  4. Provide BlockEvents() channel for components expecting it
//  5. Serve a block at a height (for proof generation): the leader's record,
//     or this process's own node when the record is missing
//
// The current height comes from the leader: two processes asking two nodes
// would hold two versions of the present. The one exception is the startup
// seed (SeedHeight), read from this process's own node before the first event. A block's hash at a height is
// different, immutable once the block exists, so reading it from any node
// that has it gives the same answer.
type RedisBlockClientAdapter struct {
	logger          logging.Logger
	redisSubscriber *RedisBlockSubscriber
	blockReader     BlockAtHeightReader
	pollInterval    time.Duration

	// deliverMu orders deliveries: the event loop and the latest-height poll
	// both deliver, and a lower height sent after a higher one would move a
	// relayer's height backwards.
	deliverMu     sync.Mutex
	lastBlock     atomic.Pointer[simpleBlock]
	blockEventsCh chan client.Block
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup

	// blockEventsRequested is set the first time BlockEvents() is called, i.e.
	// when a component actually wires up the BlockEvents() channel as its block
	// source (the relayer does; the miner never does). Until then, events are
	// discarded on arrival rather than buffered into blockEventsCh — otherwise the
	// 2000-deep buffer slowly fills on the miner (nobody drains it) and emits a
	// misleading "channel full, dropping event" warning every block. Guarding on
	// an actual consumer keeps that RAM and that noise off processes that don't use
	// BlockEvents() at all.
	blockEventsRequested atomic.Bool

	// Fan-out subscribers for Subscribe() method
	subscribersMu sync.RWMutex
	subscribers   []chan *localclient.SimpleBlock

	// afterLoadHook, when set, runs in advance between reading the held block
	// and replacing it: the window where another writer's higher height is lost
	// unless the replace is a compare-and-swap. Nil outside tests.
	afterLoadHook func()

	// afterAdvanceHook, when set, runs in deliver after a height was taken and
	// before it is sent: the window where a second deliverer's higher height
	// would overtake it. Nil outside tests.
	afterAdvanceHook func(height int64)
}

// BlockAtHeightReader reads a block at a height from a node, retrying while
// the node does not have it yet. client.BlockReader is the production one.
type BlockAtHeightReader interface {
	BlockAtHeight(ctx context.Context, height int64) (client.Block, error)
}

// latestHeightPollInterval paces the adapter's read of the leader's latest
// published height.
const latestHeightPollInterval = 1 * time.Second

// NewRedisBlockClientAdapter creates an adapter over the leader's published
// blocks. The adapter subscribes to Redis events and implements
// client.BlockClient interface.
//
// blockReader serves a block hash the leader's record lacks; nil (the relayer)
// makes such a read an error.
func NewRedisBlockClientAdapter(
	logger logging.Logger,
	redisSubscriber *RedisBlockSubscriber,
	blockReader BlockAtHeightReader,
) *RedisBlockClientAdapter {
	return &RedisBlockClientAdapter{
		logger:          logging.ForComponent(logger, logging.ComponentRedisBlockClientAdapter),
		redisSubscriber: redisSubscriber,
		blockReader:     blockReader,
		pollInterval:    latestHeightPollInterval,
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

				if !a.deliver(event) {
					return
				}
			}
		}
	}()

	a.wg.Add(1)
	go logging.RecoverGoRoutine(a.logger, "redis_block_client_adapter_poll", a.pollLatestHeight)(a.ctx)

	a.logger.Info().Msg("redis block client adapter started")
	return nil
}

// deliver takes a block the leader published, from the event channel or the
// latest-height poll, whichever brought it first. It returns false when the
// adapter is stopping.
func (a *RedisBlockClientAdapter) deliver(event BlockEvent) bool {
	a.deliverMu.Lock()
	defer a.deliverMu.Unlock()
	// Only a height above the one held moves the adapter; anything
	// else is neither stored nor forwarded. See advance.
	block, ignored := a.advance(event.Height, event.Hash)
	if ignored != "" {
		blockEventsIgnored.WithLabelValues(ignored).Inc()
		return true
	}
	if a.afterAdvanceHook != nil {
		a.afterAdvanceHook(event.Height)
	}

	// Forward to blockEventsCh ONLY if a component has wired up
	// BlockEvents() as a consumer (relayer yes, miner no). With no
	// consumer the channel would just fill and emit a misleading "full,
	// dropping event" warning forever, so discard on arrival instead —
	// no buffer growth, no noise. See blockEventsRequested.
	if a.blockEventsRequested.Load() {
		// Non-blocking send to prevent the Redis event loop from blocking.
		select {
		case a.blockEventsCh <- block:
			// Event forwarded successfully
		case <-a.ctx.Done():
			return false
		default:
			// A consumer is attached but not draining fast enough. This is
			// a real wedged-consumer signal, so make it loud (metric + warn)
			// rather than a silent drop.
			blockEventsDropped.WithLabelValues("block_events").Inc()
			a.logger.Warn().
				Int64("height", event.Height).
				Msg("block events channel full, dropping event (consumer not draining)")
		}
	}

	// Fan-out to Subscribe() subscribers
	a.publishToSubscribers(&event)
	return true
}

// pollLatestHeight reads the leader's latest published height once per
// interval and delivers it when the event channel has not. Redis pub/sub is
// fire-and-forget: a subscriber that lost its connection, or was too slow,
// misses events and is never told.
func (a *RedisBlockClientAdapter) pollLatestHeight(ctx context.Context) {
	defer a.wg.Done()
	redisClient := a.redisSubscriber.store
	if redisClient == nil {
		return
	}
	ticker := time.NewTicker(a.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		latest, found, err := readLatestPublishedHeight(ctx, redisClient)
		if err != nil {
			a.logger.Debug().Err(err).Msg("latest published height poll failed")
			continue
		}
		if !found || latest <= a.LastBlock(ctx).Height() {
			continue
		}
		event, found, err := readBlockRecord(ctx, redisClient, latest)
		if err != nil || !found {
			continue
		}
		a.redisSubscriber.advanceClock(event)
		if !a.deliver(event) {
			return
		}
	}
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

// Why a block event did not move the height. Bounded: two values.
const (
	blockIgnoredRepeated = "repeated"
	blockIgnoredRewound  = "rewound"
)

// advance makes height the last block only when it is above the height already
// held, and otherwise says why not. The subscriber forwards every event it
// receives, including a repeated or late delivery, and a lower height taken as
// current makes the miner admit relays for a session whose claim it had already
// stopped taking relays for.
// A higher height is taken whatever the gap: a jump is also what an operator
// switching from a lagging node to a synced one looks like.
//
// Compare-and-swap, not load-then-store: the startup seed and the event loop
// both write, and a store landing between another writer's load and its store
// would lose the higher of the two.
func (a *RedisBlockClientAdapter) advance(height int64, hash []byte) (*simpleBlock, string) {
	next := &simpleBlock{height: height, hash: hash}
	for {
		current := a.lastBlock.Load()
		if a.afterLoadHook != nil {
			a.afterLoadHook()
		}
		var currentHeight int64
		if current != nil {
			currentHeight = current.height
		}
		switch {
		case height == currentHeight:
			return nil, blockIgnoredRepeated
		case height < currentHeight:
			return nil, blockIgnoredRewound
		}
		if a.lastBlock.CompareAndSwap(current, next) {
			return next, ""
		}
	}
}

// SeedHeight sets the last block from a height read off the chain, so the
// process knows where the chain is before the first block event reaches it. It
// follows the same rule as an event, so a seed below what an event already
// brought is dropped, and it reports whether it moved the height. The seed
// carries no hash: the block hashes the miner uses, the proof seeds, are read at
// their own height through GetBlockAtHeight.
func (a *RedisBlockClientAdapter) SeedHeight(height int64) bool {
	_, ignored := a.advance(height, nil)
	return ignored == ""
}

// GetBlockAtHeight returns the block at height with its canonical hash, which
// must match what the validator stores via ctx.HeaderHash() in
// StoreBlockHash(): the leader's record when there is one, otherwise this
// process's own node. It does not wait for the leader. A missing record was
// lost (a refused write, a deleted key) or never written: a worker's startup
// seed comes from its own node and can be ahead of what the leader has
// published. Either way the node has the answer.
func (a *RedisBlockClientAdapter) GetBlockAtHeight(ctx context.Context, height int64) (client.Block, error) {
	if held := a.lastBlock.Load(); held != nil && held.height == height && len(held.hash) > 0 {
		return held, nil
	}
	if redisClient := a.redisSubscriber.store; redisClient != nil {
		event, found, err := readBlockRecord(ctx, redisClient, height)
		if err == nil && found && len(event.Hash) > 0 {
			return &simpleBlock{height: event.Height, hash: event.Hash}, nil
		}
		if err != nil {
			a.logger.Debug().Err(err).Int64("height", height).Msg("block record read failed; reading the node")
		}
	}
	if a.blockReader == nil {
		return nil, fmt.Errorf("no record of the block at height %d and no node to read it from", height)
	}
	a.logger.Debug().Int64("height", height).Msg("no record of this block; reading it from this process's node")
	return a.blockReader.BlockAtHeight(ctx, height)
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
// Calling it marks BlockEvents() as having a consumer, which switches on
// forwarding in the Redis event loop (see blockEventsRequested). Until the
// first call, events are discarded on arrival rather than buffered.
// This provides backward compatibility for components expecting BlockEvents().
//
// NOTE: This channel is populated from Redis pub/sub events, ensuring
// all relayers see the same block progression as the miner.
func (a *RedisBlockClientAdapter) BlockEvents() <-chan client.Block {
	// Mark that a consumer exists so the Redis event loop starts forwarding to
	// blockEventsCh (idempotent; safe to call repeatedly, e.g. from a consumer
	// loop's select case).
	a.blockEventsRequested.Store(true)
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
//
// Short-circuits when there are no subscribers so the SimpleBlock allocation
// is skipped entirely. The relayer binary wires up BlockEvents() as its only
// block consumer and never calls adapter.Subscribe(), so on the relayer this
// fast path is the common case — every block would otherwise allocate a
// SimpleBlock that nobody reads.
func (a *RedisBlockClientAdapter) publishToSubscribers(event *BlockEvent) {
	a.subscribersMu.RLock()
	if len(a.subscribers) == 0 {
		a.subscribersMu.RUnlock()
		return
	}

	// Convert to SimpleBlock for subscribers. Safe to allocate now that we
	// know at least one subscriber will (try to) receive it.
	block := localclient.NewSimpleBlock(event.Height, event.Hash, event.Timestamp)

	dropped := 0
	for _, ch := range a.subscribers {
		select {
		case ch <- block:
			// Sent successfully
		default:
			// Channel full. A subscriber that drains on arrival (the coalescing
			// block loop does) keeps its channel clear, so reaching here means a
			// subscriber is wedged — the silent stall that stopped claim/proof
			// windows from firing at high supplier counts. Count it; we still do
			// not block the producer.
			dropped++
		}
	}
	a.subscribersMu.RUnlock()

	// Surface drops loudly, but OUTSIDE the RLock and once per block (not once per
	// wedged subscriber): logging under the lock would hold it across log I/O and
	// block Subscribe()/removeSubscriber(), and a permanently-wedged consumer would
	// otherwise flood the log every block. The counter still carries the per-drop
	// rate.
	if dropped > 0 {
		blockEventsDropped.WithLabelValues("fanout").Add(float64(dropped))
		a.logger.Warn().
			Int64("height", event.Height).
			Int("subscribers_dropped", dropped).
			Msg("block fan-out subscriber channel full, dropping event (subscriber not draining)")
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
