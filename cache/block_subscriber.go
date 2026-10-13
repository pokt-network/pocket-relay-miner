package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/client"
)

const (
	blockSubscriberComponentName = "block_subscriber"
)

var _ BlockHeightSubscriber = (*RedisBlockSubscriber)(nil)

// subscriberInfo holds metadata about a subscriber for debugging.
type subscriberInfo struct {
	id         uint64
	ch         chan BlockEvent
	callerFunc string // Function that created the subscriber
	callerFile string // File that created the subscriber
	callerLine int    // Line that created the subscriber
}

// RedisBlockSubscriber implements BlockHeightSubscriber using Redis Pub/Sub.
// It allows multiple Relayer instances to stay synchronized on the current block height.
type RedisBlockSubscriber struct {
	logger      logging.Logger
	store       kv.Store
	blockClient client.BlockClient

	// Subscribers with metadata for debugging
	subscribers   map[uint64]*subscriberInfo
	subscribersMu sync.RWMutex
	nextSubID     atomic.Uint64

	// Current block height and timestamp (cached locally).
	// heightMu guards both currentHeight and currentTime — they are
	// updated atomically from the same BlockEvent in handleBlockEvent so
	// readers get a consistent (height, time) pair.
	currentHeight int64
	currentTime   time.Time
	heightMu      sync.RWMutex

	// Lifecycle
	mu       sync.RWMutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewRedisBlockSubscriber creates a new BlockHeightSubscriber backed by Redis Pub/Sub.
// Uses the Redis client wrapper with KeyBuilder for namespace-aware channel names.
func NewRedisBlockSubscriber(
	logger logging.Logger,
	store kv.Store,
	blockClient client.BlockClient,
) *RedisBlockSubscriber {
	return &RedisBlockSubscriber{
		logger:      logging.ForComponent(logger, logging.ComponentBlockSubscriber),
		store:       store,
		blockClient: blockClient,
		subscribers: make(map[uint64]*subscriberInfo),
	}
}

// Start begins listening for block height updates.
func (s *RedisBlockSubscriber) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("subscriber is closed")
	}

	ctx, s.cancelFn = context.WithCancel(ctx)
	s.mu.Unlock()

	// Initialize the current height from a block client
	if s.blockClient != nil {
		lastBlock := s.blockClient.LastBlock(ctx)
		s.heightMu.Lock()
		s.currentHeight = lastBlock.Height()
		s.heightMu.Unlock()
		currentBlockHeight.Set(float64(s.currentHeight))
	}

	// Subscribe to Redis pub/sub
	s.wg.Add(1)
	go s.subscribeLoop(ctx)

	s.logger.Info().Int64("initial_height", s.currentHeight).Msg("block subscriber started")
	return nil
}

// subscribeLoop listens for block events from Redis Pub/Sub with automatic reconnection.
// Uses exponential backoff reconnection (1s → 2s → 4s → max 30s) to handle Redis disconnections.
func (s *RedisBlockSubscriber) subscribeLoop(ctx context.Context) {
	defer s.wg.Done()

	reconnectLoop := redisutil.NewReconnectionLoop(
		s.logger,
		blockSubscriberComponentName,
		// connectFn: Test Redis connection
		func(ctx context.Context) error {
			return s.store.Ping(ctx)
		},
		// runFn: Subscribe and process block events until disconnect
		func(ctx context.Context) error {
			return s.runBlockPubSubLoop(ctx)
		},
	)

	reconnectLoop.Run(ctx)
}

// runBlockPubSubLoop runs the pub/sub listener for block events until disconnect.
// Returns error to trigger reconnection via the reconnection loop.
func (s *RedisBlockSubscriber) runBlockPubSubLoop(ctx context.Context) error {
	channel := s.store.KB().BlockEventChannel()
	// Returns once the subscription is active.
	sub, err := s.store.Subscribe(ctx, channel)
	if err != nil {
		return fmt.Errorf("failed to subscribe to block channel: %w", err)
	}
	defer func() {
		if err := sub.Close(); err != nil {
			s.logger.Error().Err(err).Msg("failed to close pubsub channel")
		}
	}()

	s.logger.Info().Msg("block pub/sub subscription active")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-sub.Messages():
			// A closed channel is a lost subscription (Redis disconnected).
			if !ok {
				return fmt.Errorf("pub/sub channel closed")
			}

			var event BlockEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				s.logger.Warn().Err(err).Int("payload_bytes", len(msg.Payload)).Msg("invalid block event")
				continue
			}

			s.handleBlockEvent(event)
		}
	}
}

