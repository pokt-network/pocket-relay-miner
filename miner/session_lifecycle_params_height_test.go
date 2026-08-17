//go:build test

package miner

import (
	"context"
	"sync"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// recordingSharedQueryClient wraps mockSharedQueryClient and records which
// resolution path each call took, so a test can assert on WHICH height (if any)
// the params were read at rather than only on the returned value — the two are
// indistinguishable by value alone whenever governance has not moved a param.
type recordingSharedQueryClient struct {
	mockSharedQueryClient

	mu           sync.Mutex
	atHeightArgs []int64
	liveCalls    int
}

func (c *recordingSharedQueryClient) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	c.mu.Lock()
	c.liveCalls++
	c.mu.Unlock()
	return c.mockSharedQueryClient.GetParams(ctx)
}

func (c *recordingSharedQueryClient) GetParamsAtHeight(ctx context.Context, queryHeight int64) (*sharedtypes.Params, error) {
	c.mu.Lock()
	c.atHeightArgs = append(c.atHeightArgs, queryHeight)
	c.mu.Unlock()
	return c.mockSharedQueryClient.GetParamsAtHeight(ctx, queryHeight)
}

func (c *recordingSharedQueryClient) snapshot() (atHeightArgs []int64, liveCalls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.atHeightArgs...), c.liveCalls
}

// newParamsHeightTestManager builds the smallest manager checkSessionTransitions
// needs when no session clears the pre-filter: logger, shared client and the
// in-memory session map. The Redis-backed store is never reached on that path.
//
// The claim window opens 101 blocks after session end, keeping every session in
// these tests below the pre-filter threshold so the run stops after the params
// resolution under test.
func newParamsHeightTestManager() (*SessionLifecycleManager, *recordingSharedQueryClient) {
	sharedClient := &recordingSharedQueryClient{
		mockSharedQueryClient: mockSharedQueryClient{
			params: &sharedtypes.Params{
				NumBlocksPerSession:          10,
				GracePeriodEndOffsetBlocks:   1,
				ClaimWindowOpenOffsetBlocks:  100,
				ClaimWindowCloseOffsetBlocks: 4,
				ProofWindowOpenOffsetBlocks:  0,
				ProofWindowCloseOffsetBlocks: 4,
			},
		},
	}

	m := &SessionLifecycleManager{
		logger:         logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:   sharedClient,
		activeSessions: xsync.NewMap[string, *SessionSnapshot](),
	}

	return m, sharedClient
}

// TestCheckSessionTransitions_ActiveSessionReadsLiveParams pins the height the
// per-block transition filter resolves params at for a session that is still
// RUNNING.
//
// An active session's end height is in the FUTURE. poktroll's GetParamsAtHeight
// walks back to the newest history entry <= the requested height, so a future
// height resolves to today's value anyway — but the query layer then caches that
// value under a future-height key it treats as immutable
// (query.storeParamsAtHeight), where a later governance change stays masked until
// the TTL floor lapses. Read live while the session is running.
func TestCheckSessionTransitions_ActiveSessionReadsLiveParams(t *testing.T) {
	const (
		currentHeight    = int64(1_000)
		sessionStart     = int64(1_001)
		sessionEndFuture = int64(1_010)
	)

	m, sharedClient := newParamsHeightTestManager()
	m.activeSessions.Store("session-active", &SessionSnapshot{
		SessionID:          "session-active",
		State:              SessionStateActive,
		SessionStartHeight: sessionStart,
		SessionEndHeight:   sessionEndFuture,
	})

	m.checkSessionTransitions(context.Background(), currentHeight)

	atHeightArgs, liveCalls := sharedClient.snapshot()
	require.NotContains(t, atHeightArgs, sessionEndFuture,
		"an active session must NOT resolve params at its future end height")
	require.Empty(t, atHeightArgs, "an active session must issue no at-height read at all")
	require.Equal(t, 1, liveCalls, "an active session must resolve the live params exactly once")
}

// TestCheckSessionTransitions_EndedSessionReadsAtItsOwnHeight is the other half:
// once the end height is in the PAST it is immutable, and the session's windows
// must be computed under the params epoch that session belonged to — not under
// whatever governance moved to afterwards.
func TestCheckSessionTransitions_EndedSessionReadsAtItsOwnHeight(t *testing.T) {
	const (
		currentHeight  = int64(1_000)
		sessionStart   = int64(961)
		sessionEndPast = int64(970)
	)

	m, sharedClient := newParamsHeightTestManager()
	m.activeSessions.Store("session-ended", &SessionSnapshot{
		SessionID:          "session-ended",
		State:              SessionStateActive,
		SessionStartHeight: sessionStart,
		SessionEndHeight:   sessionEndPast,
	})

	m.checkSessionTransitions(context.Background(), currentHeight)

	atHeightArgs, liveCalls := sharedClient.snapshot()
	require.Equal(t, []int64{sessionEndPast}, atHeightArgs,
		"an ended session must resolve params at its own end height")
	require.Zero(t, liveCalls, "an ended session must not fall back to the live params")
}
