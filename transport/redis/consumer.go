package redis

import (
	"context"
	"encoding/json"
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
// Supports multi-stream consumption via periodic SCAN for session-based streams.
type StreamsConsumer struct {
	logger        logging.Logger
	client        redis.UniversalClient
	config        transport.ConsumerConfig
	streamPattern string // Pattern for discovering streams (e.g., "ha:relays:pokt1abc:*")

	// Active streams being consumed
	activeStreams   []string
	activeStreamsMu sync.RWMutex

	// Stream discovery interval
	discoveryInterval time.Duration

	// Message channel
	msgCh chan transport.StreamMessage

	// Lifecycle management
	mu       sync.RWMutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewStreamsConsumer creates a new Redis Streams consumer with multi-stream support.
// It discovers session streams via periodic SCAN and consumes from all active sessions.
func NewStreamsConsumer(
	logger logging.Logger,
	client redis.UniversalClient,
	config transport.ConsumerConfig,
	discoveryInterval time.Duration,
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

	// Set defaults
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	if config.BlockTimeout <= 0 {
		config.BlockTimeout = 5000 // 5 seconds
	}
	if config.ClaimIdleTimeout <= 0 {
		config.ClaimIdleTimeout = 30000 // 30 seconds
	}
	if config.MaxRetries <= 0 {
		config.MaxRetries = 3
	}
	if discoveryInterval <= 0 {
		discoveryInterval = 10 * time.Second // Default: 10 seconds
	}

	return &StreamsConsumer{
		logger:            logging.ForSupplierComponent(logger, logging.ComponentRedisConsumer, config.SupplierOperatorAddress),
		client:            client,
		config:            config,
		streamPattern:     transport.StreamPattern(config.StreamPrefix, config.SupplierOperatorAddress),
		discoveryInterval: discoveryInterval,
		activeStreams:     []string{},
		msgCh:             make(chan transport.StreamMessage, config.BatchSize*2),
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

	// Start stream discovery loop (this will also ensure consumer groups exist)
	c.wg.Add(1)
	go c.streamDiscoveryLoop(ctx)

	// Start consumer goroutine
	c.wg.Add(1)
	go c.consumeLoop(ctx)

	// Start pending message claimer (handles crashed consumers)
	c.wg.Add(1)
	go c.claimIdleMessages(ctx)

	return c.msgCh
}

// streamDiscoveryLoop periodically scans for new session streams and updates the active streams list.
func (c *StreamsConsumer) streamDiscoveryLoop(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(c.discoveryInterval)
	defer ticker.Stop()

	// Do an initial discovery immediately
	c.discoverStreams(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.discoverStreams(ctx)
		}
	}
}

// discoverStreams scans Redis for session streams matching the pattern and updates the active streams list.
func (c *StreamsConsumer) discoverStreams(ctx context.Context) {
	start := time.Now()

	// Use SCAN to find all matching stream keys
	var cursor uint64
	var discoveredStreams []string

	for {
		keys, nextCursor, err := c.client.Scan(ctx, cursor, c.streamPattern, 100).Result()
		if err != nil {
			streamDiscoveryErrors.WithLabelValues(c.config.SupplierOperatorAddress, "scan_failed").Inc()
			c.logger.Warn().Err(err).Msg("failed to scan for session streams")
			return
		}

		// Filter out non-stream keys and ensure consumer group exists
		for _, key := range keys {
			// Verify it's actually a stream by checking its type
			keyType, typeErr := c.client.Type(ctx, key).Result()
			if typeErr != nil || keyType != "stream" {
				continue
			}

			// Ensure consumer group exists for this stream
			if groupErr := c.ensureConsumerGroupForStream(ctx, key); groupErr != nil {
				c.logger.Debug().
					Err(groupErr).
					Str("stream", key).
					Msg("failed to ensure consumer group for stream")
				continue
			}

			discoveredStreams = append(discoveredStreams, key)
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	// Update active streams list
	c.activeStreamsMu.Lock()
	oldCount := len(c.activeStreams)
	c.activeStreams = discoveredStreams
	newCount := len(c.activeStreams)
	c.activeStreamsMu.Unlock()

	// Update metrics
	activeSessionStreams.WithLabelValues(c.config.SupplierOperatorAddress).Set(float64(newCount))
	streamDiscoveryScanDuration.WithLabelValues(c.config.SupplierOperatorAddress).Observe(time.Since(start).Seconds())

	if newCount != oldCount {
		c.logger.Debug().
			Int("old_count", oldCount).
			Int("new_count", newCount).
			Msg("stream discovery updated active streams")
	}
}

// ensureConsumerGroupForStream creates the consumer group for a specific stream if it doesn't exist.
func (c *StreamsConsumer) ensureConsumerGroupForStream(ctx context.Context, streamName string) error {
	// Try to create the consumer group
	err := c.client.XGroupCreateMkStream(ctx, streamName, c.config.ConsumerGroup, "0").Err()
	if err != nil {
		// Ignore "BUSYGROUP" error - group already exists
		if !strings.Contains(err.Error(), "BUSYGROUP") {
			return fmt.Errorf("failed to create consumer group for %s: %w", streamName, err)
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
		// connectFn: No-op since stream discovery handles consumer group creation
		func(ctx context.Context) error {
			return nil
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
// Consumes from multiple session streams simultaneously.
func (c *StreamsConsumer) consumeMessagesUntilError(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Get current active streams list
		c.activeStreamsMu.RLock()
		streamsCopy := make([]string, len(c.activeStreams))
		copy(streamsCopy, c.activeStreams)
		c.activeStreamsMu.RUnlock()

		// Skip if no streams discovered yet
		if len(streamsCopy) == 0 {
			time.Sleep(100 * time.Millisecond) // Small delay before retry
			continue
		}

		// Build XREADGROUP arguments with all active streams
		// XREADGROUP requires stream names followed by IDs in alternating order
		streamArgs := make([]string, 0, len(streamsCopy)*2)
		streamArgs = append(streamArgs, streamsCopy...)
		// Append ">" for each stream (read only new messages)
		for range streamsCopy {
			streamArgs = append(streamArgs, ">")
		}

		// Read new messages from all streams
		streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.config.ConsumerGroup,
			Consumer: c.config.ConsumerName,
			Streams:  streamArgs,
			Count:    c.config.BatchSize,
			Block:    time.Duration(c.config.BlockTimeout) * time.Millisecond,
		}).Result()

		if err != nil {
			if err == redis.Nil {
				// No new messages, continue
				continue
			}
			if ctx.Err() != nil {
				// Context cancelled - return to stop gracefully
				return ctx.Err()
			}

			// Handle race conditions with stream expiration
			// NOGROUP or stream not found errors are expected when streams expire
			if strings.Contains(err.Error(), "NOGROUP") ||
				strings.Contains(err.Error(), "no such key") {
				streamDiscoveryErrors.WithLabelValues(c.config.SupplierOperatorAddress, "stream_expired").Inc()
				c.logger.Debug().Err(err).Msg("stream expired or consumer group missing, will rediscover")
				// Trigger immediate rediscovery
				go c.discoverStreams(ctx)
				continue
			}

			consumeErrorsTotal.WithLabelValues(c.config.SupplierOperatorAddress, "read_error").Inc()
			c.logger.Error().Err(err).Msg("error reading from streams")
			// Return error to trigger reconnection (replaces time.Sleep backoff)
			return err
		}

		// Process messages from all streams
		for _, stream := range streams {
			for _, message := range stream.Messages {
				msg, parseErr := c.parseMessage(message, stream.Stream)
				if parseErr != nil {
					deserializationErrors.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
					c.logger.Error().
						Err(parseErr).
						Str(logging.FieldMessageID, message.ID).
						Str("stream", stream.Stream).
						Msg("failed to parse message")
					// Acknowledge bad message to avoid redelivery
					_ = c.client.XAck(ctx, stream.Stream, c.config.ConsumerGroup, message.ID)
					continue
				}

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
}

// claimIdleMessages periodically claims messages from idle consumers.
// This handles the case where a consumer crashes without acknowledging messages.
func (c *StreamsConsumer) claimIdleMessages(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(time.Duration(c.config.ClaimIdleTimeout/2) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.claimPendingMessages(ctx)
		}
	}
}

// claimPendingMessages claims messages that have been pending too long across all active streams.
func (c *StreamsConsumer) claimPendingMessages(ctx context.Context) {
	// Get all active streams
	c.activeStreamsMu.RLock()
	streamsCopy := make([]string, len(c.activeStreams))
	copy(streamsCopy, c.activeStreams)
	c.activeStreamsMu.RUnlock()

	// Claim idle messages from each stream
	for _, streamName := range streamsCopy {
		messages, _, err := c.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   streamName,
			Group:    c.config.ConsumerGroup,
			Consumer: c.config.ConsumerName,
			MinIdle:  time.Duration(c.config.ClaimIdleTimeout) * time.Millisecond,
			Start:    "0-0",
			Count:    c.config.BatchSize,
		}).Result()

		if err != nil {
			// Stream may have expired - skip
			if strings.Contains(err.Error(), "no such key") ||
				strings.Contains(err.Error(), "NOGROUP") {
				continue
			}
			if ctx.Err() == nil {
				c.logger.Debug().
					Err(err).
					Str("stream", streamName).
					Msg("error claiming idle messages")
			}
			continue
		}

		if len(messages) == 0 {
			continue
		}

		claimedMessages.WithLabelValues(c.config.SupplierOperatorAddress).Add(float64(len(messages)))

		c.logger.Debug().
			Int("count", len(messages)).
			Str("stream", streamName).
			Msg("claimed idle messages")

		// Process claimed messages
		for _, message := range messages {
			msg, parseErr := c.parseMessage(message, streamName)
			if parseErr != nil {
				deserializationErrors.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
				// Acknowledge bad message
				_ = c.client.XAck(ctx, streamName, c.config.ConsumerGroup, message.ID)
				continue
			}

			select {
			case c.msgCh <- *msg:
			case <-ctx.Done():
				return
			}
		}
	}
}

// parseMessage deserializes a Redis Stream message into a StreamMessage.
// The streamName parameter is required for acknowledgment in multi-stream consumption.
func (c *StreamsConsumer) parseMessage(message redis.XMessage, streamName string) (*transport.StreamMessage, error) {
	data, ok := message.Values["data"]
	if !ok {
		return nil, fmt.Errorf("message missing 'data' field")
	}

	dataStr, ok := data.(string)
	if !ok {
		return nil, fmt.Errorf("message 'data' field is not a string")
	}

	var minedRelay transport.MinedRelayMessage
	if err := json.Unmarshal([]byte(dataStr), &minedRelay); err != nil {
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

	err := c.client.XAck(ctx, msg.StreamName, c.config.ConsumerGroup, msg.ID).Err()
	if err != nil {
		return fmt.Errorf("failed to ack message %s: %w", msg.ID, err)
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

	// Use pipeline to acknowledge all messages
	pipe := c.client.Pipeline()
	for streamName, ids := range byStream {
		pipe.XAck(ctx, streamName, c.config.ConsumerGroup, ids...)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to batch ack: %w", err)
	}

	ackedTotal.WithLabelValues(c.config.SupplierOperatorAddress).Add(float64(len(msgs)))
	return nil
}

// Pending returns the number of messages that have been delivered but not yet acknowledged across all streams.
func (c *StreamsConsumer) Pending(ctx context.Context) (int64, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return 0, fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	// Get all active streams
	c.activeStreamsMu.RLock()
	streamsCopy := make([]string, len(c.activeStreams))
	copy(streamsCopy, c.activeStreams)
	c.activeStreamsMu.RUnlock()

	// Sum pending across all streams
	var totalPending int64
	for _, streamName := range streamsCopy {
		info, err := c.client.XPending(ctx, streamName, c.config.ConsumerGroup).Result()
		if err != nil {
			// Stream may have expired - skip
			continue
		}
		totalPending += info.Count
	}

	pendingMessages.WithLabelValues(c.config.SupplierOperatorAddress).Set(float64(totalPending))
	return totalPending, nil
}

// GetPendingRelayCount returns the number of pending relays for a specific session stream.
// This implements the PendingRelayChecker interface for late relay detection.
func (c *StreamsConsumer) GetPendingRelayCount(ctx context.Context, sessionID string) (int64, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return 0, fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	// Build stream name for this session
	streamName := transport.StreamName(c.config.StreamPrefix, c.config.SupplierOperatorAddress, sessionID)

	// Check pending messages for this specific stream
	info, err := c.client.XPending(ctx, streamName, c.config.ConsumerGroup).Result()
	if err != nil {
		// Stream may not exist or may have expired
		if strings.Contains(err.Error(), "no such key") ||
			strings.Contains(err.Error(), "NOGROUP") {
			return 0, nil // Stream doesn't exist - no pending messages
		}
		return 0, fmt.Errorf("failed to get pending info for session %s: %w", sessionID, err)
	}

	return info.Count, nil
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
