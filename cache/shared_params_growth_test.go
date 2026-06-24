//go:build test

package cache

import (
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// TestSharedParamsLocal_BoundedGrowth is the regression guard for the unbounded
// per-height L1 leak: RedisSharedParamCache.localCache used to grow ~1 entry per
// block forever (only dead Delete paths existed). storeLocal must prune entries
// far below the newest height so the map stays bounded.
func TestSharedParamsLocal_BoundedGrowth(t *testing.T) {
	c := &RedisSharedParamCache{}

	const total = 500
	for h := int64(1); h <= total; h++ {
		c.storeLocal(c.keys.SharedParams(h), h, &sharedtypes.Params{NumBlocksPerSession: uint64(h)})
	}

	n := 0
	c.localCache.Range(func(_, _ any) bool {
		n++
		return true
	})
	require.LessOrEqual(t, n, sharedParamsLocalKeepHeights+1,
		"L1 must stay bounded to the height window, not grow ~1 entry/block forever")

	// The newest height is retained; a far-older one was pruned.
	_, newest := c.localCache.Load(c.keys.SharedParams(total))
	require.True(t, newest, "the newest height must be retained")
	_, oldest := c.localCache.Load(c.keys.SharedParams(1))
	require.False(t, oldest, "a far-below-window height must be pruned")
}

// TestSharedParamsLocal_TTLFloor verifies the L1 read treats an entry past the
// max-age floor as a miss (the cache-TTL mandate), even though params-at-height
// are immutable.
func TestSharedParamsLocal_TTLFloor(t *testing.T) {
	c := &RedisSharedParamCache{}
	key := c.keys.SharedParams(100)

	// A fresh entry is a hit.
	c.storeLocal(key, 100, &sharedtypes.Params{NumBlocksPerSession: 4})
	cached, ok := c.localCache.Load(key)
	require.True(t, ok)
	e, ok := cached.(sharedParamLocalEntry)
	require.True(t, ok)
	require.Less(t, time.Since(e.cachedAt), sharedParamsLocalTTL, "a just-stored entry is within the floor")

	// Force the entry past the floor and confirm the freshness gate rejects it.
	prev := sharedParamsLocalTTL
	sharedParamsLocalTTL = 0
	t.Cleanup(func() { sharedParamsLocalTTL = prev })
	require.False(t, time.Since(e.cachedAt) < sharedParamsLocalTTL,
		"past the TTL floor the L1 entry must be treated as a miss")
}
