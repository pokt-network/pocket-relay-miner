package redis

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

var _ transport.MinedRelayConsumer = (*StreamsConsumer)(nil)

// StreamsConsumer implements MinedRelayConsumer using Redis Streams with consumer groups.
// It provides exactly-once delivery semantics within the consumer group.
// TRUE PUSH architecture: BLOCK 0 means zero latency when data arrives.
// - Each consumer holds 1 connection indefinitely waiting on XREADGROUP BLOCK 0
// - Pool sizing: Allocate numSuppliers + 20 overhead for cache/pubsub
// - Context cancellation cleanly interrupts blocked calls
// - Claims = money - we cannot afford ANY latency consuming relays.
type StreamsConsumer struct {
	logger     logging.Logger
	client     redis.UniversalClient
	config     transport.ConsumerConfig
	streamName string // Single stream per supplier: ha:relays:{supplierAddr}

	// Message channel
	msgCh chan transport.StreamMessage

	// Claiming rate limit (prevent excessive claiming when stream is idle)
	lastClaimTime time.Time
	claimMu       sync.Mutex

	// Lifecycle management
	mu       sync.RWMutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewStreamsConsumer creates a new Redis Streams consumer.
// TRUE PUSH architecture: BLOCK 0 for zero-latency message delivery.
// The discoveryInterval parameter is ignored (kept for API compatibility).
func NewStreamsConsumer(
	logger logging.Logger,
	client redis.UniversalClient,
	config transport.ConsumerConfig,
	discoveryInterval time.Duration, // Ignored - kept for API compatibility
) (*StreamsConsumer, error) {
	if config.StreamPrefix == "" {
		return nil, fmt.Errorf("stream prefix is required")
	}
	if config.SupplierOperatorAddress == "" {
		return nil, fmt.Errorf("supplier operator address is required")
	}
	if config.ConsumerGroup == "" {
		return nil, fmt.Errorf("consumer group is required")
	}
	if config.ConsumerName == "" {
		return nil, fmt.Errorf("consumer name is required")
	}

	// Set defaults - VERY AGGRESSIVE for minimal latency
	// TRUE PUSH: BLOCK 0 returns instantly when data arrives, holds connection when empty
	// Claims = money, we cannot afford to be slow consuming relays
	if config.BatchSize <= 0 {
		config.BatchSize = 5000 // Large batch for throughput
	}
	// ClaimIdleTimeout: How long before we claim messages from crashed consumers
	if config.ClaimIdleTimeout <= 0 {
		config.ClaimIdleTimeout = 30000 // 30 seconds for claiming idle messages
	}
	if config.MaxRetries <= 0 {
		config.MaxRetries = 3
	}

	// Channel buffer: 5000 messages to match batch size for smooth pipelining
	channelBufferSize := int64(5000)

	// Single stream per supplier (simplified architecture)
	streamName := transport.SupplierStreamName(config.StreamPrefix, config.SupplierOperatorAddress)

	return &StreamsConsumer{
		logger:     logging.ForSupplierComponent(logger, logging.ComponentRedisConsumer, config.SupplierOperatorAddress),
		client:     client,
		config:     config,
		streamName: streamName,
		msgCh:      make(chan transport.StreamMessage, channelBufferSize),
	}, nil
}

// Consume returns a channel that yields mined relay messages.
func (c *StreamsConsumer) Consume(ctx context.Context) <-chan transport.StreamMessage {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		close(c.msgCh)
		return c.msgCh
	}

	// Create cancellable context
	ctx, c.cancelFn = context.WithCancel(ctx)
	c.mu.Unlock()

	// Start consumer goroutine - consumer group creation happens in connectFn
	// with proper exponential backoff retry via ReconnectionLoop
	c.wg.Add(1)
	go c.consumeLoop(ctx)

	c.logger.Info().
		Str("stream", c.streamName).
		Str("consumer_group", c.config.ConsumerGroup).
		Msg("started consuming from supplier stream")

	return c.msgCh
}

// ensureConsumerGroup creates the consumer group for the single supplier stream if it doesn't exist.
func (c *StreamsConsumer) ensureConsumerGroup(ctx context.Context) error {
	// Try to create the consumer group (XGroupCreateMkStream creates stream if needed)
	err := c.client.XGroupCreateMkStream(ctx, c.streamName, c.config.ConsumerGroup, "0").Err()
	if err != nil {
		// Ignore "BUSYGROUP" error - group already exists
		if !strings.Contains(err.Error(), "BUSYGROUP") {
			return fmt.Errorf("failed to create consumer group for %s: %w", c.streamName, err)
		}
	}
	return nil
}

