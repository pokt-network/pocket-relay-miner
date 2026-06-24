//go:build test

package cache

import (
	"fmt"
	"testing"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
)

// TestSessionCacheL1_BoundedGrowth is the regression guard for the unbounded L1
// session map: storeSession must prune entries whose height is far below the
// newest so the sync.Map stays bounded instead of growing one entry per distinct
// session served for the process lifetime.
func TestSessionCacheL1_BoundedGrowth(t *testing.T) {
	c := &RedisSessionCache{keys: CacheKeys{Prefix: "ha:cache"}}

	const total = sessionCacheL1KeepHeights + 800
	for h := int64(1); h <= total; h++ {
		c.storeSession(fmt.Sprintf("app/svc/%d", h), h, &sessiontypes.Session{SessionId: fmt.Sprintf("s%d", h)})
	}

	n := 0
	c.sessionCache.Range(func(_, _ any) bool {
		n++
		return true
	})
	require.LessOrEqual(t, n, sessionCacheL1KeepHeights+1,
		"L1 session map must stay bounded to the height window, not grow per session")

	_, newest := c.sessionCache.Load(fmt.Sprintf("app/svc/%d", int64(total)))
	require.True(t, newest, "the newest session must be retained")
	_, oldest := c.sessionCache.Load(fmt.Sprintf("app/svc/%d", int64(1)))
	require.False(t, oldest, "a session far below the keep window must be pruned")
}
