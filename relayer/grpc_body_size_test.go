//go:build test

package relayer

import (
	"context"
	"net"
	"testing"
	"time"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestGRPCAcceptsWhatTheConfigAllows is the defect, and it is the kind that
// leaves no trace.
//
// grpc.NewServer defaults to a 4 MiB receive limit. Nothing here set it, so a
// service configured for more was served in full over HTTP and cut off at 4 MiB
// over gRPC -- one config, two answers. And the refusal happened inside grpc-go
// before any of our code ran: no counter of ours moved, no reason was recorded,
// and from the miner's side those relays simply never existed.
//
// Both directions are asserted, because only the pair is evidence: a message
// within the configured bound must get through the transport, and one past it
// must still be refused. Checking only the first would also pass with no limit
// at all.
//
// LINK: grpc-honours-the-configured-request-bound
func TestGRPCAcceptsWhatTheConfigAllows(t *testing.T) {
	const configured = 8 << 20 // above grpc-go's 4 MiB default, deliberately

	svc := NewRelayGRPCService(testLogger(), RelayGRPCServiceConfig{
		MaxRequestBodySizeAcrossServices: configured,
	})
	require.Equal(t, int64(configured), svc.maxRequestBodySizeAcrossServices,
		"LINK grpc-honours-the-configured-request-bound: the bound must reach the service -- it used to be stored in a field nothing read")

	server := NewGRPCServerForRelayService(svc)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(lis) }()
	defer server.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(configured*2)),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	send := func(payload int) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req := &servicetypes.RelayRequest{Payload: make([]byte, payload)}
		// The REAL method path: an unknown one returns Unimplemented without ever
		// reading the message, so the transport bound would never be exercised and
		// both assertions would pass for the wrong reason. Measured -- the first
		// draft of this test did exactly that.
		return conn.Invoke(ctx, RelayServiceMethodPath, req, &servicetypes.RelayResponse{})
	}

	// 6 MiB: over grpc-go's default, under ours.
	err = send(6 << 20)
	require.Error(t, err, "the handler still refuses it for lacking a session header")
	require.Equal(t, codes.InvalidArgument, status.Code(err),
		"LINK grpc-honours-the-configured-request-bound: it must be OUR handler refusing it, which is the proof the message crossed the transport")
	require.NotEqual(t, codes.ResourceExhausted, status.Code(err),
		"LINK grpc-honours-the-configured-request-bound: a body the config allows must reach our handler; ResourceExhausted here means the transport cut it off before we ever saw it, which is what made this invisible")

	// Past the configured bound: still refused, now by our number.
	err = send(configured + (1 << 20))
	require.Equal(t, codes.ResourceExhausted, status.Code(err),
		"LINK grpc-honours-the-configured-request-bound: past the bound the transport must still refuse -- otherwise the first assertion would pass with no limit at all")
}
