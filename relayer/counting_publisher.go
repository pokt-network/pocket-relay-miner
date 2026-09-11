package relayer

import (
	"context"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// countingPublisher counts in relays_published_total every mined relay its
// publisher accepts, whatever transport published it. The count used to sit
// after the HTTP path's own Publish calls only, so a relay published over
// WebSocket or gRPC reached the store uncounted there -- and scripts/gates/
// live.sh reads that counter as proof that a load ran at all.
type countingPublisher struct {
	transport.MinedRelayPublisher
}

// countPublished wraps p so that what it publishes is counted. It returns nil
// for nil, because every transport reads a nil publisher as "publish nothing"
// -- a simulated WebSocket relay relies on it -- and p itself when p is
// already wrapped: the proxy hands its publisher to the WebSocket bridge and to
// the gRPC service, whose constructors wrap it again, and a second wrapper
// would count each relay twice.
func countPublished(p transport.MinedRelayPublisher) transport.MinedRelayPublisher {
	if p == nil {
		return nil
	}
	if _, ok := p.(*countingPublisher); ok {
		return p
	}
	return &countingPublisher{MinedRelayPublisher: p}
}

// Publish publishes msg and, once the store has accepted it, counts it.
func (c *countingPublisher) Publish(ctx context.Context, msg *transport.MinedRelayMessage) error {
	if err := c.MinedRelayPublisher.Publish(ctx, msg); err != nil {
		return err
	}
	relaysPublished.WithLabelValues(msg.ServiceId, msg.SupplierOperatorAddress).Inc()
	return nil
}
