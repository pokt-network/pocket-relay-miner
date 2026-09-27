//go:build test

package observability

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Stop() guards on `running`, and the ctx.Done goroutines delegate to Stop(), so
// `running == false` has to MEAN that nothing is listening. That invariant holds
// today for a reason nothing states: startMetricsServer binds its listener
// BEFORE the Registry check, and closes it in a defer when it never hands it to
// an http.Server. Delete that defer and the guard starts lying.
//
// A nil Registry is the reachable way to exercise it: net.Listen has already
// succeeded, so the failure happens with a bound port in hand. The assertion is
// that the port can be bound AGAIN -- a connection, not s.IsRunning(), because
// the field is what the invariant reads and asserting it would re-assert the
// defect instead of the property.
func TestServer_FailedStartLeavesNothingListening(t *testing.T) {
	addr := freeAddr(t)

	s := NewServer(logging.NewLoggerFromConfig(logging.DefaultConfig()), ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    addr,
		Registry:       nil, // fails AFTER net.Listen succeeded
	})

	require.Error(t, s.Start(context.Background()), "a nil Registry must fail the start")

	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err,
		"a Start that returned an error must leave nothing listening, or Stop()'s `running` guard is a false invariant")
	require.NoError(t, ln.Close())
}

// The shutdown that arrives by context cancellation must go through the same
// door as an explicit Stop(). It used to be a second, parallel path that called
// Shutdown directly and discarded its error, so a failure there left no trace
// while the identical failure through Stop() was logged.
func TestServer_ContextCancellationShutsDownThroughStop(t *testing.T) {
	addr := freeAddr(t)

	s := NewServer(logging.NewLoggerFromConfig(logging.DefaultConfig()), ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    addr,
		Registry:       prometheus.NewRegistry(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, s.Start(ctx))
	require.Eventually(t, func() bool { return listening(addr) }, 3*time.Second, 50*time.Millisecond,
		"the metrics server must be up before cancelling, or the test proves nothing")

	cancel()

	require.Eventually(t, func() bool { return !listening(addr) }, 3*time.Second, 50*time.Millisecond,
		"cancelling the context must shut the server down")

	// This is what tells the two paths apart: a direct Shutdown would have left
	// the port closed with running still true. Only going through Stop() clears it.
	require.Eventually(t, func() bool { return !s.IsRunning() }, 3*time.Second, 50*time.Millisecond,
		"the context path must pass through Stop(), not shut the server down behind its back")
}

// syncBuffer is a Writer the shutdown goroutines and the test can share. A bare
// bytes.Buffer would be a data race here -- unlike the miner tests this borrows
// the pattern from, the writes come from goroutines the server owns.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The per-server "stopping X server" lines were removed because a goroutine
// that delegates would emit one without stopping anything -- a log that lies
// half the time. The single line that replaced them is only honest while it
// sits BELOW the `running` guard, and nothing about the code says so: moving it
// one line up compiles, passes every other test, and restores the exact defect
// this commit removed.
//
// So the property under test is a count, not a presence. Two goroutines reach
// Stop() on a normal shutdown and a caller's deferred Stop() follows them, and
// across all of that the line must appear exactly ONCE -- once for the one
// caller that actually shuts the servers down.
func TestServer_ShutdownLineIsEmittedOnlyByTheCallerThatShutsDown(t *testing.T) {
	addr := freeAddr(t)
	buf := &syncBuffer{}

	s := NewServer(zerolog.New(buf).Level(zerolog.TraceLevel), ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    addr,
		PprofEnabled:   true,
		PprofAddr:      freeAddr(t),
		Registry:       prometheus.NewRegistry(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, s.Start(ctx))
	require.Eventually(t, func() bool { return listening(addr) }, 3*time.Second, 50*time.Millisecond,
		"the metrics server must be up before cancelling, or the test proves nothing")

	cancel() // both ctx.Done goroutines race into Stop()
	require.Eventually(t, func() bool { return !s.IsRunning() }, 3*time.Second, 50*time.Millisecond,
		"the context path must shut the servers down")

	// And the caller's own deferred Stop(), which every binary does.
	require.NoError(t, s.Stop())

	require.Equal(t, 1, strings.Count(buf.String(), "stopping observability servers"),
		"the line must be emitted only by the caller that actually shuts the servers down; "+
			"above the `running` guard it would be emitted by every caller, which is the lie this commit removed")
}
