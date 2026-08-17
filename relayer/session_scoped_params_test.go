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
func newEpochValidator(oldGrace, newGrace uint64, boundary int64) (*relayValidator, *epochSharedParamCache) {
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
	).(*relayValidator)

	return v, paramCache
}

// TestCheckRewardEligibility_UsesParamsAtSessionEnd is the P1.2 regression test.
//
// Governance SHORTENS grace_period_end_offset_blocks after a session has ended.
// The chain still measures that session's window under the epoch it belongs to,
// so a relay inside the old grace window is still rewardable. Reading the live
// (shortened) offset instead rejects a relay the chain would have paid.
func TestCheckRewardEligibility_UsesParamsAtSessionEnd(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		oldGrace     = uint64(10) // last accept block = 100 + 10 - 1 = 109
		newGrace     = uint64(2)  // last accept block = 100 +  2 - 1 = 101
		currentH     = int64(105) // inside old grace, outside new grace
	)

	v, paramCache := newEpochValidator(oldGrace, newGrace, sessionEnd)
	v.SetCurrentBlockHeight(currentH)

	err := v.CheckRewardEligibility(context.Background(), relayWithSession(sessionStart, sessionEnd))
	require.NoError(t, err,
		"relay inside the session's OWN grace window must stay eligible after governance shortens the offset")

	heights, latestCalls := paramCache.snapshot()
	require.Equal(t, []int64{sessionEnd}, heights, "params must be resolved at the session END height")
	require.Zero(t, latestCalls, "the live params must not be consulted")
}

// TestCheckRewardEligibility_ActiveSessionUsesLiveParams is the F1 regression.
//
// For an ACTIVE session the end height is in the FUTURE. Querying shared params at
// that future height pins today's live value under a future cache key (poisoning it
// against a later governance change) and leans on pocketd answering future heights.
// poktroll resolves a future projection against the LIVE grid, so an active session
// must read the live params directly — never at the future end height.
func TestCheckRewardEligibility_ActiveSessionUsesLiveParams(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		currentH     = int64(95) // still inside the session -> end height is in the future
	)

	v, paramCache := newEpochValidator(10, 10, sessionEnd)
	v.SetCurrentBlockHeight(currentH)

	err := v.CheckRewardEligibility(context.Background(), relayWithSession(sessionStart, sessionEnd))
	require.NoError(t, err, "an active session's relay is well inside its window and must be eligible")

	heights, latestCalls := paramCache.snapshot()
	require.NotContains(t, heights, sessionEnd, "an active session must NOT query params at the future end height")
	require.Positive(t, latestCalls, "an active session must resolve the live params")
}

// TestCheckRewardEligibility_StillRejectsGenuinelyLateRelay proves the fix did not
// defang the check: past its own epoch's cutoff, the relay is still rejected.
func TestCheckRewardEligibility_StillRejectsGenuinelyLateRelay(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		oldGrace     = uint64(10) // last accept block = 109
		currentH     = int64(115) // past even the old window
	)

	v, _ := newEpochValidator(oldGrace, oldGrace, sessionEnd)
	v.SetCurrentBlockHeight(currentH)

	err := v.CheckRewardEligibility(context.Background(), relayWithSession(sessionStart, sessionEnd))
	require.Error(t, err)
	require.Contains(t, err.Error(), "relay too late")
}

// TestCheckRewardEligibility_UnknownHeightIsEligible covers the startup case where
// no block height is known yet.
func TestCheckRewardEligibility_UnknownHeightIsEligible(t *testing.T) {
	v, paramCache := newEpochValidator(10, 2, 100)
	// currentBlockHeight left at 0.

	require.NoError(t, v.CheckRewardEligibility(context.Background(), relayWithSession(91, 100)))

	heights, latestCalls := paramCache.snapshot()
	require.Empty(t, heights)
	require.Zero(t, latestCalls, "must short-circuit before any params lookup")
}

// TestGetTargetSessionBlockHeight_GracePathUsesParamsAtSessionEnd is the second
// half of P1.2. The grace-period branch must agree with CheckRewardEligibility
// about which params epoch applies, or the relayer accepts a relay it then marks
// ineligible (or the reverse).
func TestGetTargetSessionBlockHeight_GracePathUsesParamsAtSessionEnd(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		oldGrace     = uint64(10) // grace elapses after height 110
		newGrace     = uint64(2)  // grace elapses after height 102
		currentH     = int64(105) // inside old grace, outside new grace
	)

	v, paramCache := newEpochValidator(oldGrace, newGrace, sessionEnd)
	v.SetCurrentBlockHeight(currentH)

	height, err := v.getTargetSessionBlockHeight(context.Background(), relayWithSession(sessionStart, sessionEnd))
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

	v, paramCache := newEpochValidator(10, 2, sessionEnd)
	v.SetCurrentBlockHeight(95) // still inside the session

	height, err := v.getTargetSessionBlockHeight(context.Background(), relayWithSession(sessionStart, sessionEnd))
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

	v, _ := newEpochValidator(grace, grace, sessionEnd)
	v.SetCurrentBlockHeight(150)

	_, err := v.getTargetSessionBlockHeight(context.Background(), relayWithSession(sessionStart, sessionEnd))
	require.Error(t, err)
	require.Contains(t, err.Error(), "session expired")
}
