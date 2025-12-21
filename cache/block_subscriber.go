package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

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
	redisClient *redisutil.Client
	blockClient client.BlockClient

	// Subscribers with metadata for debugging
	subscribers   map[uint64]*subscriberInfo
	subscribersMu sync.RWMutex
	nextSubID     atomic.Uint64

	// Current block height (cached locally)
	currentHeight int64
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
	redisClient *redisutil.Client,
	blockClient client.BlockClient,
) *RedisBlockSubscriber {
	return &RedisBlockSubscriber{
		logger:      logging.ForComponent(logger, logging.ComponentBlockSubscriber),
		redisClient: redisClient,
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
			return s.redisClient.Ping(ctx).Err()
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
	channel := s.redisClient.KB().BlockEventChannel()
	pubsub := s.redisClient.Subscribe(ctx, channel)
	defer func() {
		if err := pubsub.Close(); err != nil {
			s.logger.Error().Err(err).Msg("failed to close pubsub channel")
		}
	}()

	// Verify subscription
	if _, err := pubsub.Receive(ctx); err != nil {
		return fmt.Errorf("failed to subscribe to block channel: %w", err)
	}

	s.logger.Info().Msg("block pub/sub subscription active")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-pubsub.Channel():
			// Check if msg is nil (pubsub channel is closed = Redis disconnected)
			if msg == nil {
				return fmt.Errorf("pub/sub channel closed")
			}

			var event BlockEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				s.logger.Error().Err(err).Str("payload", msg.Payload).Msg("invalid block event")
				continue
			}

			s.handleBlockEvent(event)
		}
	}
}

// handleBlockEvent processes a received block event.
func (s *RedisBlockSubscriber) handleBlockEvent(event BlockEvent) {
	// Update the current height
	s.heightMu.Lock()
	if event.Height > s.currentHeight {
		s.currentHeight = event.Height
		currentBlockHeight.Set(float64(s.currentHeight))
	}
	s.heightMu.Unlock()

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

	// Set the timestamp if not set
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	channel := s.redisClient.KB().BlockEventChannel()

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal block event: %w", err)
	}

	if err = s.redisClient.Publish(ctx, channel, data).Err(); err != nil {
		return fmt.Errorf("failed to publish block event: %w", err)
	}

	blockEventsPublished.Inc()

	s.logger.Debug().Int64("height", event.Height).Msg("published block event")
	return nil
}

// GetCurrentHeight returns the current block height (cached locally).
func (s *RedisBlockSubscriber) GetCurrentHeight() int64 {
	s.heightMu.RLock()
	defer s.heightMu.RUnlock()
	return s.currentHeight
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
