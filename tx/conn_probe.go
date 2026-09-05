package tx

import (
	"context"
	"errors"
	"fmt"
	"time"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

const (
	// txConnProbeTimeout bounds one probe RPC.
	//
	// It is not only there so a hung probe fails loudly instead of never
	// returning. It is what keeps the connection alive at all: with
	// PermitWithoutStream false the client keepalive sleeps while no stream
	// exists and sends one ping when a stream wakes it, and the server forgives
	// exactly ONE ping per server write (setResetPingStrikes is a 0/1 flag, not
	// a counter). A healthy probe produces one ping, so the accounting closes
	// 1:1 with nothing spare -- but a probe that hangs past the keepalive
	// interval produces a SECOND ping with no server write in between, which
	// strikes, and three strikes earn a GOAWAY. See transport/grpcconn.
	txConnProbeTimeout = 5 * time.Second

	// DefaultTxConnProbeInterval is how often an idle tx connection is probed.
	//
	// It is deliberately the same cadence as the keepalive's Time, because it
	// IS that cadence: each probe creates a stream, wakes the dormant keepalive
	// and produces one ping. Choosing it is a judgement, not a measurement --
	// nobody here has measured how long a middlebox on an operator's path lets
	// an idle flow live. That is what idle_seconds on the failure log is for.
	DefaultTxConnProbeInterval = 60 * time.Second

	probeReasonStartup = "startup"
	probeReasonTick    = "tick"
)

// ErrTxConnMisconfigured marks a probe failure that retrying cannot fix: the
// endpoint answered, and what it answered says the connection is pointed at the
// wrong thing or refused our credentials. A caller should refuse to start on
// this, and should start anyway on anything else.
var ErrTxConnMisconfigured = errors.New("tx connection is misconfigured")

// probeConn issues the cheapest RPC that proves this connection can carry a
// request and receive an answer: auth's Params takes no arguments and reads no
// state.
//
// WHAT IT DOES NOT PROVE, which matters when reading a green probe next to a
// failing claim: it exercises cosmos.auth.v1beta1.Query, not
// cosmos.tx.v1beta1.Service. It says nothing about whether BroadcastTx works,
// nothing about the node's hop to CometBFT behind it, and nothing about the
// account lookup path, which fails on its own for a supplier with no account.
// It answers one question -- is this connection carrying bytes both ways --
// and that is the question the window cannot afford to ask for the first time
// while it is closing.
func (tc *TxClient) probeConn(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, txConnProbeTimeout)
	defer cancel()

	if _, err := tc.authQuerier.Params(probeCtx, &authtypes.QueryParamsRequest{}); err != nil {
		if isMisconfigured(err) {
			return fmt.Errorf("%w: %w", ErrTxConnMisconfigured, err)
		}
		return err
	}
	tc.markConnOK()
	return nil
}

// isMisconfigured classifies a probe failure BY CODE. Never by message text:
// the substring guard is the defect this branch of the work exists to remove.
//
// Unimplemented means the endpoint speaks gRPC but does not serve the auth
// module -- it is not a Pocket node. Unauthenticated means the credentials were
// rejected. Both survive any amount of retrying.
//
// A TLS mismatch is NOT in this set, and that is a limitation rather than a
// choice: grpc-go surfaces a failed handshake as Unavailable, the same code a
// node that is simply down returns, and the only thing separating them is the
// message text this function refuses to read. So a TLS misconfiguration is
// treated as a blip -- logged every interval, never fatal.
func isMisconfigured(err error) bool {
	switch status.Code(err) {
	case codes.Unimplemented, codes.Unauthenticated:
		return true
	default:
		return false
	}
}

// markConnOK records that this connection carried an RPC end to end just now.
func (tc *TxClient) markConnOK() {
	tc.lastConnOKUnixNano.Store(time.Now().UnixNano())
}

