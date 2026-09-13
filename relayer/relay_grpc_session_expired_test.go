//go:build test

package relayer

import (
	"context"
	"fmt"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

// sessionExpiredValidator always rejects with ErrSessionExpired, wrapped the
// way getTargetSessionBlockHeight wraps it in production (validator.go:340-344,
// ValidateRelayRequest:149).
type sessionExpiredValidator struct{}

func (sessionExpiredValidator) ValidateRelayRequest(context.Context, *servicetypes.RelayRequest) error {
	return fmt.Errorf("session timing validation failed: %w", ErrSessionExpired)
}
func (sessionExpiredValidator) GetCurrentBlockHeight() int64 { return 100 }
func (sessionExpiredValidator) SetCurrentBlockHeight(int64)  {}

// newSessionExpiredGRPCFixture wires a RelayGRPCService whose pipeline ALWAYS
// rejects at ValidateRelay with ErrSessionExpired. No meter, no processor, no
// publisher call is ever reached -- the handler must return before any of
// them, so a nil RelayMeter is safe here (MeterRelay is never called).
func newSessionExpiredGRPCFixture(t *testing.T, ctx context.Context) (*RelayGRPCService, *mockServerStream) {
	t.Helper()

	const supplier = "pokt1testsupplieroperator"
	const serviceID = "develop-http"

	endpoint, err := pool.NewBackendEndpoint("backend", "http://127.0.0.1:1")
	require.NoError(t, err)
	healthyPool := pool.NewPool(
		"test-pool",
		[]*pool.BackendEndpoint{endpoint},
		&pool.FirstHealthySelector{},
		"first_healthy(test)",
	)

	pipeline := NewRelayPipeline(sessionExpiredValidator{}, nil, testLogger())

	keys := map[string]cryptotypes.PrivKey{supplier: secp256k1.GenPrivKey()}
	rs, err := NewResponseSigner(testLogger(), keys)
	require.NoError(t, err)

	svc := NewRelayGRPCService(testLogger(), RelayGRPCServiceConfig{
		ServiceConfigs: map[string]ServiceConfig{serviceID: {}},
		ResponseSigner: rs,
		Publisher:      &recordingPublisher{},
		RelayProcessor: &recordingProcessor{},
		RelayPipeline:  pipeline,
		GetPool: func(string, string) *pool.Pool {
			return healthyPool
		},
	})

	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      ownerTestAppAddr,
				ServiceId:               serviceID,
				SessionId:               "test-session-id",
				SessionStartBlockHeight: 91,
				SessionEndBlockHeight:   100,
			},
			SupplierOperatorAddress: supplier,
		},
	}

	streamCtx := metadata.NewIncomingContext(ctx, metadata.Pairs("rpc-type", "3"))
	return svc, &mockServerStream{ctx: streamCtx, req: relayRequest}
}

// TestHandleSendRelay_SessionExpiredDispatchesReason pins the gRPC dispatch:
// an expired-session validation failure must move relays_rejected_total under
// "session_expired", not the generic "validation_failed".
func TestHandleSendRelay_SessionExpiredDispatchesReason(t *testing.T) {
	svc, stream := newSessionExpiredGRPCFixture(t, context.Background())

	expired := relaysRejected.WithLabelValues("develop-http", BackendTypeGRPC, rejectReasonSessionExpired)
	genericBefore := testutil.ToFloat64(relaysRejected.WithLabelValues("develop-http", BackendTypeGRPC, rejectReasonValidationFailed))
	expiredBefore := testutil.ToFloat64(expired)

	err := svc.handleSendRelay(stream)
	require.Error(t, err, "an expired session must not be served")

	require.Equal(t, expiredBefore+1, testutil.ToFloat64(expired),
		"session_expired must move by exactly one")
	require.Equal(t, genericBefore, testutil.ToFloat64(relaysRejected.WithLabelValues("develop-http", BackendTypeGRPC, rejectReasonValidationFailed)),
		"the generic validation_failed reason must NOT move for this rejection")
}

// TestHandleSendRelay_DisconnectedClientWinsOverSessionExpired pins the ORDER:
// a client that already left must be counted as a disconnect, even when the
// validation failure underneath it is ALSO an expired session. The
// stream.Context() check runs first.
func TestHandleSendRelay_DisconnectedClientWinsOverSessionExpired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone before the handler runs

	svc, stream := newSessionExpiredGRPCFixture(t, ctx)

	disconnected := relaysRejected.WithLabelValues("develop-http", BackendTypeGRPC, rejectReasonClientDisconnected)
	expired := relaysRejected.WithLabelValues("develop-http", BackendTypeGRPC, rejectReasonSessionExpired)
	disconnectedBefore := testutil.ToFloat64(disconnected)
	expiredBefore := testutil.ToFloat64(expired)

	err := svc.handleSendRelay(stream)
	require.Error(t, err)

	require.Equal(t, disconnectedBefore+1, testutil.ToFloat64(disconnected),
		"a cancelled stream context must win as client_disconnected")
	require.Equal(t, expiredBefore, testutil.ToFloat64(expired),
		"session_expired must NOT be counted when the client already disconnected")
}
