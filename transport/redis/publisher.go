package redis

import (
	"context"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"

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

	// mu protects closed state
	mu     sync.RWMutex
	closed bool
}

// NewStreamsPublisher creates a new Redis Streams publisher.
//
// It sets no expiry on the streams it writes. A supplier's stream is a permanent
// lane that spans every session that supplier ever serves, so a clock is the wrong
// instrument for ending its life: the lane should live as long as the supplier does.
// See Publish for what bounds the stream's SIZE instead.
func NewStreamsPublisher(
	logger logging.Logger,
	client redis.UniversalClient,
	streamPrefix string,
) *StreamsPublisher {
	return &StreamsPublisher{
		logger:       logging.ForComponent(logger, logging.ComponentRedisPublisher),
		client:       client,
		streamPrefix: streamPrefix,
	}
}

// Publish sends a mined relay message to the Redis Stream for the session.
//
// The stream key is NOT given an expiry, and this is load-bearing rather than an
// omission. Until 2026-08-20 the publisher issued EXPIRE once per (process, stream)
// and memoised the fact, which produced two defects measured against a live Redis:
//
//   - an absolute deadline anchored to the first publish of that process, unrelated
//     to any session boundary, that deleted the whole key mid-session -- taking
//     un-consumed entries and the pending-entries list with it, silently, because
//     Redis key expiry emits no log and no metric;
//   - a key that, once expired and recreated by XADD, came back with no TTL at all
//     while the memo still said one had been set, so it was never re-armed.
//
// What bounds the stream's size is delivery, not time: the miner deletes each entry
// as it acknowledges it (XACKDEL/DELREF), and a periodic XTRIM MINID sweeps whatever
// slipped past. What ends the stream's life is the supplier's own lifecycle, not a
// timer.
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

	// Per-relay: Debug only, and the alertable signal is publishedTotal below.
	p.logger.Debug().
		Str(logging.FieldStreamID, streamName).
		Str(logging.FieldMessageID, messageID).
		Str(logging.FieldSessionID, msg.SessionId).
		Str(logging.FieldSupplier, msg.SupplierOperatorAddress).
		Str("service", msg.ServiceId).
		Msg("relay published to stream")

	// Update metrics
	publishedTotal.WithLabelValues(msg.SupplierOperatorAddress, msg.ServiceId).Inc()

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
