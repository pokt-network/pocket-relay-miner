//go:build test

package redis

import (
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// unaskableClient is a client whose concrete type EffectivePoolOf cannot ask:
// the embedded interface is nil, and the type switch falls through to default.
type unaskableClient struct{ goredis.UniversalClient }

// TestTheSilenceBudgetComesFromTheRunningClientAndNeverGoesUnderItsFloor: the
// budget is derived from the pool timeout the client actually runs with, not
// from config -- a pool timeout left unset reaches the client as go-redis's own
// default, and config would have published the zero.
func TestTheSilenceBudgetComesFromTheRunningClientAndNeverGoesUnderItsFloor(t *testing.T) {
	// go-redis's own default: PoolTimeout is ReadTimeout+1s, and ReadTimeout
	// defaults to 5s. Verified against v9.22.0 options.go, not against a comment.
	defaults := goredis.NewClient(&goredis.Options{})
	t.Cleanup(func() { _ = defaults.Close() })
	pool, ok := EffectivePoolOf(defaults)
	require.True(t, ok, "control: a *redis.Client can be asked for its pool")
	require.Equal(t, 6*time.Second, pool.PoolTimeout,
		"premise: an unset pool timeout arrives as go-redis's default, which is what the formula must use")
	require.Equal(t, minDispatcherSilence, dispatcherSilenceBudget(zerolog.Nop(), defaults),
		"with the defaults the formula lands under the floor, and the floor is the operator's decision")

	// An operator who raises the pool timeout raises the budget with it:
	// otherwise the gate would close on a dispatcher still waiting for the
	// connection its own configuration allows it to wait for.
	patient := goredis.NewClient(&goredis.Options{PoolTimeout: 30 * time.Second})
	t.Cleanup(func() { _ = patient.Close() })
	require.Equal(t, healthyWriteBudget+30*time.Second+heartbeatInterval,
		dispatcherSilenceBudget(zerolog.Nop(), patient))

	// A client type that cannot report its pool falls back to the floor rather
	// than to a zero, which would close admission on every relay.
	require.Equal(t, minDispatcherSilence, dispatcherSilenceBudget(zerolog.Nop(), unaskableClient{}))
}

// TestAFreshPublisherRefusesUntilRedisAnswers: construction marks nothing, so a
// relayer whose Redis is unreachable from the start refuses instead of admitting
// for a whole budget on a mark nobody earned.
func TestAFreshPublisherRefusesUntilRedisAnswers(t *testing.T) {
	// Nothing is dialled here: the client is never used, because the publisher
	// must refuse before it reaches Redis at all.
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	p := NewBatchingPublisher(zerolog.Nop(), client, "ha:relays", time.Hour)
	require.NoError(t, p.Close())

	alive, err := p.DispatcherHealthy()
	require.False(t, alive)
	require.ErrorIs(t, err, errDispatcherNeverReachedRedis)

	at := time.Now()
	p.lastSuccess.Store(&at)
	alive, err = p.DispatcherHealthy()
	require.True(t, alive, "%v", err)

	// One nanosecond past the budget is refused, and the refusal carries the age
	// and the budget so an operator reading a 503 knows which of the two moved.
	p.now = func() time.Time { return at.Add(p.silenceBudget + time.Nanosecond) }
	alive, err = p.DispatcherHealthy()
	require.False(t, alive)
	require.ErrorIs(t, err, errDispatcherSilent)
	require.ErrorContains(t, err, p.silenceBudget.String())
}
