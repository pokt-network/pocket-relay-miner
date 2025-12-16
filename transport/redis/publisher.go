package redis

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

var _ transport.MinedRelayPublisher = (*StreamsPublisher)(nil)

// StreamsPublisher implements MinedRelayPublisher using Redis Streams.
// It publishes mined relays to session-specific streams with automatic TTL cleanup.
type StreamsPublisher struct {
	logger logging.Logger
	client redis.UniversalClient
	config transport.PublisherConfig

	// blockTimeSeconds is the expected block time for TTL calculation
	blockTimeSeconds int64

	// mu protects closed state
	mu     sync.RWMutex
	closed bool
}

// NewStreamsPublisher creates a new Redis Streams publisher.
// blockTimeSeconds is used to calculate stream TTL (default: 30s if not provided).
func NewStreamsPublisher(
	logger logging.Logger,
	client redis.UniversalClient,
	config transport.PublisherConfig,
	blockTimeSeconds int64,
) *StreamsPublisher {
	if blockTimeSeconds <= 0 {
		blockTimeSeconds = 30 // Default to 30s block time
	}

	return &StreamsPublisher{
		logger:           logging.ForComponent(logger, logging.ComponentRedisPublisher),
		client:           client,
		config:           config,
		blockTimeSeconds: blockTimeSeconds,
	}
}

// Publish sends a mined relay message to the Redis Stream for the session.
// The stream is automatically expired after the session's claim window closes.
func (p *StreamsPublisher) Publish(ctx context.Context, msg *transport.MinedRelayMessage) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return fmt.Errorf("publisher is closed")
	}
	p.mu.RUnlock()

	if msg == nil {
		return fmt.Errorf("message is nil")
	}

	// Validate required fields for TTL calculation
	if msg.SessionId == "" {
		return fmt.Errorf("session_id is required")
	}
	if msg.SessionEndHeight <= 0 {
		return fmt.Errorf("session_end_height is required")
	}

	// Set published timestamp if not already set
	if msg.PublishedAtUnixNano == 0 {
		msg.SetPublishedAt()
	}

	// Use per-session stream naming
	streamName := transport.StreamName(p.config.StreamPrefix, msg.SupplierOperatorAddress, msg.SessionId)

	// Serialize message to protobuf for Redis Stream
	// Protobuf binary format is 3-5× smaller than JSON and eliminates JSON decoder
	// memory overhead (literalStore accumulation with 1000 suppliers).
	// Performance: protobuf Marshal is ~2× faster than json.Marshal
	data, err := msg.Marshal()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	// Build XADD arguments (NO MaxLen - use TTL instead)
	args := &redis.XAddArgs{
		Stream: streamName,
		Values: map[string]interface{}{
			"data": data,
		},
	}

	// Publish to stream
	messageID, err := p.client.XAdd(ctx, args).Result()
	if err != nil {
		publishErrorsTotal.WithLabelValues(msg.SupplierOperatorAddress, msg.ServiceId).Inc()
		return fmt.Errorf("failed to publish to stream %s: %w", streamName, err)
	}

	// Set stream expiration (this is idempotent - safe to call multiple times)
	// TTL is calculated to expire shortly after the session is no longer useful
	// We don't have shared params here, so use a conservative estimate
	// Assume: claim window opens ~grace period after session end
	// Conservative: session_end + 10 blocks buffer + extra safety margin
	// This will be overridden by the consumer with accurate params
	ttl := p.calculateConservativeTTL(msg.SessionEndHeight)
	if ttlErr := p.client.Expire(ctx, streamName, ttl).Err(); ttlErr != nil {
		// Log but don't fail - stream will still work, just won't auto-expire
		p.logger.Warn().
			Err(ttlErr).
			Str(logging.FieldStreamID, streamName).
			Int64("ttl_seconds", int64(ttl.Seconds())).
			Msg("failed to set stream TTL")
	}

	// Update metrics
	publishedTotal.WithLabelValues(msg.SupplierOperatorAddress, msg.ServiceId).Inc()

	p.logger.Debug().
		Str(logging.FieldStreamID, streamName).
		Str(logging.FieldMessageID, messageID).
		Str(logging.FieldSessionID, msg.SessionId).
		Str(logging.FieldSupplier, msg.SupplierOperatorAddress).
		Int64("ttl_seconds", int64(ttl.Seconds())).
		Msg("published mined relay to session stream")

	return nil
}

