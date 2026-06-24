//go:build test

package cache

import (
	"context"
	"testing"
	"time"

	"github.com/pokt-network/poktroll/pkg/client"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// frozenSessionQueryClient models the L3 (chain) session source for the session
// cache. GetSession returns a session whose SessionId tracks frozenID; the test
// flips frozenID to simulate the chain returning a corrected/different session
// for the same height (e.g. a non-deterministic session re-fetch). Embedding the
// interface satisfies GetParams (and any future methods) without hand-writing
// them — only GetSession is exercised by RedisSessionCache.GetSession.
type frozenSessionQueryClient struct {
	client.SessionQueryClient
	frozenID   string
	appAddress string
	serviceID  string
	endHeight  int64
}

func (f *frozenSessionQueryClient) GetSession(
	_ context.Context,
	appAddress, serviceID string,
	height int64,
) (*sessiontypes.Session, error) {
	return &sessiontypes.Session{
		SessionId: f.frozenID,
		Header: &sessiontypes.SessionHeader{
			ApplicationAddress:      appAddress,
			ServiceId:               serviceID,
			SessionStartBlockHeight: height,
			SessionEndBlockHeight:   f.endHeight,
		},
	}, nil
}

// stubSharedQueryClient supplies just enough shared params for
// calculateSessionTTL. Embedding the interface covers the rest.
type stubSharedQueryClient struct {
	client.SharedQueryClient
}

func (s *stubSharedQueryClient) GetParams(_ context.Context) (*sharedtypes.Params, error) {
	return &sharedtypes.Params{
		NumBlocksPerSession:        10,
		GracePeriodEndOffsetBlocks: 1,
	}, nil
}

// stubBlockClient returns a fixed last block for calculateSessionTTL.
type stubBlockClient struct {
	client.BlockClient
	height int64
}

func (s *stubBlockClient) LastBlock(_ context.Context) client.Block {
	return localclient.NewSimpleBlock(s.height, []byte{0x01}, time.Unix(0, 0))
}

// TestSessionCache_L1RefreshesAfterTTL is the cache-TTL mandate regression test
// for the session cache, mirroring TestServiceCache_L1RefreshesAfterTTL. The L1
// (in-process sync.Map) had NO TTL, so once a session was cached its value was
// frozen for the process lifetime — never aged out by any invalidation path,
// which could mask an L2/L3 correction for the same height key. The fix ages L1
// entries out after sessionCacheL1TTL so GetSession falls through to L2/L3. This
// test drives that change against the REAL session cache with miniredis.
func TestSessionCache_L1RefreshesAfterTTL(t *testing.T) {
	const (
		appAddr   = "pokt1app"
		serviceID = "svc"
		height    = int64(100)
		endHeight = int64(110)
	)

	redisClient := newTestRedis(t)
	sq := &frozenSessionQueryClient{
		frozenID:   "session-A",
		appAddress: appAddr,
		serviceID:  serviceID,
		endHeight:  endHeight,
	}
	sh := &stubSharedQueryClient{}
	// Keep the L2 TTL well above the test's wall-clock so the L2 entry never
	// expires under us: current height far below sessionValidUntil → long TTL.
	bc := &stubBlockClient{height: height}

	sc := NewRedisSessionCache(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		sq,
		sh,
		bc,
		CacheConfig{BlockTimeSeconds: 600},
	)

	ctx := context.Background()
	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Close() })

	// Use a large L1 TTL while we prove caching; restore the default after.
	origTTL := sessionCacheL1TTL
	sessionCacheL1TTL = time.Hour
	t.Cleanup(func() { sessionCacheL1TTL = origTTL })

	// Initial lazy load caches session-A in L1 (and L2).
	got, err := sc.GetSession(ctx, appAddr, serviceID, height)
	require.NoError(t, err)
	require.Equal(t, "session-A", got.SessionId)

	// The chain now returns a different session for the same height, and the L2
	// entry is removed so a stale-L1 re-read would have to fall through to L3.
	sq.frozenID = "session-B"
	key := sc.keys.Session(appAddr, serviceID, height)
	require.NoError(t, redisClient.Del(ctx, key).Err())

	// Within the (huge) L1 TTL: GetSession must still serve the cached session,
	// even though the downstream value already changed. Proves L1 caches.
	got, err = sc.GetSession(ctx, appAddr, serviceID, height)
	require.NoError(t, err)
	require.Equal(t, "session-A", got.SessionId,
		"L1 must keep serving the cached session while the entry is within sessionCacheL1TTL")

	// Expire L1: the next GetSession must treat L1 as a miss, re-read downstream,
	// and pick up the new session — the exact regression this test guards.
	sessionCacheL1TTL = 0
	got, err = sc.GetSession(ctx, appAddr, serviceID, height)
	require.NoError(t, err)
	require.Equal(t, "session-B", got.SessionId,
		"after sessionCacheL1TTL elapses, L1 must refresh and follow the downstream session")
}