// advanceClock moves the current height and block time to event's when it is
// newer. It is the chain clock the tx client anchors timeouts on, so the
// adapter's latest-height poll moves it too: a silent pub/sub channel must not
// leave transactions signed against a stale block time.
func (s *RedisBlockSubscriber) advanceClock(event BlockEvent) {
	s.heightMu.Lock()
	defer s.heightMu.Unlock()
	if event.Height > s.currentHeight {
		s.currentHeight = event.Height
		s.currentTime = event.Timestamp
		currentBlockHeight.Set(float64(s.currentHeight))
	}
}

// handleBlockEvent processes a received block event.
func (s *RedisBlockSubscriber) handleBlockEvent(event BlockEvent) {
	// Update the current height and timestamp together. We only advance
	// on strictly-newer heights so out-of-order events (e.g. duplicate
	// pub/sub deliveries or a late retry) can't rewind the observed
	// block time that downstream tx-timeout anchoring depends on.
	s.advanceClock(event)

	blockEventsReceived.Inc()

	// Notify all subscribers
	s.subscribersMu.RLock()
	defer s.subscribersMu.RUnlock()

	for _, info := range s.subscribers {
		select {
		case info.ch <- event:
		default:
			// Channel full, skip (subscriber is slow)
			s.logger.Error().
				Uint64("subscriber_id", info.id).
				Int64("height", event.Height).
				Str("caller_func", info.callerFunc).
				Str("caller_file", info.callerFile).
				Int("caller_line", info.callerLine).
				Msg("subscriber channel full, dropping event")
		}
	}
}

// Subscribe returns a channel that receives new block heights.
func (s *RedisBlockSubscriber) Subscribe(ctx context.Context) <-chan BlockEvent {
	// Capture caller info for debugging
	_, file, line, _ := runtime.Caller(1)
	pc, _, _, _ := runtime.Caller(1)
	callerFunc := "unknown"
	if fn := runtime.FuncForPC(pc); fn != nil {
		callerFunc = fn.Name()
	}

	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()

	// Generate unique subscriber ID
	subID := s.nextSubID.Add(1)

	// Create a buffered channel (2000 blocks handles any reasonable backlog)
	ch := make(chan BlockEvent, 2000)

	// Store subscriber info
	info := &subscriberInfo{
		id:         subID,
		ch:         ch,
		callerFunc: callerFunc,
		callerFile: file,
		callerLine: line,
	}
	s.subscribers[subID] = info

	// Auto-cleanup on context cancellation
	go func() {
		<-ctx.Done()
		s.unsubscribe(subID)
	}()

	s.logger.Debug().
		Uint64("subscriber_id", subID).
		Str("caller_func", callerFunc).
		Str("caller_file", file).
		Int("caller_line", line).
		Msg("new block subscriber registered")

	return ch
}

// unsubscribe removes a subscriber channel.
func (s *RedisBlockSubscriber) unsubscribe(subID uint64) {
	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()

	if info, exists := s.subscribers[subID]; exists {
		close(info.ch)
		delete(s.subscribers, subID)
		s.logger.Debug().
			Uint64("subscriber_id", subID).
			Str("caller_func", info.callerFunc).
			Str("caller_file", info.callerFile).
			Int("caller_line", info.callerLine).
			Msg("block subscriber unregistered")
	}
}

// PublishBlockHeight publishes a new block height to all subscribers.
func (s *RedisBlockSubscriber) PublishBlockHeight(ctx context.Context, event BlockEvent) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return fmt.Errorf("subscriber is closed")
	}
	s.mu.RUnlock()

	return publishBlockEvent(ctx, s.logger, s.store, event)
}

