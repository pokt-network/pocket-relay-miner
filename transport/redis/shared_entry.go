package redis

import (
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// The functions below are what another queue implementation shares with this
// one, so a relay is encoded, decoded and counted the same way whichever store
// carries it: the metric series are the ha_transport_* ones the dashboards read.

// DecodeEntry decodes the bytes PrepareEntry encoded into a pooled message; the
// caller releases it (transport.ReleaseMinedRelayMessage).
func DecodeEntry(data []byte) (*transport.MinedRelayMessage, error) {
	msg := transport.AcquireMinedRelayMessage()
	if err := msg.Unmarshal(data); err != nil {
		transport.ReleaseMinedRelayMessage(msg)
		return nil, err
	}
	return msg, nil
}

// RecordPublishReject counts and logs a relay Publish refused.
func RecordPublishReject(logger logging.Logger, reason string, msg *transport.MinedRelayMessage, detail string) {
	recordPublishReject(logger, reason, serviceOf(msg), detail)
}

// RecordPublished counts one relay written to its supplier's queue.
func RecordPublished(supplier, service string) {
	publishedTotal.WithLabelValues(supplier, service).Inc()
}

// RecordConsumed counts one relay delivered to the miner, and its latency from
// the publish.
func RecordConsumed(supplier string, msg *transport.MinedRelayMessage, bytes int) {
	consumerReadBytesTotal.WithLabelValues(supplier).Add(float64(bytes))
	if msg.PublishedAtUnixNano > 0 {
		endToEndLatency.WithLabelValues(supplier, msg.ServiceId).Observe(time.Since(msg.PublishedAt()).Seconds())
	}
	consumedTotal.WithLabelValues(supplier, msg.ServiceId).Inc()
}

// RecordAcked counts n entries acknowledged for the supplier.
func RecordAcked(supplier string, n int) {
	ackedTotal.WithLabelValues(supplier).Add(float64(n))
}
