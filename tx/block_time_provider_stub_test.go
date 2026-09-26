package tx

import (
	"sync"
	"time"
)

// stubBlockTimeProvider is a test double for BlockTimeProvider. The
// stored time is returned by every LatestBlockTime call; set it to the
// zero time.Time to simulate "no block event yet".
// testBlockTime is the anchor every test client is built with: the production
// one is seeded from the chain at startup, and a client without it is refused.
func testBlockTime() *stubBlockTimeProvider {
	return &stubBlockTimeProvider{t: time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)}
}

type stubBlockTimeProvider struct {
	mu sync.RWMutex
	t  time.Time
}

func (s *stubBlockTimeProvider) LatestBlockTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.t
}