// calculateConservativeTTL calculates a conservative TTL for the stream.
// This is a fallback when we don't have access to shared params.
// The consumer will set a more accurate TTL when it has access to params.
func (p *StreamsPublisher) calculateConservativeTTL(sessionEndHeight int64) time.Duration {
	// Conservative estimate: session might need to stay around until claim window closes + buffer
	// Assume: grace period ~4 blocks, claim window ~4 blocks, buffer ~10 blocks
	// Total: ~18 blocks after session end
	conservativeBlocks := int64(20)
	ttlSeconds := conservativeBlocks * p.blockTimeSeconds

	// Add 5 minute safety margin
	ttlSeconds += 300

	return time.Duration(ttlSeconds) * time.Second
}

// PublishBatch sends multiple mined relay messages in a single pipeline operation.
// Each message goes to its own session-specific stream with automatic TTL.
func (p *StreamsPublisher) PublishBatch(ctx context.Context, msgs []*transport.MinedRelayMessage) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return fmt.Errorf("publisher is closed")
	}
	p.mu.RUnlock()

	if len(msgs) == 0 {
		return nil
	}

	// Group messages by session for efficient pipelining
	bySession := make(map[string][]*transport.MinedRelayMessage)
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		if msg.SessionId == "" || msg.SessionEndHeight <= 0 {
			p.logger.Warn().
				Str("session_id", msg.SessionId).
				Int64("session_end_height", msg.SessionEndHeight).
				Msg("skipping message with missing session info")
			continue
		}
		sessionKey := msg.SupplierOperatorAddress + ":" + msg.SessionId
		bySession[sessionKey] = append(bySession[sessionKey], msg)
	}

	// Use pipeline for batch efficiency
	pipe := p.client.Pipeline()
	var cmds []*redis.StringCmd
	streamTTLs := make(map[string]time.Duration) // Track TTL per stream

	for _, sessionMsgs := range bySession {
		for _, msg := range sessionMsgs {
			// Set published timestamp
			if msg.PublishedAtUnixNano == 0 {
				msg.SetPublishedAt()
			}

			streamName := transport.StreamName(p.config.StreamPrefix, msg.SupplierOperatorAddress, msg.SessionId)

			// Use protobuf binary serialization (same as single publish)
			data, err := msg.Marshal()
			if err != nil {
				return fmt.Errorf("failed to serialize message: %w", err)
			}

			args := &redis.XAddArgs{
				Stream: streamName,
				Values: map[string]interface{}{
					"data": data,
				},
			}

			cmds = append(cmds, pipe.XAdd(ctx, args))

			// Calculate TTL for this stream (only once per stream)
			if _, exists := streamTTLs[streamName]; !exists {
				streamTTLs[streamName] = p.calculateConservativeTTL(msg.SessionEndHeight)
			}
		}
	}

	// Execute pipeline
	_, err := pipe.Exec(ctx)
	if err != nil {
		// Count errors per session
		for _, sessionMsgs := range bySession {
			for _, msg := range sessionMsgs {
				publishErrorsTotal.WithLabelValues(msg.SupplierOperatorAddress, msg.ServiceId).Inc()
			}
		}
		return fmt.Errorf("failed to execute batch publish: %w", err)
	}

	// Verify all commands succeeded
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			return fmt.Errorf("batch publish command %d failed: %w", i, cmd.Err())
		}
	}

	// Set TTLs for all streams (separate pipeline for efficiency)
	ttlPipe := p.client.Pipeline()
	for streamName, ttl := range streamTTLs {
		ttlPipe.Expire(ctx, streamName, ttl)
	}
	if _, ttlErr := ttlPipe.Exec(ctx); ttlErr != nil {
		p.logger.Warn().Err(ttlErr).Msg("failed to set TTLs for some streams")
	}

	// Update success metrics
	for _, sessionMsgs := range bySession {
		for _, msg := range sessionMsgs {
			publishedTotal.WithLabelValues(msg.SupplierOperatorAddress, msg.ServiceId).Inc()
		}
	}

	p.logger.Debug().
		Int("batch_size", len(msgs)).
		Int("sessions", len(bySession)).
		Msg("published batch of mined relays to session streams")

	return nil
}

// Close gracefully shuts down the publisher.
func (p *StreamsPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}

	p.closed = true
	p.logger.Info().Msg("Redis Streams publisher closed")
	return nil
}