// idleSeconds is how long the connection had been silent, as far as this client
// can tell, when the caller asked.
//
// It is bounded from above by the probe interval, so it can prove that an idle
// flow died within that window but never that it survived longer. That is
// enough to justify LOWERING the interval on an operator's network and not
// enough to justify raising it.
func (tc *TxClient) idleSeconds() float64 {
	last := tc.lastConnOKUnixNano.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last)).Seconds()
}

// VerifyConn proves at startup that the transaction connection works, because
// grpc.NewClient is lazy: without this, a connection built against the wrong
// endpoint or the wrong credentials reports nothing until the first claim, and
// the first claim happens inside a closing window.
//
// It returns ErrTxConnMisconfigured for failures retrying cannot fix; the
// caller should refuse to start on those and start anyway on the rest, since a
// full node that is briefly down at miner startup is not a reason to stay down
// with it.
func (tc *TxClient) VerifyConn(ctx context.Context) error {
	err := tc.probeConn(ctx)
	if err == nil {
		txConnVerified.Set(1)
		tc.logger.Debug().
			Str("endpoint", tc.config.GRPCEndpoint).
			Msg("transaction connection verified")
		return nil
	}

	txConnVerified.Set(0)
	txConnProbeFailures.WithLabelValues(probeReasonStartup).Inc()
	return err
}

// startConnProbe runs the probe on a ticker so a connection that dies while
// idle is discovered BEFORE the window rather than inside it.
//
// The failure it is looking for is silent by construction: with no streams
// there are no keepalive pings, so a middlebox that drops the idle flow leaves
// both ends believing the connection is READY. That is also why asking
// grpc.ClientConn.GetState() instead would answer nothing -- it reports what
// grpc-go believes, and what grpc-go believes is the bug.
func (tc *TxClient) startConnProbe() {
	ctx, cancel := context.WithCancel(context.Background())
	tc.probeCancel = cancel
	tc.probeDone = make(chan struct{})

	probe := func(ctx context.Context) {
		defer close(tc.probeDone)

		ticker := time.NewTicker(tc.probeInterval())
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tc.probeOnce(ctx)
			}
		}
	}

	go logging.RecoverGoRoutine(tc.logger, "tx_conn_probe", probe)(ctx)
}

// probeOnce is one tick's worth of work, separated so the loop stays readable.
func (tc *TxClient) probeOnce(ctx context.Context) {
	err := tc.probeConn(ctx)
	if err == nil {
		txConnVerified.Set(1)
		return
	}

	// Shutdown cancelled the probe. This is not a connection failure, and
	// counting it would move the "the connection died silently" metric on
	// every single deploy, which is precisely when nobody can afford it to
	// be noise.
	//
	// codes.Canceled also covers a probe that reached an already-closed
	// grpc.ClientConn, which is the same situation arriving by another door:
	// the client is going away. Measured while proving this test can fail --
	// it is why a probe goroutine that outlived Close() would be SILENT, and
	// therefore why the test asserts the goroutine exited rather than that
	// the counter stopped moving.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return
	}

	txConnVerified.Set(0)
	txConnProbeFailures.WithLabelValues(probeReasonTick).Inc()

	// Warn, not Debug: this fires once per interval, not once per relay, and
	// it is what an operator reads when claims start failing.
	//
	// If this says too_many_pings, it does NOT mean the keepalive is
	// misconfigured. It means three probes in a row hit a node that is
	// REACHABLE and stuck: one that is down cannot send a GOAWAY, because it
	// is not there to send it.
	tc.logger.Warn().
		Err(err).
		Str("endpoint", tc.config.GRPCEndpoint).
		Float64("idle_seconds", tc.idleSeconds()).
		Dur("interval", tc.probeInterval()).
		Msg("transaction connection probe failed")
}

// probeInterval is the configured interval, or the default when unset.
func (tc *TxClient) probeInterval() time.Duration {
	if tc.config.ConnProbeInterval > 0 {
		return tc.config.ConnProbeInterval
	}
	return DefaultTxConnProbeInterval
}
