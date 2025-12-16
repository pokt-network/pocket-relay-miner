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

	// cacheTTL is the TTL for relay stream data (backup safety net)
	cacheTTL time.Duration

	// mu protects closed state
	mu     sync.RWMutex
	closed bool
}

// NewStreamsPublisher creates a new Redis Streams publisher.
// cacheTTL is the TTL for relay stream data (default: 2h if not provided).
func NewStreamsPublisher(
	logger logging.Logger,
	client redis.UniversalClient,
	config transport.PublisherConfig,
	cacheTTL time.Duration,
) *StreamsPublisher {
	if cacheTTL <= 0 {
		cacheTTL = 2 * time.Hour // Default to 2h
	}

	return &StreamsPublisher{
		logger:   logging.ForComponent(logger, logging.ComponentRedisPublisher),
		client:   client,
		config:   config,
		cacheTTL: cacheTTL,
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
	// TTL is a backup safety net - manual cleanup is primary
	if ttlErr := p.client.Expire(ctx, streamName, p.cacheTTL).Err(); ttlErr != nil {
		// Log but don't fail - stream will still work, just won't auto-expire
		p.logger.Warn().
			Err(ttlErr).
			Str(logging.FieldStreamID, streamName).
			Int64("ttl_seconds", int64(p.cacheTTL.Seconds())).
			Msg("failed to set stream TTL")
	}

	// Update metrics
	publishedTotal.WithLabelValues(msg.SupplierOperatorAddress, msg.ServiceId).Inc()

	p.logger.Debug().
		Str(logging.FieldStreamID, streamName).
		Str(logging.FieldMessageID, messageID).
		Str(logging.FieldSessionID, msg.SessionId).
		Str(logging.FieldSupplier, msg.SupplierOperatorAddress).
		Int64("ttl_seconds", int64(p.cacheTTL.Seconds())).
		Msg("published mined relay to session stream")

	return nil
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

			// Set TTL for this stream (only once per stream)
			// TTL is a backup safety net - manual cleanup is primary
			if _, exists := streamTTLs[streamName]; !exists {
				streamTTLs[streamName] = p.cacheTTL
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
