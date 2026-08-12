//go:build test

package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// recordingSharedQueryClient records which of GetParams (live) / GetParamsAtHeight
// the cache actually calls, and serves a different value for each so the
// difference is observable in the returned params.
type recordingSharedQueryClient struct {
	mu             sync.Mutex
	liveParams     *sharedtypes.Params
	byHeight       map[int64]*sharedtypes.Params
	liveCalls      int
	atHeightCalls  []int64
	fallbackParams *sharedtypes.Params
}

func (c *recordingSharedQueryClient) GetParams(_ context.Context) (*sharedtypes.Params, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.liveCalls++
	return c.liveParams, nil
}

func (c *recordingSharedQueryClient) GetParamsAtHeight(_ context.Context, height int64) (*sharedtypes.Params, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.atHeightCalls = append(c.atHeightCalls, height)
	if p, ok := c.byHeight[height]; ok {
		return p, nil
	}
	if c.fallbackParams != nil {
		return c.fallbackParams, nil
	}
	return c.liveParams, nil
}

func (c *recordingSharedQueryClient) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	p, err := c.GetParamsAtHeight(ctx, queryHeight)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetSessionGracePeriodEndHeight(p, queryHeight), nil
}

func (c *recordingSharedQueryClient) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	p, err := c.GetParamsAtHeight(ctx, queryHeight)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetClaimWindowOpenHeight(p, queryHeight), nil
}

func (c *recordingSharedQueryClient) GetProofWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	p, err := c.GetParamsAtHeight(ctx, queryHeight)
	if err != nil {
		return 0, err
	}
	return sharedtypes.GetProofWindowOpenHeight(p, queryHeight), nil
}

func (c *recordingSharedQueryClient) GetEarliestSupplierClaimCommitHeight(_ context.Context, queryHeight int64, _ string) (int64, error) {
	return queryHeight, nil
}

func (c *recordingSharedQueryClient) GetEarliestSupplierProofCommitHeight(_ context.Context, queryHeight int64, _ string) (int64, error) {
	return queryHeight, nil
}

func (c *recordingSharedQueryClient) snapshot() (liveCalls int, atHeight []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveCalls, append([]int64(nil), c.atHeightCalls...)
}

// newAtHeightTestCache builds a RedisSharedParamCache over miniredis.
func newAtHeightTestCache(t *testing.T, ctx context.Context, sharedClient *recordingSharedQueryClient) *RedisSharedParamCache {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	c := NewRedisSharedParamCache(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		sharedClient,
		nil, // blockClient: only GetLatestSharedParams needs it
		CacheConfig{},
	)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestGetSharedParams_QueriesAtRequestedHeight is the regression test for a
// latent bug: GetSharedParams caches by height but used to fetch the LIVE params
// via GetParams. Because the entry is written to Redis under that height's key and
// shared with every replica, the wrong value was both returned to the caller and
// published fleet-wide.
//
// It was invisible while GetLatestSharedParams was the only caller — live and
// at-latest agree — and became live the moment session-scoped reads started
// passing a session end height.
func TestGetSharedParams_QueriesAtRequestedHeight(t *testing.T) {
	const sessionEnd = int64(100)

	sharedClient := &recordingSharedQueryClient{
		liveParams: &sharedtypes.Params{NumBlocksPerSession: 20, GracePeriodEndOffsetBlocks: 2},
		byHeight: map[int64]*sharedtypes.Params{
			sessionEnd: {NumBlocksPerSession: 10, GracePeriodEndOffsetBlocks: 10},
		},
	}

	ctx := context.Background()
	c := newAtHeightTestCache(t, ctx, sharedClient)

	params, err := c.GetSharedParams(ctx, sessionEnd)
	require.NoError(t, err)
	require.Equal(t, uint64(10), params.GetNumBlocksPerSession(),
		"must serve the params effective at the requested height")
	require.Equal(t, uint64(10), params.GetGracePeriodEndOffsetBlocks())

	liveCalls, atHeight := sharedClient.snapshot()
	require.Equal(t, []int64{sessionEnd}, atHeight, "the chain read must be at the requested height")
	require.Zero(t, liveCalls, "the live params RPC must not be used for a height-keyed read")
}

// TestGetSharedParams_CachesUnderRequestedHeight proves the correct value — not a
// live one — is what lands in L1/L2 under that height's key.
func TestGetSharedParams_CachesUnderRequestedHeight(t *testing.T) {
	const sessionEnd = int64(100)

	sharedClient := &recordingSharedQueryClient{
		liveParams: &sharedtypes.Params{NumBlocksPerSession: 20},
		byHeight: map[int64]*sharedtypes.Params{
			sessionEnd: {NumBlocksPerSession: 10},
		},
	}

	ctx := context.Background()
	c := newAtHeightTestCache(t, ctx, sharedClient)

	first, err := c.GetSharedParams(ctx, sessionEnd)
	require.NoError(t, err)
	require.Equal(t, uint64(10), first.GetNumBlocksPerSession())

	// Second read is served from cache — the chain must not be hit again.
	second, err := c.GetSharedParams(ctx, sessionEnd)
	require.NoError(t, err)
	require.Equal(t, uint64(10), second.GetNumBlocksPerSession())

	liveCalls, atHeight := sharedClient.snapshot()
	require.Len(t, atHeight, 1, "the second read must be a cache hit")
	require.Zero(t, liveCalls)
}

// TestGetSharedParams_DistinctHeightsDoNotAlias guards the key: two heights in two
// different params epochs must not collapse onto one cached value.
func TestGetSharedParams_DistinctHeightsDoNotAlias(t *testing.T) {
	sharedClient := &recordingSharedQueryClient{
		liveParams: &sharedtypes.Params{NumBlocksPerSession: 20},
		byHeight: map[int64]*sharedtypes.Params{
			100: {NumBlocksPerSession: 10},
			200: {NumBlocksPerSession: 20},
		},
	}

	ctx := context.Background()
	c := newAtHeightTestCache(t, ctx, sharedClient)

	old, err := c.GetSharedParams(ctx, 100)
	require.NoError(t, err)
	recent, err := c.GetSharedParams(ctx, 200)
	require.NoError(t, err)

	require.Equal(t, uint64(10), old.GetNumBlocksPerSession())
	require.Equal(t, uint64(20), recent.GetNumBlocksPerSession())

	_, atHeight := sharedClient.snapshot()
	require.Equal(t, []int64{100, 200}, atHeight)
}
