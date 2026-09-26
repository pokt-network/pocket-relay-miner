package redis

import (
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// prepareXAdd validates a mined relay and turns it into the XADD that carries
// it, WITHOUT issuing anything.
//
// It lives apart from the publisher because it predates it: it was shared with
// the one-relay-per-round-trip publisher that the always-on batch replaced, and
// these checks are what rejected 1412 served relays in the 2026-09-11 load.
//
// The XADD it builds carries NO expiry, and that is load-bearing. Until
// 2026-08-20 the publisher armed EXPIRE once per (process, stream), which deleted
// a supplier's whole stream mid-session -- un-consumed entries and the pending
// entries list with it, silently -- and re-created it with no TTL at all. A
// supplier's stream spans every session it serves; what bounds its size is
// delivery (the miner deletes each entry as it acknowledges it, plus a periodic
// XTRIM MINID), not a clock. TestBatchingPublisherSetsNoStreamTTL pins it.
//
// It is also WHERE the validation happens that decides how big the poison-message
// problem is. Running it at ENQUEUE means an invalid message never reaches a
// batch; running it at dispatch would let one into a chunk, where the EXEC
// rejects it and the chunk becomes permanently undispatchable -- head-of-line
// blocking invented for a message we already knew how to reject.
func prepareXAdd(streamPrefix string, msg *transport.MinedRelayMessage) (string, *redis.XAddArgs, string, error) {
	if msg == nil {
		return "", nil, rejectReasonNilMessage, fmt.Errorf("message is nil")
	}

	// Validate required fields for TTL calculation
	if msg.SessionId == "" {
		return "", nil, rejectReasonNoSessionID, fmt.Errorf("session_id is required")
	}
	if msg.SessionEndHeight <= 0 {
		return "", nil, rejectReasonBadEndHeight, fmt.Errorf("session_end_height is required")
	}

	// Set published timestamp if not already set. At ENQUEUE for the batching
	// publisher, which is the honest reading: it is when the relayer handed the
	// relay over, and the gap to the dispatch is the queue's own latency.
	if msg.PublishedAtUnixNano == 0 {
		msg.SetPublishedAt()
	}

	// Use single stream per supplier (simplified architecture)
	streamName := transport.SupplierStreamName(streamPrefix, msg.SupplierOperatorAddress)

	// Serialize message to protobuf for Redis Stream
	// Protobuf binary format is 3-5× smaller than JSON and eliminates JSON decoder
	// memory overhead (literalStore accumulation with 1000 suppliers).
	// Performance: protobuf Marshal is ~2× faster than json.Marshal
	// The relay bytes are compressed HERE, on the one path every publisher shares,
	// and on a COPY of the message: the caller's RelayBytes stay the original, and
	// the entry the queue counts and Redis stores carries the compressed form.
	wire := msg
	if len(msg.RelayBytesS2) == 0 {
		compressed, outcome := transport.CompressRelayBytes(msg.RelayBytes)
		relayCompressionTotal.WithLabelValues(msg.ServiceId, outcome).Inc()
		if compressed != nil {
			relayCompressionBytes.WithLabelValues(msg.ServiceId, "in").Add(float64(len(msg.RelayBytes)))
			relayCompressionBytes.WithLabelValues(msg.ServiceId, "out").Add(float64(len(compressed)))
			c := *msg
			c.RelayBytes = nil
			c.RelayBytesS2 = compressed
			wire = &c
		}
	}
	data, err := wire.Marshal()
	if err != nil {
		return "", nil, "serialize_failed", fmt.Errorf("failed to serialize message: %w", err)
	}

	// Build XADD arguments (NO MaxLen - use TTL instead)
	return streamName, &redis.XAddArgs{
		Stream: streamName,
		Values: map[string]interface{}{
			"data": data,
		},
	}, "", nil
}

// serviceOf is the service label for a message that may be nil.
func serviceOf(msg *transport.MinedRelayMessage) string {
	if msg == nil {
		return ""
	}
	return msg.ServiceId
}
