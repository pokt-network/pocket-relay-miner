//go:build test

package leader

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestExclusiveLock_IsHeldByOneOwner(t *testing.T) {
	ctx := context.Background()
	l := &exclusiveLock{}

	got, err := l.acquire(ctx, "a", 30)
	require.NoError(t, err)
	require.Equal(t, 1, got)
	got, _ = l.acquire(ctx, "b", 30)
	require.Equal(t, 0, got, "held by a")
	got, _ = l.renew(ctx, "b", 30)
	require.Equal(t, 0, got, "only the owner renews")
	got, _ = l.release(ctx, "b")
	require.Equal(t, 0, got, "only the owner releases")
	got, _ = l.renew(ctx, "a", 30)
	require.Equal(t, 1, got)
	got, _ = l.release(ctx, "a")
	require.Equal(t, 1, got)
	got, _ = l.acquire(ctx, "b", 30)
	require.Equal(t, 1, got, "free again")
}

// The exclusive elector runs the same election as over Redis and wins it at
// the first attempt, then keeps it: OnElected once, OnLost never.
func TestExclusiveElector_LeadsAtOnceAndNeverLoses(t *testing.T) {
	ctx := context.Background()
	e := NewExclusiveGlobalLeaderElector(zerolog.Nop(), "standalone-1", GlobalLeaderElectorConfig{LeaderTTL: 30 * time.Second, HeartbeatRate: time.Hour})
	var elected, lost atomic.Int32
	done := make(chan struct{}, 4)
	e.OnElected(func(context.Context) { elected.Add(1); done <- struct{}{} })
	e.OnLost(func(context.Context) { lost.Add(1); done <- struct{}{} })

	e.attemptLeadership(ctx)
	<-done
	require.True(t, e.IsLeader())
	for i := 0; i < 3; i++ {
		e.attemptLeadership(ctx) // the renewals of later heartbeats
	}

	e.wg.Wait()
	require.True(t, e.IsLeader())
	require.Equal(t, int32(1), elected.Load())
	require.Zero(t, lost.Load())
}
