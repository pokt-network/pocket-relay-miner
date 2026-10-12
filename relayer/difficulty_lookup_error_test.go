//go:build test

package relayer

import (
	"context"
	"errors"
	"testing"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

var errDifficultyLookup = errors.New("difficulty lookup failed")

// failingDifficulty is a provider whose lookup fails.
type failingDifficulty struct{}

func (failingDifficulty) GetTargetHash(context.Context, string, int64) ([]byte, error) {
	return nil, errDifficultyLookup
}

// failingDifficultyQuery is a chain query client whose difficulty query fails.
type failingDifficultyQuery struct{}

func (failingDifficultyQuery) GetServiceRelayDifficulty(context.Context, string, int64) ([]byte, error) {
	return nil, errDifficultyLookup
}

// A relay whose mining difficulty could not be resolved is served but not
// mined. Mined against the base target instead, a relay below the real target
// could be the leaf the chain picks for the proof, which the chain validates
// against the difficulty at the session start: the whole session's proof
// would fail. One relay unmined is the smaller loss.
func TestProcessRelay_ARelayWhoseDifficultyLookupFailsIsServedNotMined(t *testing.T) {
	rp := NewRelayProcessor(testLogger(), nil, nil, nil)
	rp.SetDifficultyProvider(failingDifficulty{})
	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1lookupapp",
				ServiceId:               "svc-lookup-error",
				SessionId:               "lookup-session",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   110,
			},
			SupplierOperatorAddress: "pokt1lookupsupplier",
		},
		Payload: []byte("payload"),
	}
	reqBz, err := req.Marshal()
	require.NoError(t, err)

	failures := difficultyQueryFailures.WithLabelValues("svc-lookup-error")
	skipped := relaysSkippedDifficulty.WithLabelValues("svc-lookup-error", BackendTypeJSONRPC)
	failuresBefore, skippedBefore := testutil.ToFloat64(failures), testutil.ToFloat64(skipped)

	msg, err := rp.ProcessRelay(WithRPCType(context.Background(), BackendTypeJSONRPC), reqBz, []byte(`{"ok":true}`), "pokt1lookupsupplier", "svc-lookup-error", 105)

	require.NoError(t, err, "the relay is served: its response is not refused")
	require.Nil(t, msg, "the relay is not mined")
	require.Equal(t, failuresBefore+1, testutil.ToFloat64(failures), "the failed lookup is counted once")
	require.Equal(t, skippedBefore, testutil.ToFloat64(skipped), "not counted as a relay below the difficulty")
}

// The chain-backed provider returns the lookup's error instead of the base
// target, so the processor can tell an unresolved difficulty from a resolved one.
func TestQueryDifficultyProvider_ALookupErrorIsReturnedNotTheBaseTarget(t *testing.T) {
	p := NewQueryDifficultyProvider(testLogger(), failingDifficultyQuery{})
	target, err := p.GetTargetHash(context.Background(), "svc-provider-error", 100)
	require.ErrorIs(t, err, errDifficultyLookup)
	require.Nil(t, target)
}
