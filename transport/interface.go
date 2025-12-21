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
// Implementations must provide exactly-once delivery semantics within the consumer group.
type MinedRelayConsumer interface {
	// Consume returns a channel that yields mined relay messages.
	// Messages are not acknowledged until Ack is called.
	//
	// The channel is closed when:
	// - The context is cancelled
	// - Close() is called
	// - An unrecoverable error occurs
	//
	// Callers should handle channel closure gracefully.
	Consume(ctx context.Context) <-chan StreamMessage

	// Ack acknowledges that a message has been successfully processed.
	// The message will not be redelivered to any consumer in the group.
	//
	// Call this AFTER the relay has been:
	// 1. Deduplicated
	// 2. Added to the session tree
	// 3. Persisted to WAL (if applicable)
	Ack(ctx context.Context, messageID string) error

	// AckBatch acknowledges multiple messages in a single operation.
	// More efficient than individual Ack calls.
	AckBatch(ctx context.Context, messageIDs []string) error

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
}

// SupplierStreamName returns the Redis stream name for a supplier.
// Format: {prefix}:{supplierAddr}
// All relays for a supplier go to this single stream (simplified architecture).
// The sessionID is embedded in the message, not in the stream name.
func SupplierStreamName(prefix, supplierOperatorAddress string) string {
	return prefix + ":" + supplierOperatorAddress
}

// StreamName returns the full Redis stream name for a session.
// Format: {prefix}:{supplierAddr}:{sessionID}
// DEPRECATED: Use SupplierStreamName instead. Kept for backwards compatibility.
func StreamName(prefix, supplierOperatorAddress, sessionID string) string {
	return prefix + ":" + supplierOperatorAddress + ":" + sessionID
}

// StreamPattern returns a Redis key pattern for discovering all streams for a supplier.
// Use with SCAN to find all session streams for a supplier.
// DEPRECATED: No longer needed with single stream per supplier architecture.
func StreamPattern(prefix, supplierOperatorAddress string) string {
	return prefix + ":" + supplierOperatorAddress + ":*"
}
