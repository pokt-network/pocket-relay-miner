package relay

import (
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// loadBackoffMin and loadBackoffMax bound the wait after a refusal. Without
	// any wait, a relayer refusing every connection (close 1013, HTTP 429) was
	// redialed 104,955 times in about three minutes, the client ran out of
	// ephemeral ports ("connect: cannot assign requested address") and the load
	// Job died.
	loadBackoffMin = 50 * time.Millisecond
	loadBackoffMax = 5 * time.Second
)

// loadBackoff spaces a load test's retries out while the relayer refuses work or
// a dial fails: each refusal doubles the wait, from loadBackoffMin to
// loadBackoffMax, with jitter so the workers do not come back together; any
// success resets it. It is shared by every worker of a run.
type loadBackoff struct {
	mu    sync.Mutex
	delay time.Duration
	waits atomic.Int64
	sleep func(time.Duration)
}

func newLoadBackoff() *loadBackoff {
	return &loadBackoff{sleep: time.Sleep}
}

// refused waits before the caller tries again.
func (b *loadBackoff) refused() {
	b.mu.Lock()
	if b.delay == 0 {
		b.delay = loadBackoffMin
	} else {
		b.delay = min(2*b.delay, loadBackoffMax)
	}
	wait := b.delay/2 + rand.N(b.delay/2+1)
	b.mu.Unlock()
	b.waits.Add(1)
	b.sleep(wait)
}

// succeeded resets the wait.
func (b *loadBackoff) succeeded() {
	b.mu.Lock()
	b.delay = 0
	b.mu.Unlock()
}

// Waits is how many times a worker waited.
func (b *loadBackoff) Waits() int64 { return b.waits.Load() }

// isTryAgainLater reports a WebSocket close 1013 (try again later).
func isTryAgainLater(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == websocket.CloseTryAgainLater
}

// isGRPCRefusal reports a gRPC relay refused for capacity.
func isGRPCRefusal(err error) bool {
	code := status.Code(err)
	return code == codes.ResourceExhausted || code == codes.Unavailable
}
