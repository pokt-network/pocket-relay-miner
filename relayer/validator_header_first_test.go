//go:build test

package relayer

import (
	"context"
	"sync/atomic"
	"testing"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/pokt-network/ring-go"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// countingRingClient counts signature verifications and accepts every one.
type countingRingClient struct{ verified atomic.Int64 }

func (*countingRingClient) GetRingForAddressAtHeight(context.Context, string, int64) (*ring.Ring, error) {
	return nil, nil
}

func (c *countingRingClient) VerifyRelayRequestSignature(context.Context, *servicetypes.RelayRequest) error {
	c.verified.Add(1)
	return nil
}

// fixedSessionCache answers every lookup with one session.
type fixedSessionCache struct{ session *sessiontypes.Session }

func (c fixedSessionCache) GetSession(context.Context, string, string, int64) (*sessiontypes.Session, error) {
	return c.session, nil
}
func (fixedSessionCache) Start(context.Context) error { return nil }
func (fixedSessionCache) Close() error                { return nil }

// The ring is looked up at the request header's session end height, so the
// header is checked against the onchain session before the signature is.
func TestValidateRelayRequest_ChecksTheHeaderBeforeTheSignature(t *testing.T) {
	const sessionStart, sessionEnd = int64(91), int64(100)
	onchain := validRelayRequest(sessionStart, sessionEnd).Meta.SessionHeader
	session := &sessiontypes.Session{
		Header:    onchain,
		SessionId: onchain.SessionId,
		Suppliers: []*sharedtypes.Supplier{{OperatorAddress: "pokt1supplieroperatoraddr"}},
	}
	params := &sharedtypes.Params{NumBlocksPerSession: 10, GracePeriodEndOffsetBlocks: 1}

	newValidator := func(rc *countingRingClient) RelayValidator {
		return NewRelayValidator(
			logging.NewLoggerFromConfig(logging.DefaultConfig()),
			&ValidatorConfig{},
			rc,
			fixedSessionCache{session: session},
			&epochSharedParamCache{oldParams: params, newParams: params, epochBoundary: sessionEnd},
			nil,
		)
	}

	rc := &countingRingClient{}
	require.NoError(t, newValidator(rc).ValidateRelayRequest(context.Background(), validRelayRequest(sessionStart, sessionEnd), sessionEnd))
	require.Equal(t, int64(1), rc.verified.Load(), "control: a matching header reaches the signature check")

	rc = &countingRingClient{}
	mismatched := validRelayRequest(sessionStart, sessionEnd)
	mismatched.Meta.SessionHeader.SessionEndBlockHeight = sessionEnd + 10
	err := newValidator(rc).ValidateRelayRequest(context.Background(), mismatched, sessionEnd)
	require.ErrorContains(t, err, "session end height mismatch")
	require.Zero(t, rc.verified.Load(), "the signature is not checked for a header that is not the onchain session's")
}
