package kv

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

// subscriberBuffer is how many messages a subscriber may fall behind before
// the bus drops for it. Redis disconnects a subscriber that falls too far
// behind; dropping and counting is the in-process equivalent, and every
// subscriber here (cache invalidation, block events) reconciles on its own
// schedule as well.
const subscriberBuffer = 1024

var busDropped = observability.SharedFactory.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "ha",
		Subsystem: "kv",
		Name:      "bus_dropped_total",
		Help:      "Messages the in-process bus dropped for a subscriber that was behind",
	},
	[]string{"channel"},
)

// Bus is pub/sub inside one process.
type Bus struct {
	logger logging.Logger
	mu     sync.RWMutex
	subs   map[string]map[*busSubscription]struct{}
}

// NewBus returns an empty bus.
func NewBus(logger logging.Logger) *Bus {
	return &Bus{logger: logger, subs: make(map[string]map[*busSubscription]struct{})}
}

// Publish delivers payload to every subscriber of channel, without waiting on
// any of them.
func (b *Bus) Publish(channel, payload string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs[channel] {
		select {
		case sub.out <- Message{Channel: channel, Payload: payload}:
		default:
			busDropped.WithLabelValues(channel).Inc()
		}
	}
}

// Subscribe opens a subscription; it is active when Subscribe returns.
func (b *Bus) Subscribe(channels ...string) Subscription {
	sub := &busSubscription{bus: b, channels: channels, out: make(chan Message, subscriberBuffer)}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range channels {
		if b.subs[ch] == nil {
			b.subs[ch] = make(map[*busSubscription]struct{})
		}
		b.subs[ch][sub] = struct{}{}
	}
	return sub
}

type busSubscription struct {
	bus       *Bus
	channels  []string
	out       chan Message
	closeOnce sync.Once
}

func (s *busSubscription) Messages() <-chan Message { return s.out }

func (s *busSubscription) Close() error {
	s.closeOnce.Do(func() {
		s.bus.mu.Lock()
		defer s.bus.mu.Unlock()
		for _, ch := range s.channels {
			delete(s.bus.subs[ch], s)
		}
		close(s.out)
	})
	return nil
}
