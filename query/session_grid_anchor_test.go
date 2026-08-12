//go:build test

package query

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// Grid arithmetic these tests depend on (GetSessionStartHeight with the #543
// anchored grid; below an anchor the helper falls back to the genesis block-1 grid
// but KEEPS the params' num_blocks_per_session):
//
//	live params      N=20, anchor=1000  ->  h=81 => 81,  h=91 => 81   (COLLIDE)
//	historical params N=10, anchor=0     ->  h=81 => 81,  h=91 => 91   (distinct)
//
// So heights 81 and 91 are two DIFFERENT real sessions that alias onto one cache
// key if the live params are used to derive it.
const (
	gridLiveAnchor  = 1000
	gridProbeEarly  = int64(81)
	gridProbeLate   = int64(91)
	gridAboveAnchor = int64(1000)
)

func newGridTestClients(t *testing.T, mock *mockQueryServer, address string) *Clients {
	t.Helper()
	qc, err := NewQueryClients(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })
	return qc
}

// TestGetSession_BelowGridAnchorDoesNotAliasSessions is the P2.1 regression test.
//
// The session cache key is derived from GetSessionStartHeight(params, blockHeight).
// Below the LIVE session_grid_anchor_height the params describe an epoch this
// height never belonged to: the helper falls back to the GENESIS grid while still
// using the live num_blocks_per_session, so the derived start height belongs to no
// real session — and two heights in DIFFERENT real sessions collapse onto one key.
// GetSession then returns the WRONG cached session, which surfaces downstream as a
// session ID mismatch and rejects legitimate relays. The exposure is the grace
// window right after a num_blocks_per_session change.
func TestGetSession_BelowGridAnchorDoesNotAliasSessions(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.sharedParams = &sharedtypes.Params{
		NumBlocksPerSession:     20,
		SessionGridAnchorHeight: gridLiveAnchor,
	}
	mock.sharedParamsAtHeight = &sharedtypes.Params{
		NumBlocksPerSession:     10,
		SessionGridAnchorHeight: 0,
	}

	// The chain echoes the requested height into the session ID, so serving an
	// aliased cache entry is directly observable in the returned session.
	var mu sync.Mutex
	var requested []int64
	mock.getSessionFunc = func(_ context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		mu.Lock()
		requested = append(requested, req.BlockHeight)
		mu.Unlock()
		return &sessiontypes.QueryGetSessionResponse{
			Session: &sessiontypes.Session{
				SessionId:           fmt.Sprintf("session-at-%d", req.BlockHeight),
				NumBlocksPerSession: 10,
			},
		}, nil
	}

	qc := newGridTestClients(t, mock, address)
	ctx := context.Background()

	early, err := qc.Session().GetSession(ctx, "pokt1app", "seda", gridProbeEarly)
	require.NoError(t, err)
	late, err := qc.Session().GetSession(ctx, "pokt1app", "seda", gridProbeLate)
	require.NoError(t, err)

	require.Equal(t, fmt.Sprintf("session-at-%d", gridProbeEarly), early.SessionId)
	require.Equal(t, fmt.Sprintf("session-at-%d", gridProbeLate), late.SessionId,
		"two heights in DIFFERENT real sessions must not collapse onto one cache key")
	require.NotEqual(t, early.SessionId, late.SessionId)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int64{gridProbeEarly, gridProbeLate}, requested,
		"each distinct session must be resolved against the chain, not served from an aliased entry")
}

// TestGetSession_BelowGridAnchorResolvesParamsAtHeight asserts the mechanism, not
// just the outcome: below the anchor the at-height params RPC must be consulted,
// with the probe height.
func TestGetSession_BelowGridAnchorResolvesParamsAtHeight(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.sharedParams = &sharedtypes.Params{
		NumBlocksPerSession:     20,
		SessionGridAnchorHeight: gridLiveAnchor,
	}
	mock.sharedParamsAtHeight = &sharedtypes.Params{
		NumBlocksPerSession:     10,
		SessionGridAnchorHeight: 0,
	}

	var mu sync.Mutex
	var atHeightCalls []int64
	mock.onParamsAtHeight = func(h int64) {
		mu.Lock()
		atHeightCalls = append(atHeightCalls, h)
		mu.Unlock()
	}
	mock.getSessionFunc = func(_ context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		return &sessiontypes.QueryGetSessionResponse{
			Session: &sessiontypes.Session{SessionId: "s", NumBlocksPerSession: 10},
		}, nil
	}

	qc := newGridTestClients(t, mock, address)

	_, err := qc.Session().GetSession(context.Background(), "pokt1app", "seda", gridProbeEarly)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, atHeightCalls, gridProbeEarly,
		"a height below the live grid anchor must resolve params at that height")
}

// TestGetSession_AtOrAboveGridAnchorUsesLiveParams pins the fast path: at or above
// the live anchor the live grid IS the grid in effect, so the at-height params RPC
// must NOT be consulted. This matters because GetSession is called with the CURRENT
// block height on the relay hot path — querying at-height every block would add a
// paramsAtHeightCache entry per block and thrash that memo for every other caller.
func TestGetSession_AtOrAboveGridAnchorUsesLiveParams(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	mock.sharedParams = &sharedtypes.Params{
		NumBlocksPerSession:     20,
		SessionGridAnchorHeight: gridLiveAnchor,
	}
	mock.sharedParamsAtHeight = &sharedtypes.Params{
		NumBlocksPerSession:     10,
		SessionGridAnchorHeight: 0,
	}

	var mu sync.Mutex
	var atHeightCalls []int64
	sessionQueries := 0
	mock.onParamsAtHeight = func(h int64) {
		mu.Lock()
		atHeightCalls = append(atHeightCalls, h)
		mu.Unlock()
	}
	mock.getSessionFunc = func(_ context.Context, _ *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
		mu.Lock()
		sessionQueries++
		mu.Unlock()
		return &sessiontypes.QueryGetSessionResponse{
			Session: &sessiontypes.Session{SessionId: "live-session", NumBlocksPerSession: 20},
		}, nil
	}

	qc := newGridTestClients(t, mock, address)
	ctx := context.Background()

	// Both heights sit inside the SAME 20-block session under the live grid.
	_, err := qc.Session().GetSession(ctx, "pokt1app", "seda", gridAboveAnchor)
	require.NoError(t, err)
	_, err = qc.Session().GetSession(ctx, "pokt1app", "seda", gridAboveAnchor+10)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, atHeightCalls,
		"at or above the anchor the live grid applies; the at-height memo must not be touched per block")
	require.Equal(t, 1, sessionQueries,
		"two heights in the same live-grid session must share one cache entry")
}