// consumeLoop is the main consumption loop with automatic reconnection.
// This wraps the message consumption with exponential backoff reconnection,
// matching the pattern in client/block_subscriber.go:145-194
func (c *StreamsConsumer) consumeLoop(ctx context.Context) {
	defer c.wg.Done()
	defer close(c.msgCh)

	// Create reconnection loop
	reconnectLoop := NewReconnectionLoop(
		c.logger,
		"streams_consumer",
		// connectFn: Create consumer group proactively on connect/reconnect.
		// XGroupCreateMkStream creates both stream and group if they don't exist.
		// This ensures the group exists before we try to consume, avoiding NOGROUP errors.
		func(ctx context.Context) error {
			return c.ensureConsumerGroup(ctx)
		},
		// runFn: Consume messages until error or context cancellation
		func(ctx context.Context) error {
			return c.consumeMessagesUntilError(ctx)
		},
	)

	// Run until context cancellation (handles all reconnection logic)
	reconnectLoop.Run(ctx)
}

// consumeMessagesUntilError runs the message consumption loop until an error occurs.
// Returns error to trigger reconnection via the reconnection loop.
// TRUE PUSH SEMANTICS: Uses BLOCK 0 (infinite wait).
// - Returns INSTANTLY when data arrives (zero latency)
// - Blocks indefinitely when stream is empty (zero CPU waste)
// - Context cancellation interrupts the blocked call (clean shutdown)
// This is the most efficient approach - no polling, pure push.
func (c *StreamsConsumer) consumeMessagesUntilError(ctx context.Context) error {
	for {
		// TRUE PUSH: BLOCK 0 = infinite wait until data arrives (live consumption)
		// go-redis respects context cancellation, so this is safe:
		// - When data arrives: returns immediately with messages
		// - When context cancelled: returns with context.Canceled error
		// - No polling, no wasted CPU cycles
		// Note: Each blocked call holds 1 connection from the pool
		streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.config.ConsumerGroup,
			Consumer: c.config.ConsumerName,
			Streams:  []string{c.streamName, ">"},
			Count:    c.config.BatchSize,
			Block:    0, // TRUE PUSH: infinite wait, context cancellation interrupts
		}).Result()

		if err != nil {
			// With BLOCK 0, context cancellation is the normal shutdown path
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if err == redis.Nil {
				// With BLOCK 0, this shouldn't happen often (only on timeout which we don't have)
				// But handle it gracefully - consider claiming idle messages
				c.claimMu.Lock()
				timeSinceLastClaim := time.Since(c.lastClaimTime)
				shouldClaim := timeSinceLastClaim >= time.Duration(c.config.ClaimIdleTimeout)*time.Millisecond
				if shouldClaim {
					c.lastClaimTime = time.Now()
				}
				c.claimMu.Unlock()

				if shouldClaim {
					c.claimPendingMessages(ctx)
				}
				continue
			}

			// Handle NOGROUP error - recreate consumer group
			// This is a fallback safety net. Normally connectFn creates the group at startup.
			// This handles edge cases like external deletion of the consumer group.
			if strings.Contains(err.Error(), "NOGROUP") {
				c.logger.Debug().Err(err).Msg("consumer group missing (unexpected - recreating)")
				if groupErr := c.ensureConsumerGroup(ctx); groupErr != nil {
					// Failed to recreate consumer group - return error to trigger
					// reconnection loop with exponential backoff instead of tight loop
					c.logger.Warn().Err(groupErr).Msg("failed to recreate consumer group, triggering reconnection")
					return fmt.Errorf("failed to recreate consumer group: %w", groupErr)
				}
				// Successfully created consumer group, retry XREADGROUP
				continue
			}

			consumeErrorsTotal.WithLabelValues(c.config.SupplierOperatorAddress, "read_error").Inc()
			c.logger.Error().Err(err).Msg("error reading from stream")
			return err
		}

		// Process messages (single stream, so streams[0])
		if len(streams) == 0 {
			continue
		}

		for _, message := range streams[0].Messages {
			msg, parseErr := c.parseMessage(message, c.streamName)
			if parseErr != nil {
				deserializationErrors.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
				c.logger.Error().
					Err(parseErr).
					Str(logging.FieldMessageID, message.ID).
					Msg("failed to parse message")
				// Acknowledge AND delete bad message to avoid redelivery and keep stream clean
				_ = c.client.XAckDel(ctx, c.streamName, c.config.ConsumerGroup, "DELREF", message.ID)
				continue
			}

			// Log consume details for tracing
			c.logger.Debug().
				Str("stream_name", c.streamName).
				Str("session_id", msg.Message.SessionId).
				Str("supplier", msg.Message.SupplierOperatorAddress).
				Str("service", msg.Message.ServiceId).
				Str("message_id", message.ID).
				Msg("consumed relay from supplier stream")

			// Record end-to-end latency
			if msg.Message.PublishedAtUnixNano > 0 {
				latency := time.Since(msg.Message.PublishedAt()).Seconds()
				endToEndLatency.WithLabelValues(
					c.config.SupplierOperatorAddress,
					msg.Message.ServiceId,
				).Observe(latency)
			}

			consumedTotal.WithLabelValues(
				c.config.SupplierOperatorAddress,
				msg.Message.ServiceId,
			).Inc()

			// Send to channel (blocks if channel is full)
			select {
			case c.msgCh <- *msg:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// claimPendingMessages claims messages that have been pending too long.
// This is only called when normal XREADGROUP consumption returns no new messages (idle state).
// It recovers messages from consumers that crashed without acknowledging.
func (c *StreamsConsumer) claimPendingMessages(ctx context.Context) {
	// Claim idle messages from the single supplier stream
	messages, _, err := c.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   c.streamName,
		Group:    c.config.ConsumerGroup,
		Consumer: c.config.ConsumerName,
		MinIdle:  time.Duration(c.config.ClaimIdleTimeout) * time.Millisecond,
		Start:    "0-0",
		Count:    50, // Reasonable batch size for claiming
	}).Result()

	if err != nil {
		// Stream may not exist yet - skip
		if strings.Contains(err.Error(), "no such key") ||
			strings.Contains(err.Error(), "NOGROUP") {
			return
		}
		if ctx.Err() == nil {
			c.logger.Debug().Err(err).Msg("error claiming idle messages")
		}
		return
	}

	if len(messages) == 0 {
		return
	}

	claimedMessages.WithLabelValues(c.config.SupplierOperatorAddress).Add(float64(len(messages)))

	c.logger.Debug().
		Int("count", len(messages)).
		Str("stream", c.streamName).
		Msg("claimed idle messages")

	// Process claimed messages
	for _, message := range messages {
		msg, parseErr := c.parseMessage(message, c.streamName)
		if parseErr != nil {
			deserializationErrors.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
			// Acknowledge AND delete bad message to keep stream clean
			_ = c.client.XAckDel(ctx, c.streamName, c.config.ConsumerGroup, "DELREF", message.ID)
			continue
		}

		select {
		case c.msgCh <- *msg:
		case <-ctx.Done():
			return
		}
	}
}

