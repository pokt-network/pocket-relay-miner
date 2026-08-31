//go:build test

package relayer

import (
	"context"
	"testing"

	servicev1 "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// newMeterPolicyPipeline wires a RelayPipeline around the meter from
// newFailClosedMeter. Only the meter matters here: MeterRelay does not validate.
func newMeterPolicyPipeline(t *testing.T, appAddrTheClientKnows string) (*RelayPipeline, func()) {
	t.Helper()
	meter, breakStore := newFailClosedMeter(t, appAddrTheClientKnows)
	return NewRelayPipeline(nil, meter, logging.NewLoggerFromConfig(logging.DefaultConfig())), breakStore
}

func meterPolicyRelayCtx(sessionID, appAddr string) *RelayContext {
	return &RelayContext{
		Request: &servicev1.RelayRequest{
			Meta: servicev1.RelayRequestMetadata{
				SessionHeader: &sessiontypes.SessionHeader{
					SessionId:               sessionID,
					ApplicationAddress:      appAddr,
					ServiceId:               "svc",
					SessionStartBlockHeight: 91,
					SessionEndBlockHeight:   95,
				},
			},
		},
		ServiceID:          "svc",
		SupplierAddress:    "pokt1supplier",
		SessionID:          sessionID,
		ArrivalBlockHeight: 100,
	}
}

// TestMeterRelayKeepsServingWhenOnlyTheChainBlinked is the regression test for a
// defect that made the store-vs-chain rule dead on two of five transports.
//
// WebSocket (websocket.go) and gRPC (relay_grpc_service.go) reach the meter ONLY
// through this method, and they decide with `meterErr != nil && allowed`. This
// method used to answer `return false, err` on every failure, so that condition
// could never hold: a chain query blinking closed every WebSocket connection --
// dropping all of its subscriptions -- and failed every gRPC relay, which is the
// fleet-wide outage the asymmetry exists to prevent. HTTP never showed it,
// because proxy.go calls CheckAndConsumeRelay directly.
func TestMeterRelayKeepsServingWhenOnlyTheChainBlinked(t *testing.T) {
	// The app client knows a DIFFERENT address, so the stake query fails while
	// the store stays readable.
	p, _ := newMeterPolicyPipeline(t, "pokt1someone_else")

	allowed, err := p.MeterRelay(context.Background(), meterPolicyRelayCtx("sess-1", "pokt1app"))

	require.Error(t, err, "the failure is still reported, not swallowed")
	require.True(t, allowed,
		"a chain blip must stay servable through the pipeline: flattening it to false "+
			"is what closed every WebSocket and failed every gRPC relay")
	require.NotErrorIs(t, err, ErrMeterStoreUnavailable,
		"nothing may mark a chain failure as a store failure")
}

// TestMeterRelayRefusesWhenTheStoreIsUnreadable is the other half: the pipeline
// must not soften a refusal either. Without this, propagating `allowed` could be
// "fixed" by always returning true.
func TestMeterRelayRefusesWhenTheStoreIsUnreadable(t *testing.T) {
	const appAddr = "pokt1app"
	p, breakStore := newMeterPolicyPipeline(t, appAddr)

	allowed, err := p.MeterRelay(context.Background(), meterPolicyRelayCtx("sess-1", appAddr))
	require.NoError(t, err)
	require.True(t, allowed, "precondition: a healthy meter admits the relay")

	breakStore()

	allowed, err = p.MeterRelay(context.Background(), meterPolicyRelayCtx("sess-2", appAddr))
	require.Error(t, err)
	require.False(t, allowed,
		"the consumed counter is unreadable, so admission cannot know the budget")
	require.ErrorIs(t, err, ErrMeterStoreUnavailable,
		"the marking must survive the pipeline's error wrapping")
}
