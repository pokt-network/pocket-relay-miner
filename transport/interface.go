package transport

import (
	"context"
	"time"
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

	// Close gracefully shuts down the publisher, flushing any buffered messages.
	Close() error
}

// MinedRelayConsumer is the queue the Miner reads one supplier's mined relays
// from. Delivery is at-least-once within TrimStream's maxAge: a delivered entry
// stays pending until it is acknowledged (AckMessage, or a committer that
// acknowledges it together with other writes), handed back (ReleaseMessage), or
// trimmed. A redelivery of an entry that was already delivered once carries
// IsReclaim=true.
//
// Entry IDs are "<ms>-<seq>", strictly increasing in append order (the Miner
// compares them to gate a claim on everything appended so far).
//
// Implementations must be safe for concurrent use by multiple goroutines.
type MinedRelayConsumer interface {
	// Consume starts delivery and returns the channel it delivers on. It is
	// called at most once per consumer. The channel is closed once delivery
	// has stopped: the context is cancelled, or Stop or Close is called.
	Consume(ctx context.Context) <-chan StreamMessage

	// MarkDelivered is called by whoever takes a message from the channel, as
	// soon as it takes it, so the channel's byte budget stops counting it.
	MarkDelivered(msg StreamMessage)

	// AckMessage acknowledges and removes one entry: it is never delivered again.
	AckMessage(ctx context.Context, msg StreamMessage) error

	// ReleaseMessage hands one pending entry back unacknowledged, so the next
	// redelivery pass can deliver it again without waiting for it to go idle.
	ReleaseMessage(ctx context.Context, msg StreamMessage) error

	// EachOwnPending calls fn with every entry pending under this consumer,
	// oldest first, marked a reclaim. Meant for a consumer already stopped. An
	// entry no longer in the queue, or one that does not parse, is
	// acknowledged instead of passed to fn.
	EachOwnPending(ctx context.Context, fn func(StreamMessage)) error

	// LastGeneratedID is the highest ID appended, delivered or not; "" or
	// "0-0" means nothing has been appended.
	LastGeneratedID(ctx context.Context) (string, error)

	// RecordAcked counts n entries acknowledged on this consumer's behalf
	// outside AckMessage, on the series AckMessage counts on.
	RecordAcked(n int)

	// TrimStream removes entries older than maxAge, a safety net for entries
	// never acknowledged. Returns how many it removed.
	TrimStream(ctx context.Context, maxAge time.Duration) (int64, error)

	// StreamName is the name every message this consumer delivers carries in
	// StreamMessage.StreamName, and the one AckMessage and ReleaseMessage need.
	StreamName() string

	// Stop ends delivery and waits for it, without closing: AckMessage and
	// ReleaseMessage keep working. Idempotent.
	Stop()

	// Close stops the consumer and releases its resources. Entries still
	// pending stay pending. Idempotent.
	Close() error
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

	// Note: stream consumption blocks on XREADGROUP for a fixed interval
	// (transport/redis.blockInterval), not configurable here. Delivery is still
	// push -- the read returns the instant data arrives; the interval only
	// bounds an idle wait so shutdown does not hang on it.

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