// publishBlockEvent writes one block event onto the shared channel. It is the
// single implementation of the block-event wire format: both the subscriber
// (which can also publish) and the publish-only RedisBlockPublisher go through
// here, so the channel name, the JSON shape and the counter cannot drift apart
// between the two.
func publishBlockEvent(
	ctx context.Context,
	logger logging.Logger,
	store kv.Store,
	event BlockEvent,
) error {
	// Set the timestamp if not set
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	kb := store.KB()
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal block event: %w", err)
	}

	// The record and the latest height are written BEFORE the event, so a
	// consumer woken by the event finds the hash whenever its write landed.
	// Each write stands alone, not a MULTI: with Redis at maxmemory under
	// noeviction a MULTI holding a SET is aborted whole, and the event must go
	// out even then -- it is what moves claims and proofs, which free the
	// memory. A consumer that finds no record reads the hash from its own node.
	// On Redis the three go in one round trip (a pipeline), on every block.
	entries := []kv.Entry{
		{Key: kb.BlockHashAtHeightKey(event.Height), Value: data, TTL: blockRecordTTL},
		{Key: kb.BlockLatestHeightKey(), Value: []byte(strconv.FormatInt(event.Height, 10)), TTL: blockRecordTTL},
	}
	var writeErrs []error
	if pipelined, ok := store.(kv.PipelinedPublisher); ok {
		writeErrs, err = pipelined.SetEachAndPublish(ctx, entries, kb.BlockEventChannel(), data)
	} else {
		for _, e := range entries {
			writeErrs = append(writeErrs, store.Set(ctx, e.Key, e.Value, e.TTL))
		}
		err = store.Publish(ctx, kb.BlockEventChannel(), data)
	}
	if err != nil {
		return fmt.Errorf("failed to publish block event: %w", err)
	}
	for _, writeErr := range writeErrs {
		if writeErr != nil {
			logger.Warn().Err(writeErr).Int64("height", event.Height).
				Msg("failed to record the published block; consumers will read its hash from their own node")
		}
	}

	blockEventsPublished.Inc()

	logger.Debug().Int64("height", event.Height).Msg("published block event")
	return nil
}

// blockRecordTTL is how long a published block's record lives. A day is far
// longer than any claim or proof window a consumer can still be waiting on, and
// one small key per block is cheap.
const blockRecordTTL = 24 * time.Hour

// readBlockRecord reads the leader's record of the block at height. found is
// false when the leader has not published that height (yet).
func readBlockRecord(ctx context.Context, store kv.Store, height int64) (event BlockEvent, found bool, err error) {
	data, err := store.Get(ctx, store.KB().BlockHashAtHeightKey(height))
	if errors.Is(err, kv.ErrNotFound) {
		return BlockEvent{}, false, nil
	}
	if err != nil {
		return BlockEvent{}, false, fmt.Errorf("failed to read block record at height %d: %w", height, err)
	}
	if err = json.Unmarshal(data, &event); err != nil {
		return BlockEvent{}, false, fmt.Errorf("failed to decode block record at height %d: %w", height, err)
	}
	return event, true, nil
}

// readLatestPublishedHeight reads the highest height the leader has published.
// found is false when nothing has been published.
func readLatestPublishedHeight(ctx context.Context, store kv.Store) (height int64, found bool, err error) {
	data, err := store.Get(ctx, store.KB().BlockLatestHeightKey())
	if errors.Is(err, kv.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to read latest published height: %w", err)
	}
	if height, err = strconv.ParseInt(string(data), 10, 64); err != nil {
		return 0, false, fmt.Errorf("failed to read latest published height: %w", err)
	}
	return height, true, nil
}

// redisBlockPublisherComponentName labels this endpoint's logs. It is NOT
// ComponentBlockSubscriber: on the leader, a publish line tagged block_subscriber
// tells an operator reading logs during an incident that the leader is
// consuming, which is the exact wrong conclusion and the confusion this type
// exists to end. It is not blockPublisherComponentName either -- that belongs to
// BlockPublisher, the chain watcher one layer above.
const redisBlockPublisherComponentName = "redis_block_publisher"

