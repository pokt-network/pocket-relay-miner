package transport

import (
	"time"
)

// SetPublishedAt sets the PublishedAtUnixNano field to the current time.
func (m *MinedRelayMessage) SetPublishedAt() {
	m.PublishedAtUnixNano = time.Now().UnixNano()
}

// PublishedAt returns the PublishedAtUnixNano as a time.Time.
func (m *MinedRelayMessage) PublishedAt() time.Time {
	return time.Unix(0, m.PublishedAtUnixNano)
}

// StreamMessage wraps a MinedRelayMessage with its Redis Stream message ID and stream name.
// Used by the consumer to track which messages have been processed.
type StreamMessage struct {
	// ID is the Redis Stream message ID (e.g., "1234567890123-0").
	ID string

	// StreamName is the Redis Stream key this message came from.
	// This is needed for acknowledgment in multi-stream consumption.
	StreamName string

	// Message is the deserialized MinedRelayMessage.
	Message *MinedRelayMessage

	// IsReclaim is true when this message was recovered from another consumer's
	// pending entries list (i.e., a previous consumer crashed without acking).
	// The recovery is an XPENDING scan followed by XCLAIM, not XAUTOCLAIM: only
	// XPENDING reveals which consumer owns an entry, and claiming back our own
	// in-flight deliveries would produce a duplicate storm under a backlog.
	// Consumers downstream use this flag to decide whether duplicate-detection
	// is required — the normal XREADGROUP `>` delivery path never redelivers,
	// so dedup is only necessary when IsReclaim is true.
	IsReclaim bool
}
