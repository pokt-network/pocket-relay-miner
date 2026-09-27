//go:build test

package relayer

import (
	"context"
	"errors"
	"sync"
	"testing"

	poktcrypto "github.com/pokt-network/poktroll/pkg/crypto"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/pokt-network/ring-go"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// atHeight is a fixed chain height for a test that does not care that the
// height moves. Production passes ProxyServer.CurrentBlockHeight instead, which
// reads the live value at every call -- a bridge must not freeze it, because
// each backend push on a subscription is its own relay arriving at its own
// height.
func atHeight(h int64) func() int64 { return func() int64 { return h } }

// errReachedRingVerification marks that validation got as far as the ring.
//
// It is the discriminator this test needs, not a detail: the grace branch
// returns ErrSessionExpired from getTargetSessionBlockHeight, BEFORE any ring
// work (validator.go:150-158). So reaching the ring proves the validator took
// the ACTIVE-session branch, which is the defect. A nil ring client would panic
// here instead, turning a precise answer into a stack trace.
var errReachedRingVerification = errors.New("reached ring verification")

// sentinelRingClient implements crypto.RingClient (two methods, verified in
// poktroll v0.1.35 pkg/crypto/interface.go:16-28) and never verifies anything.
type sentinelRingClient struct{}

var _ poktcrypto.RingClient = sentinelRingClient{}

func (sentinelRingClient) GetRingForAddressAtHeight(context.Context, string, int64) (*ring.Ring, error) {
	return nil, errReachedRingVerification
}

func (sentinelRingClient) VerifyRelayRequestSignature(context.Context, *servicetypes.RelayRequest) error {
	return errReachedRingVerification
}

// newGraceValidator builds a validator whose only wired collaborator is the ring
// sentinel. The session cache stays nil deliberately: both branches this test
// distinguishes return before it is reached (validator.go:150 for grace,
// validator.go:158 for the ring), so wiring it would hide which one ran.
func newGraceValidator(grace uint64, sessionEnd int64) RelayValidator {
	params := &sharedtypes.Params{
		// Non-zero: IsGracePeriodElapsed resolves the session grid through
		// GetSessionStartHeight, which divides by it.
		NumBlocksPerSession:        10,
		GracePeriodEndOffsetBlocks: grace,
	}
	return NewRelayValidator(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		&ValidatorConfig{},
		sentinelRingClient{},
		nil,
		&epochSharedParamCache{
			oldParams:     params,
			newParams:     params,
			epochBoundary: sessionEnd,
		},
		// No live-height source: liveHeight() reports 0, which
		// sessionHeightsPlausible treats as unknown and lets through
		// (session_height_bounds.go). That is deliberate -- the anti-abuse band
		// is a different bound and is not what these tests are about, so
		// switching it off isolates the grace decision. It also exercises the
		// nil guard in liveHeight.
		nil,
	)
}

// TestRelayPipeline_JudgesGraceAtTheRelaysOwnArrivalHeight is the defect, and it
// needs no concurrency at all.
//
// RelayPipeline.ValidateRelay (relay_pipeline.go:84) calls ValidateRelayRequest
// and never gives the validator an arrival height, although its RelayContext
// carries one. Its only callers are WebSocket (websocket.go:1004) and gRPC
// (relay_grpc_service.go:382). So on those two transports the validator judges
// every relay against whatever the last HTTP relay happened to leave in the
// shared field -- or against 0, which getTargetSessionBlockHeight
// (validator.go:321) reads as "session active".
//
// The grace period is therefore never evaluated there: a relay long past its
// grace window validates as live, is served, and is mined into a claim the chain
// will not pay. The money runs the opposite way to a lost relay -- nothing is
// rejected, nothing errors, and the work is simply never paid for.
//
// LINK: grace-uses-the-relays-own-arrival-height
func TestRelayPipeline_JudgesGraceAtTheRelaysOwnArrivalHeight(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		grace        = uint64(10) // the grace window closes after height 110
		arrival      = int64(150) // 40 blocks past it
		graceCloses  = int64(110)
	)

	pipeline := NewRelayPipeline(
		newGraceValidator(grace, sessionEnd),
		nil, // meter: ValidateRelay never reaches it
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
	)

	err := pipeline.ValidateRelay(context.Background(), &RelayContext{
		Request:            validRelayRequest(sessionStart, sessionEnd),
		ServiceID:          "seda",
		SupplierAddress:    "pokt1supplieroperatoraddr",
		SessionID:          "session1",
		ArrivalBlockHeight: arrival,
	})

	require.ErrorIs(t, err, ErrSessionExpired,
		"LINK grace-uses-the-relays-own-arrival-height: this relay arrived at %d, %d blocks past the grace window of session [%d,%d] which closes at %d. "+
			"Reaching ring verification instead (%v) means the validator took the active-session branch, i.e. it judged this relay against a height that is not this relay's.",
		arrival, arrival-graceCloses, sessionStart, sessionEnd, graceCloses, err)
}