// RedisBlockPublisher publishes block events onto the shared channel and does
// NOTHING ELSE. It exists because the leader needs to publish but has no reason
// to receive: RedisBlockSubscriber.Start always spawns a pub/sub receive loop,
// so using one as a publisher made the leader consume every event it published
// and hand it to zero subscribers. Measured on a resting localnet before this
// type existed: the leader reported 480 received against 240 published, exactly
// double, while a follower reported 240 — which put a per-process floor under
// ha_cache_block_events_received_total that no gate could assert against.
//
// There is no Start: a publisher has no background work. Close is present so
// callers can treat it like every other component they own.
type RedisBlockPublisher struct {
	logger logging.Logger
	store  kv.Store

	mu     sync.RWMutex
	closed bool
}

// NewRedisBlockPublisher creates a publish-only block event endpoint.
func NewRedisBlockPublisher(logger logging.Logger, store kv.Store) *RedisBlockPublisher {
	return &RedisBlockPublisher{
		logger: logging.ForComponent(logger, redisBlockPublisherComponentName),
		store:  store,
	}
}

// PublishBlockHeight publishes a new block height to all subscribers.
func (p *RedisBlockPublisher) PublishBlockHeight(ctx context.Context, event BlockEvent) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return fmt.Errorf("publisher is closed")
	}
	p.mu.RUnlock()

	return publishBlockEvent(ctx, p.logger, p.store, event)
}

// Close marks the publisher closed. Idempotent.
func (p *RedisBlockPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// GetCurrentHeight returns the current block height (cached locally).
func (s *RedisBlockSubscriber) GetCurrentHeight() int64 {
	s.heightMu.RLock()
	defer s.heightMu.RUnlock()
	return s.currentHeight
}

// LatestBlockTime returns the timestamp of the most recent block this
// subscriber has observed. Returns the zero time.Time if no block event
// has been processed yet (startup race).
//
// This is the anchor used by tx.TxClient when building unordered TX
// timeoutTimestamps: cosmos-sdk's ante handler compares timeoutTimestamp
// against ctx.BlockTime() (the validator's latest committed block time)
// and rejects with `unordered tx ttl exceeds 10m0s` if the delta exceeds
// 10 minutes. Anchoring on wall-clock time instead of block time made
// that delta unpredictable whenever the chain fell behind wall clock
// (block-time lag, not host clock drift), which is what caused the
// 2026-session loss on breeze.
func (s *RedisBlockSubscriber) LatestBlockTime() time.Time {
	s.heightMu.RLock()
	defer s.heightMu.RUnlock()
	return s.currentTime
}

// SeedBlockTime sets the block time read off the chain at startup, so the
// process anchors its transactions on chain time before the first block event
// reaches it. In the process that wins the election the leader that publishes
// those events only starts later, so without this the anchor is the zero time
// for as long as a block lasts.
//
// It seeds the TIME only, never currentHeight: handleBlockEvent advances on a
// strictly greater height, so a seed from a node one block ahead would make the
// events that follow look old and be dropped.
//
// An event always wins over a seed, whichever arrives first: the seed applies
// only while no event has been seen (currentHeight == 0), so a seed that lost a
// race against the pub/sub cannot rewind the time an event brought. It reports
// whether it was applied.
func (s *RedisBlockSubscriber) SeedBlockTime(blockTime time.Time) bool {
	s.heightMu.Lock()
	defer s.heightMu.Unlock()
	if s.currentHeight > 0 || blockTime.IsZero() {
		return false
	}
	s.currentTime = blockTime
	return true
}

// Close gracefully shuts down the subscriber.
func (s *RedisBlockSubscriber) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	if s.cancelFn != nil {
		s.cancelFn()
	}

	// Close all subscriber channels
	s.subscribersMu.Lock()
	for _, info := range s.subscribers {
		close(info.ch)
	}
	s.subscribers = nil
	s.subscribersMu.Unlock()

	s.wg.Wait()

	s.logger.Info().Msg("block subscriber closed")
	return nil
}