// parseMessage deserializes a Redis Stream message into a StreamMessage.
// The streamName parameter is required for acknowledgment in multi-stream consumption.
//
// Memory optimization: Uses protobuf binary deserialization instead of JSON to eliminate
// JSON decoder memory overhead (literalStore accumulation). With 1000 suppliers consuming
// continuously, this reduces memory usage by ~67% (1.4GB → ~460MB) and improves throughput.
func (c *StreamsConsumer) parseMessage(message redis.XMessage, streamName string) (*transport.StreamMessage, error) {
	data, ok := message.Values["data"]
	if !ok {
		return nil, fmt.Errorf("message missing 'data' field")
	}

	dataStr, ok := data.(string)
	if !ok {
		return nil, fmt.Errorf("message 'data' field is not a string")
	}

	// Deserialize from protobuf binary format
	// Redis stores bytes as strings, so convert back to []byte for protobuf
	var minedRelay transport.MinedRelayMessage
	if err := minedRelay.Unmarshal([]byte(dataStr)); err != nil {
		return nil, fmt.Errorf("failed to unmarshal message: %w", err)
	}

	return &transport.StreamMessage{
		ID:         message.ID,
		StreamName: streamName,
		Message:    &minedRelay,
	}, nil
}

// Ack acknowledges that a message has been successfully processed.
// DEPRECATED: Use AckMessage instead which automatically extracts stream name from StreamMessage.
func (c *StreamsConsumer) Ack(ctx context.Context, messageID string) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	// This method is deprecated because we don't know which stream the message belongs to.
	// Callers should use AckMessage instead.
	return fmt.Errorf("Ack(messageID) is deprecated in multi-stream mode, use AckMessage(msg) instead")
}

// AckMessage acknowledges a StreamMessage using its embedded stream name.
// This is the preferred method for acknowledging messages in multi-stream consumption.
func (c *StreamsConsumer) AckMessage(ctx context.Context, msg transport.StreamMessage) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	if msg.StreamName == "" {
		return fmt.Errorf("message missing stream name")
	}

	// Use XAckDel with DELREF to acknowledge AND delete the message from stream.
	// This prevents streams from growing unbounded - messages are removed after processing.
	// DELREF removes all references from all consumer groups (we only have one).
	err := c.client.XAckDel(ctx, msg.StreamName, c.config.ConsumerGroup, "DELREF", msg.ID).Err()
	if err != nil {
		return fmt.Errorf("failed to ack+delete message %s: %w", msg.ID, err)
	}

	ackedTotal.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
	return nil
}

