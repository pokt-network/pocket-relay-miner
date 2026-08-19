package transport

import (
	"context"
)

// MinedRelayPublisher publishes mined relays to the transport layer.
// The Relayer service uses this interface to send mined relays to the Miner service.
//
// Implementations must be safe for concurrent use by multiple goroutines.
type MinedRelayPublisher interface {
	// Publish sends a mined relay message to the transport layer.
	// The message is routed based on the SupplierOperatorAddress.
	//
	// This operation is fire-and-forget with acknowledgment:
	// - Returns nil on successful publish (message accepted by transport)
	// - Returns error on failure (network error, serialization error, etc.)
	//
	// The publisher should handle transient failures with internal retries.
	Publish(ctx context.Context, msg *MinedRelayMessage) error

	// PublishBatch sends multiple mined relay messages in a single operation.
	// More efficient than individual Publish calls for high throughput scenarios.
	//
	// All messages in the batch should be for the same supplier (same stream).
	// Returns error if any message fails to publish.
	PublishBatch(ctx context.Context, msgs []*MinedRelayMessage) error

	// Close gracefully shuts down the publisher, flushing any buffered messages.
	Close() error
}

// MinedRelayConsumer consumes mined relays from the transport layer.
// The Miner service uses this interface to receive mined relays from Relayer instances.
//
// Messages are auto-acknowledged via XACKDEL (Redis 8.2+) on successful consumption.
type MinedRelayConsumer interface {
	// Consume returns a channel that yields mined relay messages.
	// Messages are auto-acknowledged when received from the channel.
	//
	// The channel is closed when:
	// - The context is cancelled
	// - Close() is called
	// - An unrecoverable error occurs
	//
	// Callers should handle channel closure gracefully.
	Consume(ctx context.Context) <-chan StreamMessage

	// Pending returns the number of messages that have been delivered but not yet acknowledged.
	// Useful for monitoring consumer health and backpressure.
	Pending(ctx context.Context) (int64, error)

	// Close gracefully shuts down the consumer.
	// Any unacknowledged messages will be redelivered to other consumers in the group.
	Close() error

	// DeleteStream deletes a session stream after the session settles.
	// This stops the consumer from reading stale messages and frees Redis memory.
	// Safe to call even if the stream doesn't exist.
	DeleteStream(ctx context.Context, sessionID string) error
}

// ConsumerConfig contains configuration for a MinedRelayConsumer.
type ConsumerConfig struct {
	// StreamPrefix is the prefix for Redis stream names.
	// Full stream name: {StreamPrefix}:{SupplierOperatorAddress}
	StreamPrefix string

	// SupplierOperatorAddress is the supplier this consumer reads relays for.
	SupplierOperatorAddress string

	// ConsumerGroup is the Redis consumer group name.
	// All Miner instances for the same supplier should use the same group.
	ConsumerGroup string

	// ConsumerName is the unique name of this consumer within the group.
	// Typically includes hostname/pod name for identification.
	ConsumerName string

	// BatchSize is the maximum number of messages to fetch per read operation.
	BatchSize int64

	// Note: Stream consumption uses BLOCK 0 (TRUE PUSH) for live consumption.
	// This is hardcoded in the consumer and not configurable.

	// MaxRetries is the maximum number of times to retry a failed message.
	// After this, the message is moved to the dead letter queue.
	MaxRetries int64

	// ClaimIdleTimeout is how long a message can be pending before being claimed
	// by another consumer. This handles consumer crashes.
	ClaimIdleTimeout int64

	// ChannelBufferSize is the capacity of the delivery channel between the
	// consumer's read loop and the worker draining it. Defaults to 5000
	// (matching the default BatchSize) when zero or negative.
	ChannelBufferSize int64
}

// SupplierStreamName returns the Redis stream name for a supplier.
// Format: {prefix}:{supplierAddr}
// All relays for a supplier go to this single stream (simplified architecture).
// The sessionID is embedded in the message, not in the stream name.
func SupplierStreamName(prefix, supplierOperatorAddress string) string {
	return prefix + ":" + supplierOperatorAddress
}
