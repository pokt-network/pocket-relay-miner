//go:build test

package cache

import (
	"context"
	"fmt"
	"testing"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestSessionCacheL1_BoundedGrowth is the regression guard for the unbounded L1
// session map: storeSession must prune entries whose height is far below the
// newest so the sync.Map stays bounded instead of growing one entry per distinct
// session served for the process lifetime.
func TestSessionCacheL1_BoundedGrowth(t *testing.T) {
	c := &RedisSessionCache{sessionCache: xsync.NewMap[string, sessionCacheL1Entry]()}

	const total = sessionCacheL1KeepHeights + 800
	for h := int64(1); h <= total; h++ {
		c.storeSession(fmt.Sprintf("app/svc/%d", h), h, &sessiontypes.Session{SessionId: fmt.Sprintf("s%d", h)})
	}

	n := 0
	c.sessionCache.Range(func(_ string, _ sessionCacheL1Entry) bool {
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

// RedisSessionCache used to carry lifecycle machinery it never used: Start did
// `_, c.cancelFn = context.WithCancel(ctx)`, discarding the context and keeping
// only the cancel, so Close cancelled a context nobody held; and a WaitGroup was
// waited on that nothing ever added to. It reads as a shutdown contract, and
// there is none to honour -- this cache runs no goroutines. These pin what the
// contract actually is now.
func TestSessionCacheLifecycle_StartIsUnaffectedByItsContext(t *testing.T) {
	c := &RedisSessionCache{
		sessionCache: xsync.NewMap[string, sessionCacheL1Entry](),
		logger:       logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "json"}),
	}

	// An already-cancelled context must not matter: nothing is launched from it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, c.Start(ctx))

	require.NoError(t, c.Close(), "Close must not wait on anything")
	require.NoError(t, c.Close(), "Close is idempotent")
	require.Error(t, c.Start(context.Background()), "a closed cache cannot be restarted")
}
