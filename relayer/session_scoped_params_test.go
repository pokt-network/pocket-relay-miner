//go:build test

package relayer

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// epochSharedParamCache implements cache.SharedParamCache and models a params
// epoch change: params at or below epochBoundary resolve to oldParams, above it
// (and "latest") resolve to newParams. Every at-height lookup is recorded.
//
// Its purpose is to make "which epoch was this session measured against?"
// directly observable, since reading the live value and reading at session end
// return the same thing whenever governance has not moved a param.
type epochSharedParamCache struct {
	mu            sync.Mutex
	oldParams     *sharedtypes.Params
	newParams     *sharedtypes.Params
	epochBoundary int64
	heights       []int64
	latestCalls   int
}

func (c *epochSharedParamCache) GetSharedParams(_ context.Context, height int64) (*sharedtypes.Params, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heights = append(c.heights, height)
	if height <= c.epochBoundary {
		return c.oldParams, nil
	}
	return c.newParams, nil
}

func (c *epochSharedParamCache) GetLatestSharedParams(_ context.Context) (*sharedtypes.Params, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latestCalls++
	return c.newParams, nil
}

func (c *epochSharedParamCache) InvalidateSharedParams(_ context.Context, _ int64) error { return nil }
func (c *epochSharedParamCache) Start(_ context.Context) error                           { return nil }
func (c *epochSharedParamCache) Close() error                                            { return nil }

func (c *epochSharedParamCache) snapshot() (heights []int64, latestCalls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.heights...), c.latestCalls
}

// relayWithSession builds a minimal relay request carrying only a session header,
// which is all the reward-eligibility and target-height paths read.
func relayWithSession(sessionStart, sessionEnd int64) *servicetypes.RelayRequest {
	return &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1app",
				ServiceId:               "seda",
				SessionId:               "session1",
				SessionStartBlockHeight: sessionStart,
				SessionEndBlockHeight:   sessionEnd,
			},
		},
	}
}

// newEpochValidator builds a validator over an epoch-aware params cache.
// grace offsets: oldGrace applies to sessions at/below boundary, newGrace above.
// liveHeight is what the process sees on chain, and it is a DIFFERENT quantity
// from the height a relay arrived at: the first bounds the plausibility band,
// the second decides the grace branch. They used to share one field, which is
// what let one relay be judged at another relay's height.
func newEpochValidator(oldGrace, newGrace uint64, boundary int64, liveHeight int64) (*relayValidator, *epochSharedParamCache) {
	// NumBlocksPerSession must be non-zero: IsGracePeriodElapsed resolves the session
	// grid through GetSessionStartHeight, which divides by it. Only the grace offset
	// differs between the two epochs, so it is the sole variable under test.
	paramCache := &epochSharedParamCache{
		oldParams: &sharedtypes.Params{
			NumBlocksPerSession:        10,
			GracePeriodEndOffsetBlocks: oldGrace,
		},
		newParams: &sharedtypes.Params{
			NumBlocksPerSession:        10,
			GracePeriodEndOffsetBlocks: newGrace,
		},
		epochBoundary: boundary,
	}

	v := NewRelayValidator(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		&ValidatorConfig{},
		nil, // ringClient: unused by the paths under test
		nil, // sessionCache: unused by the paths under test
		paramCache,
		func() int64 { return liveHeight },
	).(*relayValidator)

	return v, paramCache
}

// TestGetTargetSessionBlockHeight_GracePathUsesParamsAtSessionEnd is the P1.2
// regression test: the grace-period branch resolves the params epoch effective
// at the session's END height, matching the chain, or a governance change to
// grace_period_end_offset_blocks after a session ends silently moves which
// relays that session still accepts.
func TestGetTargetSessionBlockHeight_GracePathUsesParamsAtSessionEnd(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		oldGrace     = uint64(10) // grace elapses after height 110
		newGrace     = uint64(2)  // grace elapses after height 102
		currentH     = int64(105) // inside old grace, outside new grace
	)

	// liveHeight 0: the plausibility band is a different bound and is not under
	// test here. currentH is what THIS relay arrived at, and it is an argument.
	v, paramCache := newEpochValidator(oldGrace, newGrace, sessionEnd, 0)

	height, err := v.getTargetSessionBlockHeight(context.Background(), relayWithSession(sessionStart, sessionEnd), currentH)
	require.NoError(t, err, "a session still inside its OWN grace window must resolve, not expire")
	require.Equal(t, sessionEnd, height, "grace-period lookups use the session end height")

	heights, latestCalls := paramCache.snapshot()
	require.Equal(t, []int64{sessionEnd}, heights)
	require.Zero(t, latestCalls, "the live params must not be consulted")
}

// TestGetTargetSessionBlockHeight_ActiveSessionSkipsParams pins the documented
// cache-locality behaviour: an active session returns its start height without
// reading shared params at all. This is where this repo intentionally diverges
// from poktroll (which returns currentHeight), and it is why the active branch is
// unaffected by the params-epoch fix.
func TestGetTargetSessionBlockHeight_ActiveSessionSkipsParams(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
	)

	v, paramCache := newEpochValidator(10, 2, sessionEnd, 0)

	// 95: this relay arrived while its session was still open.
	height, err := v.getTargetSessionBlockHeight(context.Background(), relayWithSession(sessionStart, sessionEnd), 95)
	require.NoError(t, err)
	require.Equal(t, sessionStart, height,
		"active sessions resolve at their start height so the session cache key is stable for the whole session")

	heights, latestCalls := paramCache.snapshot()
	require.Empty(t, heights, "the active branch must not query shared params")
	require.Zero(t, latestCalls)
}

// TestGetTargetSessionBlockHeight_ExpiredSessionErrors covers the terminal case.
func TestGetTargetSessionBlockHeight_ExpiredSessionErrors(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		grace        = uint64(2) // grace elapses after height 102
	)

	v, _ := newEpochValidator(grace, grace, sessionEnd, 0)

	// 150: this relay arrived long after its session's grace window closed.
	_, err := v.getTargetSessionBlockHeight(context.Background(), relayWithSession(sessionStart, sessionEnd), 150)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSessionExpired)
}
