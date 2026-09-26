//go:build test

package miner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// syncBuf lets the claimer's logger and the test share a buffer safely.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TryClaim releases the claim when the callback fails, and that Release used to
// have its error discarded. Reading Release only as far as its FIRST error
// return makes the discard look harmless -- that path logs at Info before
// returning. The SECOND does not: a failure in the Lua check-and-delete comes
// back wrapped and says nothing.
//
// This test forces the second one, by closing the Redis client before the
// release runs. What it pins is that the failure is visible: without the log,
// the claim key stays until its TTL expires and no replica takes the supplier,
// while this one has already moved on -- indistinguishable, in the logs, from a
// supplier nobody wanted.
func TestTryClaim_ReleaseFailureAfterCallbackIsReported(t *testing.T) {
	rc, _ := newTestRedis(t)
	buf := &syncBuf{}

	c := NewSupplierClaimer(zerolog.New(buf).Level(zerolog.TraceLevel), rc, "instance-under-test", SupplierClaimerConfig{})
	// The callback fails AND takes Redis down with it, which is the ordering the
	// real path has: the claim is acquired first (Redis working), the callback
	// then fails, and only after that does Release run -- here with a closed
	// client, so it takes the Lua exit that reports nothing.
	c.onClaimFn = func(_ context.Context, _ string) error {
		require.NoError(t, rc.Close())
		return errors.New("injected: lifecycle refused to start")
	}

	require.False(t, c.TryClaim(context.Background(), "supplier-1"),
		"a failing callback must abandon the claim")

	out := buf.String()
	require.True(t, strings.Contains(out, "could not release the claim"),
		"a release that failed silently leaves the supplier held until its TTL with nothing said; got: %s", out)
}
