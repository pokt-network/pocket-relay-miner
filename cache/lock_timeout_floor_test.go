//go:build test

package cache

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// A LockTimeout below the floor is treated as UNSET, not honoured.
//
// The units are easy to get wrong on a time.Duration field, and the mistake is
// silent: `LockTimeout: 5` is five NANOSECONDS, go-redis truncates it to PX 1,
// and the lock expires in about a millisecond -- so it dedups nothing while
// every reader still pays the contended path. Wired exactly that way in
// miner/leader_controller.go, where the previous `== 0` default waved it
// through, because an absurd value is not a zero one. Measured 2026-08-28.
func TestLockTimeout_BelowTheFloorIsTreatedAsUnset(t *testing.T) {
	logger := logging.Logger(zerolog.Nop())

	for _, tc := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"the five-nanoseconds mistake", 5, 5 * time.Second},
		{"unset", 0, 5 * time.Second},
		{"one nanosecond below the floor", time.Millisecond - 1, 5 * time.Second},
		{"the floor itself is honoured", time.Millisecond, time.Millisecond},
		{"a sane value is honoured", 2 * time.Second, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supplier := NewRedisSupplierParamCache(logger, nil, nil, CacheConfig{LockTimeout: tc.in})
			require.Equal(t, tc.want, supplier.config.LockTimeout,
				"supplier param cache honoured %v", tc.in)

			shared := NewRedisSharedParamCache(logger, nil, nil, nil, CacheConfig{LockTimeout: tc.in})
			require.Equal(t, tc.want, shared.config.LockTimeout,
				"shared param cache honoured %v", tc.in)
		})
	}
}
