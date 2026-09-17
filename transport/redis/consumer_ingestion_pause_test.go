//go:build test

package redis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// heldIngestion is an IngestionPause a test opens and closes.
type heldIngestion struct {
	mu      sync.Mutex
	paused  bool
	changed chan struct{}
}

func newHeldIngestion() *heldIngestion {
	return &heldIngestion{paused: true, changed: make(chan struct{})}
}

func (h *heldIngestion) Paused() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paused
}

func (h *heldIngestion) PauseChanged() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *heldIngestion) resume() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paused = false
	close(h.changed)
	h.changed = make(chan struct{})
}

// heldConsumer is pausedConsumer with the store open again and ingestion held.
func heldConsumer(t *testing.T) (*StreamsConsumer, *StoreHealth, *heldIngestion, *commandCounter) {
	t.Helper()
	c, health, counter, _, _ := pausedConsumer(t)
	const maxmemory = 1024 * mib
	health.observe(maxmemory-512*mib, maxmemory)
	require.True(t, health.Operable(), "control: the store is open")
	pause := newHeldIngestion()
	c.SetIngestionPause(pause)
	counter.reads.Store(0)
	counter.reclaims.Store(0)
	return c, health, pause, counter
}

func TestStreamsConsumer_AHeldIngestionIssuesNoReadNoOwnPendingAndNoReclaimWithTheStoreOpen(t *testing.T) {
	c, health, _, counter := heldConsumer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, c.consumeMessagesUntilError(ctx), context.Canceled)
	require.Zero(t, counter.reads.Load(), "LINK hold-read: no XREADGROUP while ingestion is held")

	require.ErrorIs(t, c.deliverOwnPending(ctx), context.Canceled)
	require.Zero(t, counter.reads.Load(), "LINK hold-own: own pending is not delivered while ingestion is held")

	c.reclaimLoop(ctx)
	require.Zero(t, counter.reclaims.Load(), "LINK hold-reclaim: nothing is reclaimed while ingestion is held")
	require.True(t, health.Operable(), "holding ingestion does not close the store the tracker and publisher read")
}

func TestStreamsConsumer_ReadsOnceTheHeldIngestionIsResumed(t *testing.T) {
	c, _, pause, counter := heldConsumer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.consumeMessagesUntilError(ctx) }()

	pause.resume()
	select {
	case msg := <-c.msgCh:
		require.NotEmpty(t, msg.ID)
	case <-time.After(10 * time.Second):
		t.Fatal("resumed, the consumer must read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("consumer did not stop")
	}
	require.Positive(t, counter.reads.Load(), "control: resumed ingestion reads")
}