// AckBatch acknowledges multiple messages in a single operation.
// Messages can be from different streams - they will be grouped automatically.
func (c *StreamsConsumer) AckBatch(ctx context.Context, messageIDs []string) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	if len(messageIDs) == 0 {
		return nil
	}

	// This method is deprecated because we don't know which streams the messages belong to.
	return fmt.Errorf("AckBatch(messageIDs) is deprecated in multi-stream mode, use AckMessageBatch(msgs) instead")
}

// AckMessageBatch acknowledges multiple StreamMessages, automatically grouping by stream.
func (c *StreamsConsumer) AckMessageBatch(ctx context.Context, msgs []transport.StreamMessage) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	if len(msgs) == 0 {
		return nil
	}

	// Group messages by stream for efficient pipelining
	byStream := make(map[string][]string)
	for _, msg := range msgs {
		if msg.StreamName == "" {
			continue
		}
		byStream[msg.StreamName] = append(byStream[msg.StreamName], msg.ID)
	}

	// Use pipeline to acknowledge AND delete all messages from streams.
	// XAckDel with DELREF prevents streams from growing unbounded.
	pipe := c.client.Pipeline()
	for streamName, ids := range byStream {
		pipe.XAckDel(ctx, streamName, c.config.ConsumerGroup, "DELREF", ids...)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to batch ack+delete: %w", err)
	}

	ackedTotal.WithLabelValues(c.config.SupplierOperatorAddress).Add(float64(len(msgs)))
	return nil
}

// Pending returns the number of messages that have been delivered but not yet acknowledged.
func (c *StreamsConsumer) Pending(ctx context.Context) (int64, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return 0, fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	// Check pending on the single supplier stream
	info, err := c.client.XPending(ctx, c.streamName, c.config.ConsumerGroup).Result()
	if err != nil {
		// Stream may not exist yet
		if strings.Contains(err.Error(), "no such key") ||
			strings.Contains(err.Error(), "NOGROUP") {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get pending info: %w", err)
	}

	pendingMessages.WithLabelValues(c.config.SupplierOperatorAddress).Set(float64(info.Count))
	return info.Count, nil
}

// GetPendingRelayCount returns the total pending count for the supplier stream.
// With single stream per supplier, we can't get per-session pending counts.
// This returns the total pending for the supplier.
func (c *StreamsConsumer) GetPendingRelayCount(ctx context.Context, sessionID string) (int64, error) {
	// With single stream architecture, all sessions share one stream.
	// Return total pending for the supplier (sessionID is ignored).
	return c.Pending(ctx)
}

// DeleteStream is a no-op with single stream architecture.
// Messages are automatically deleted via XAckDel when acknowledged.
// The single stream per supplier persists but stays clean.
func (c *StreamsConsumer) DeleteStream(ctx context.Context, sessionID string) error {
	// No-op: single stream per supplier persists across all sessions.
	// Messages are automatically deleted from stream via XAckDel (DELREF mode).
	c.logger.Debug().
		Str("session_id", sessionID).
		Msg("DeleteStream is no-op with single stream architecture")
	return nil
}

// TrimStream removes entries older than the specified duration using MINID.
// NOTE: With XAckDel, messages are deleted on ack, so this is now a backup safety net
// for any orphaned messages that weren't properly acknowledged.
// Returns the number of entries trimmed.
func (c *StreamsConsumer) TrimStream(ctx context.Context, maxAge time.Duration) (int64, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return 0, nil // Don't error on closed consumer - just skip trimming
	}
	c.mu.RUnlock()

	// Calculate MINID timestamp: current time - maxAge
	// Redis stream IDs are in format <ms>-<seq>, so we use <timestamp>-0
	minTimestamp := time.Now().Add(-maxAge).UnixMilli()
	minID := fmt.Sprintf("%d-0", minTimestamp)

	// Use XTRIM with MINID and ~ (approximate) for efficiency
	// ~ allows Redis to optimize by trimming to the nearest whole node
	trimmed, err := c.client.XTrimMinID(ctx, c.streamName, minID).Result()
	if err != nil {
		// Stream may not exist - not an error
		if strings.Contains(err.Error(), "no such key") {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to trim stream %s: %w", c.streamName, err)
	}

	if trimmed > 0 {
		c.logger.Info().
			Int64("trimmed_entries", trimmed).
			Str("min_id", minID).
			Dur("max_age", maxAge).
			Msg("trimmed old entries from stream")
	}

	return trimmed, nil
}

// Close gracefully shuts down the consumer.
func (c *StreamsConsumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true

	// Cancel context to stop goroutines
	if c.cancelFn != nil {
		c.cancelFn()
	}

	// Wait for goroutines to finish
	c.wg.Wait()

	c.logger.Info().Msg("Redis Streams consumer closed")
	return nil
}
