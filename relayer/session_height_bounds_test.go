//go:build test

package relayer

import (
	"context"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// TestSessionHeightsPlausible is the F3 unit contract: legitimate session headers
// (active and grace-period) pass; obviously-bogus ones that would otherwise drive
// a pre-signature at-height chain query are rejected — all without a chain call.
func TestSessionHeightsPlausible(t *testing.T) {
	const arrival = int64(1_000_000)

	testCases := []struct {
		name          string
		start         int64
		end           int64
		arrival       int64
		wantPlausible bool
	}{
		// Legitimate: active session straddling the current height.
		{name: "active session", start: arrival - 10, end: arrival + 10, arrival: arrival, wantPlausible: true},
		// Legitimate: freshly-opened session whose start is the current height.
		{name: "fresh session at head", start: arrival, end: arrival + 20, arrival: arrival, wantPlausible: true},
		// Legitimate: grace-period relay for a just-ended session.
		{name: "grace period ended session", start: arrival - 30, end: arrival - 5, arrival: arrival, wantPlausible: true},
		// Legitimate: relayer lags a couple blocks behind a fresh session.
		{name: "minor arrival lag", start: arrival + 3, end: arrival + 23, arrival: arrival, wantPlausible: true},

		// Bogus structural. These mirror poktroll's SessionHeader.ValidateBasic
		// exactly: start >= 1 and end STRICTLY greater than start.
		{name: "zero start", start: 0, end: 20, arrival: arrival, wantPlausible: false},
		{name: "negative start", start: -5, end: 20, arrival: arrival, wantPlausible: false},
		{name: "end equals start", start: 100, end: 100, arrival: arrival, wantPlausible: false},
		{name: "end before start", start: 200, end: 100, arrival: arrival, wantPlausible: false},

		// Bogus: absurd session length.
		{name: "absurd length", start: arrival, end: arrival + maxPlausibleSessionLengthBlocks + 1, arrival: arrival, wantPlausible: false},

		// Bogus: attacker-chosen far-future / far-past heights (the amplification vector).
		{name: "far future start", start: arrival + 5_000_000, end: arrival + 5_000_020, arrival: arrival, wantPlausible: false},
		{name: "far past end", start: 1, end: 21, arrival: arrival, wantPlausible: false},
		{name: "ancient session", start: 500_000, end: 500_020, arrival: arrival, wantPlausible: false},

		// Boot window: no block seen yet -> allowed through (later validation decides),
		// but still structurally sane.
		{name: "boot window sane header", start: 100, end: 120, arrival: 0, wantPlausible: true},
		{name: "boot window bogus structure", start: 120, end: 100, arrival: 0, wantPlausible: false},
		// Negative arrival is treated the same as "no block seen yet".
		{name: "negative arrival sane header", start: 100, end: 120, arrival: -1, wantPlausible: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionHeightsPlausible(tc.start, tc.end, tc.arrival)
			require.Equal(t, tc.wantPlausible, got,
				"sessionHeightsPlausible(start=%d, end=%d, arrival=%d)", tc.start, tc.end, tc.arrival)
		})
	}
}

// TestSessionHeightsPlausible_CollapsesAttackerSpace proves the bound reduces the
// attacker-usable distinct-height space to a bounded band around the arrival
// height, instead of the whole int64 range — the property that neutralises the
// pre-auth at-height amplification.
func TestSessionHeightsPlausible_CollapsesAttackerSpace(t *testing.T) {
	const arrival = int64(1_000_000)

	// Every plausible start height must lie within the bounded band.
	lowBound := arrival - maxSessionLookbackBlocks - maxPlausibleSessionLengthBlocks
	highBound := arrival + maxSessionLookaheadBlocks

	for _, start := range []int64{
		lowBound - 1,          // just below the band
		highBound + 1,         // just above the band
		1,                     // genesis-ish
		arrival + 100_000_000, // wildly future
	} {
		// Pair each with a minimal valid-length session; only the band membership
		// should decide the outcome for the far-out ones.
		require.False(t, sessionHeightsPlausible(start, start+10, arrival),
			"start=%d outside the plausible band must be rejected", start)
	}
}

// validRelayRequest builds a relay request that passes RelayRequest.ValidateBasic
// (valid bech32 application address, non-empty session ID / signature / supplier),
// so a test can reach the checks that run AFTER basic validation.
func validRelayRequest(sessionStart, sessionEnd int64) *servicetypes.RelayRequest {
	// Built from raw bytes rather than a literal so the address carries whatever
	// bech32 prefix the SDK config holds in this binary.
	appAddress := sdk.AccAddress([]byte("relayminer-test-app0")).String()

	return &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      appAddress,
				ServiceId:               "seda",
				SessionId:               "session1",
				SessionStartBlockHeight: sessionStart,
				SessionEndBlockHeight:   sessionEnd,
			},
			SupplierOperatorAddress: "pokt1supplieroperatoraddr",
			Signature:               []byte{0x01},
		},
	}
}

// TestValidateRelayRequest_RejectsImplausibleHeightsBeforeAnyChainRead pins the
// bound on the SHARED validation path.
//
// proxy.go's handleRelay applies the same bound ahead of its eager meter, but gRPC
// (relay_grpc_service.go) and WebSocket (websocket.go) never run that code — they
// reach the chain only through ValidateRelayRequest, whose
// getTargetSessionBlockHeight resolves shared params at the client's session END
// height BEFORE the ring signature is verified. Without the bound here, those two
// transports keep the pre-signature amplification surface the HTTP path closed.
func TestValidateRelayRequest_RejectsImplausibleHeightsBeforeAnyChainRead(t *testing.T) {
	const currentH = int64(1_000_000)

	v, paramCache := newEpochValidator(10, 10, currentH)
	v.SetCurrentBlockHeight(currentH)

	// Genesis-era heights: structurally valid (so ValidateBasic passes) but far
	// outside any window this relayer could serve.
	err := v.ValidateRelayRequest(context.Background(), validRelayRequest(1, 21))

	require.Error(t, err)
	require.Contains(t, err.Error(), "implausible session heights")

	heights, latestCalls := paramCache.snapshot()
	require.Empty(t, heights, "no at-height params read may be issued for an implausible header")
	require.Zero(t, latestCalls, "no live params read may be issued for an implausible header")
}

// TestValidateRelayRequest_PlausibleHeightsReachSessionTiming is the other half of
// the contract: the bound must not swallow a header the relayer would otherwise
// judge on its merits. A recently-ended session passes the bound and is then
// rejected by the real grace-period check — proving execution continued past the
// bound and consulted the chain at the session's own end height.
func TestValidateRelayRequest_PlausibleHeightsReachSessionTiming(t *testing.T) {
	const (
		currentH     = int64(1_000_000)
		sessionStart = int64(999_960)
		sessionEnd   = int64(999_970) // grace (10) ended at 999_979, well before now
	)

	v, paramCache := newEpochValidator(10, 10, currentH)
	v.SetCurrentBlockHeight(currentH)

	err := v.ValidateRelayRequest(context.Background(), validRelayRequest(sessionStart, sessionEnd))

	require.Error(t, err)
	require.NotContains(t, err.Error(), "implausible session heights",
		"a session inside the lookback window must pass the plausibility bound")
	require.Contains(t, err.Error(), "session expired",
		"it must be the grace-period check that rejects it, not the bound")

	heights, _ := paramCache.snapshot()
	require.Equal(t, []int64{sessionEnd}, heights,
		"session timing must resolve params at the session's own end height")
}