// TestValidateRelayRequest_TwoConcurrentRelaysAreEachJudgedAtTheirOwnHeight is
// the half about concurrency, and it asserts WHICH HEIGHT decided each relay --
// not that the race detector stayed quiet.
//
// That distinction is the point. The defect this replaced was invisible to
// -race by construction: the shared height sat behind a mutex, so every access
// was clean and only the PAIR (write mine, then read whatever is there) was
// wrong. A test that merely runs two validations under -race would have passed
// against the defect.
//
// The two goroutines meet inside validation at a rendezvous, so the window in
// which the old code would have crossed the two heights is held open on
// purpose. No sleeps: the barrier is a WaitGroup, and if either side never
// arrives the test fails rather than hangs on a timer.
//
// LINK: each-relay-keeps-its-own-height
func TestValidateRelayRequest_TwoConcurrentRelaysAreEachJudgedAtTheirOwnHeight(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		grace        = uint64(10) // the grace window closes after height 110
		expiredAt    = int64(150) // past it
		liveAt       = int64(105) // inside it
	)

	params := &sharedtypes.Params{
		NumBlocksPerSession:        10,
		GracePeriodEndOffsetBlocks: grace,
	}

	// Both validations park here until the other has arrived, so each is inside
	// the height-dependent stretch while the other is too.
	var bothArrived sync.WaitGroup
	bothArrived.Add(2)

	v := NewRelayValidator(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		&ValidatorConfig{
			OwnsSupplierKey: func(string) bool {
				bothArrived.Done()
				bothArrived.Wait()
				return true
			},
		},
		sentinelRingClient{},
		nil,
		&epochSharedParamCache{oldParams: params, newParams: params, epochBoundary: sessionEnd},
		nil,
	)

	var expiredErr, liveErr error
	var done sync.WaitGroup
	done.Add(2)
	go func() {
		defer done.Done()
		expiredErr = v.ValidateRelayRequest(context.Background(),
			validRelayRequest(sessionStart, sessionEnd), expiredAt)
	}()
	go func() {
		defer done.Done()
		liveErr = v.ValidateRelayRequest(context.Background(),
			validRelayRequest(sessionStart, sessionEnd), liveAt)
	}()
	done.Wait()

	require.ErrorIs(t, expiredErr, ErrSessionExpired,
		"LINK each-relay-keeps-its-own-height: the relay that arrived at %d is past the grace window and must expire, whatever the other goroutine was validating",
		expiredAt)
	require.ErrorIs(t, liveErr, errReachedRingVerification,
		"LINK each-relay-keeps-its-own-height: the relay that arrived at %d is inside the grace window and must be validated, whatever the other goroutine was validating",
		liveAt)
	require.NotErrorIs(t, liveErr, ErrSessionExpired,
		"LINK each-relay-keeps-its-own-height: a live relay judged expired means it was measured against the other relay's height")
}

// TestRelayPipeline_AGraceRelayIsStillLive is the other half, and it must NOT
// move: inside the grace window the relay is legitimate and must reach the ring.
//
// Without it, "reject everything" would pass the test above, and the band that
// session_height_bounds.go enforces (10.000 blocks, roughly 7 days) would be
// mistaken for the grace period. They are different bounds and only one of them
// is about being paid.
//
// LINK: grace-relay-is-still-live
func TestRelayPipeline_AGraceRelayIsStillLive(t *testing.T) {
	const (
		sessionStart = int64(91)
		sessionEnd   = int64(100)
		grace        = uint64(10) // the grace window closes after height 110
		arrival      = int64(105) // inside it
	)

	pipeline := NewRelayPipeline(
		newGraceValidator(grace, sessionEnd),
		nil,
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
	)

	err := pipeline.ValidateRelay(context.Background(), &RelayContext{
		Request:            validRelayRequest(sessionStart, sessionEnd),
		ServiceID:          "seda",
		SupplierAddress:    "pokt1supplieroperatoraddr",
		SessionID:          "session1",
		ArrivalBlockHeight: arrival,
	})

	require.ErrorIs(t, err, errReachedRingVerification,
		"LINK grace-relay-is-still-live: a relay arriving at %d is inside the grace window of session [%d,%d] and must be validated, not expired",
		arrival, sessionStart, sessionEnd)
	require.NotErrorIs(t, err, ErrSessionExpired)
}
