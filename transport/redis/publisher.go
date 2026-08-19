package redis

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

var _ transport.MinedRelayPublisher = (*StreamsPublisher)(nil)

// StreamsPublisher implements MinedRelayPublisher using Redis Streams.
// It publishes mined relays to a single supplier stream (simplified architecture).
// Each message contains the sessionID for routing by the consumer.
type StreamsPublisher struct {
	logger       logging.Logger
	client       redis.UniversalClient
	streamPrefix string

	// cacheTTL is the TTL for relay stream data (backup safety net)
	cacheTTL time.Duration

	// ttlSet tracks which stream keys already have TTL set.
	// Avoids calling EXPIRE on every publish (saves 1 Redis round-trip per relay).
	// Bounded by supplier count (stream names are ha:relays:{supplierAddr}).
	ttlSet *xsync.Map[string, struct{}]

	// mu protects closed state
	mu     sync.RWMutex
	closed bool
}

// NewStreamsPublisher creates a new Redis Streams publisher.
// cacheTTL is the TTL for relay stream data (default: 2h if not provided).
func NewStreamsPublisher(
	logger logging.Logger,
	client redis.UniversalClient,
	streamPrefix string,
	cacheTTL time.Duration,
) *StreamsPublisher {
	if cacheTTL <= 0 {
		cacheTTL = 2 * time.Hour // Default to 2h
	}

	return &StreamsPublisher{
		ttlSet:       xsync.NewMap[string, struct{}](),
		logger:       logging.ForComponent(logger, logging.ComponentRedisPublisher),
		client:       client,
		streamPrefix: streamPrefix,
		cacheTTL:     cacheTTL,
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

	// Use single stream per supplier (simplified architecture)
	streamName := transport.SupplierStreamName(p.streamPrefix, msg.SupplierOperatorAddress)

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

	// Log publish details for tracing (DEBUG level - per-relay)
	p.logger.Debug().
		Str("stream_name", streamName).
		Str("session_id", msg.SessionId).
		Str("supplier", msg.SupplierOperatorAddress).
		Str("service", msg.ServiceId).
		Str("message_id", messageID).
		Msg("relay published to stream")

	// Set stream TTL only once per stream (not on every publish).
	// Saves 1 Redis round-trip per relay. TTL is a backup safety net.
	if _, alreadySet := p.ttlSet.LoadOrStore(streamName, struct{}{}); !alreadySet {
		if ttlErr := p.client.Expire(ctx, streamName, p.cacheTTL).Err(); ttlErr != nil {
			p.ttlSet.Delete(streamName) // retry next publish
			p.logger.Debug().
				Err(ttlErr).
				Str(logging.FieldStreamID, streamName).
				Int64("ttl_seconds", int64(p.cacheTTL.Seconds())).
				Msg("failed to set stream TTL")
		}
	}

	// Update metrics
	publishedTotal.WithLabelValues(msg.SupplierOperatorAddress, msg.ServiceId).Inc()

	p.logger.Debug().
		Str(logging.FieldStreamID, streamName).
		Str(logging.FieldMessageID, messageID).
		Str(logging.FieldSessionID, msg.SessionId).
		Str(logging.FieldSupplier, msg.SupplierOperatorAddress).
		Int64("ttl_seconds", int64(p.cacheTTL.Seconds())).
		Msg("published mined relay to supplier stream")

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
