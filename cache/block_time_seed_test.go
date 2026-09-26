//go:build test

package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// The transactions a miner signs are anchored on the chain's block time. Until
// the first block event arrives over pub/sub the subscriber knows none, and in
// the process that wins the election the leader that publishes those events
// starts later: the time read at startup is seeded here so nothing is signed
// against the zero time.

func seedSubscriber(t *testing.T) *RedisBlockSubscriber {
	t.Helper()
	return NewRedisBlockSubscriber(logging.NewLoggerFromConfig(logging.DefaultConfig()), nil, nil)
}

func TestSeedBlockTime_IsTheAnchorUntilTheFirstEvent(t *testing.T) {
	s := seedSubscriber(t)
	require.True(t, s.LatestBlockTime().IsZero(), "premise: a subscriber with no event knows no block time")

	seeded := time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)
	require.True(t, s.SeedBlockTime(seeded))
	require.Equal(t, seeded, s.LatestBlockTime(),
		"LINK seed-is-the-anchor: what was read at startup anchors the transactions signed before the first event")
	require.Zero(t, s.currentHeight,
		"LINK seed-time-only: the seed does not move the height")
}

// handleBlockEvent advances only on a strictly greater height, so a seeded
// height from a node one block ahead would make the events that follow look old.
func TestSeedBlockTime_DoesNotMakeRealEventsLookOld(t *testing.T) {
	s := seedSubscriber(t)
	require.True(t, s.SeedBlockTime(time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)))

	event := BlockEvent{Height: 43, Timestamp: time.Date(2026, 9, 17, 22, 6, 17, 0, time.UTC)}
	s.handleBlockEvent(event)
	require.Equal(t, event.Timestamp, s.LatestBlockTime(),
		"LINK seed-time-only: the first real event is taken, whatever height the seed came from")
	require.Equal(t, int64(43), s.currentHeight)
}

// The seed happens in Start and the events arrive on the pub/sub goroutine: the
// rule is that an event always wins, whichever gets there first.
func TestSeedBlockTime_NeverRewindsWhatAnEventBrought(t *testing.T) {
	s := seedSubscriber(t)
	event := BlockEvent{Height: 43, Timestamp: time.Date(2026, 9, 17, 22, 6, 17, 0, time.UTC)}
	s.handleBlockEvent(event)

	require.False(t, s.SeedBlockTime(time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)),
		"LINK seed-does-not-rewind: a seed that lost the race to an event is refused")
	require.Equal(t, event.Timestamp, s.LatestBlockTime(),
		"LINK seed-does-not-rewind: the time an event brought stays")
}

func TestSeedBlockTime_RefusesTheZeroTime(t *testing.T) {
	s := seedSubscriber(t)
	require.False(t, s.SeedBlockTime(time.Time{}))
	require.True(t, s.LatestBlockTime().IsZero())
}
