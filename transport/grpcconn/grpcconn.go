// Package grpcconn builds every outbound gRPC connection this process makes to
// a Pocket full node, so that the query path and the transaction path cannot
// drift apart in what they configure.
//
// Before this package there were two dial sites: query/query.go, which set
// keepalive, flow-control windows, a receive limit, backoff and the stream
// observer, and tx/tx_client.go, which set transport credentials and nothing
// else. The second one never ran in production -- the miner handed the tx
// client the query connection -- so giving the tx client its own connection
// would have STARTED using the unconfigured one on the path that carries the
// money. One constructor, called by both routes, is the fix.
package grpcconn

import (
	"crypto/tls"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// Role names what a connection carries. It is the conn label of
// ha_grpc_stream_queue_seconds, so the set is CLOSED and lives here, with the
// constructor that stamps it: a Prometheus label with an open set is a
// cardinality leak, and a label value whose meaning drifts is worse than one
// that disappears.
//
// "shared" was the value while a single connection carried both queries and
// transactions. It is deliberately absent: reusing it after the split, or
// renaming it to "query", would have kept a label whose meaning changed
// underneath whoever reads it, with nothing in the metric saying so. A series
// that stops is honest. The "before" measurement taken on "shared" is
// therefore compared against the SUM of the roles below, never against one.
type Role string

const (
	// RoleQuery is the connection serving chain queries.
	RoleQuery Role = "query"
	// RoleQueryLeader is the leader controller's own query connection. The
	// miner runs it alongside the supplier worker's, in one process and
	// mostly idle, so merging them would sum two very different queues.
	RoleQueryLeader Role = "query_leader"
	// RoleTx is the connection carrying claim and proof transactions.
	RoleTx Role = "tx"
)

// valid reports whether r is one of the roles above.
//
// The guarantee is at RUNTIME, not at compile time: Role's underlying type is
// string, so an untyped literal is assignable to it and New(target, "shared")
// compiles perfectly well. It is rejected here instead, which surfaces as a
// startup failure -- loud, and before any traffic.
//
// This is also why a linter cannot own the closed set, and why the AST check
// written for it was dropped: the role can arrive from configuration, and a
// value that only exists at runtime is not something source analysis can see.
func (r Role) valid() bool {
	switch r {
	case RoleQuery, RoleQueryLeader, RoleTx:
		return true
	default:
		return false
	}
}

// Target is where to dial and how. It is built ONCE per process and handed to
// every constructor that needs a connection: deriving `UseTLS: !GRPCInsecure`
// separately at each call site is how two connections to the same node end up
// disagreeing about TLS.
type Target struct {
	// Endpoint is the node's gRPC address (host:port).
	Endpoint string
	// UseTLS selects TLS 1.2+ credentials instead of insecure ones.
	UseTLS bool
}

// New dials Endpoint and returns a connection configured for high-volume node
// traffic, labelled by role.
//
// An unknown role is refused rather than passed through: the check is here and
// not in a linter because the role can arrive from configuration, and a lint
// cannot see a value that only exists at runtime.
func New(target Target, role Role) (*grpc.ClientConn, error) {
	if target.Endpoint == "" {
		return nil, fmt.Errorf("grpcconn: endpoint is required")
	}
	if !role.valid() {
		return nil, fmt.Errorf("grpcconn: unknown connection role %q", string(role))
	}

	var transportCreds credentials.TransportCredentials
	if target.UseTLS {
		transportCreds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		transportCreds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(
		target.Endpoint,
		grpc.WithTransportCredentials(transportCreds),

		// Measure how long an RPC waits for an HTTP/2 stream. The client caps
		// at 100 concurrent streams and, past that, PARKS the caller instead
		// of failing -- see observability.NewGRPCStreamQueueStats for why the
		// server never raises that cap.
		grpc.WithStatsHandler(observability.NewGRPCStreamQueueStats(string(role))),

		// Keepalive. WHY 60s IS SAFE against a server that enforces a 5-minute
		// minimum between pings (grpc-go's defaultKeepalivePolicyMinTime, which
		// cosmos-sdk does not override): it is NOT the number, and it is not
		// that idle connections stay quiet.
		//
		// Two mechanisms hold it up. With PermitWithoutStream false the client
		// keepalive sleeps on kpDormancyCond while no stream exists
		// (http2_client.go), so it pings only once a stream wakes it. And the
		// server forgives any ping preceded by a write since the last one:
		// setResetPingStrikes is wired onWrite for headers and trailers and
		// onEachWrite for data, and the CAS in http2_server.go zeroes the
		// strike counter and returns before the interval is ever checked.
		//
		// That flag is 0/1, not a count: it forgives ONE ping. A healthy RPC
		// produces one ping, so the accounting closes 1:1 with nothing spare.
		// An RPC that hangs past kp.Time produces a SECOND ping with no server
		// write in between, and three of those earn a GOAWAY with
		// too_many_pings. That is why every caller on this connection needs a
		// deadline, and why raising PermitWithoutStream here would be fatal
		// rather than merely chattier: an idle client pinging every 60s strikes
		// on every ping and the connection dies on the third.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                60 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: false,
		}),

		// Flow control: 1MB windows (default 64KB) for large query responses.
		grpc.WithInitialWindowSize(1<<20),
		grpc.WithInitialConnWindowSize(1<<20),

		// 10MB receive limit for bulk queries. The send limit is left at
		// grpc-go's default of math.MaxInt32, so a batched claim tx has no
		// client-side ceiling.
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(10*1024*1024),
		),

		// Graceful reconnection on network issues.
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  1.0 * time.Second,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   30 * time.Second,
			},
			MinConnectTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpcconn: failed to create connection to %s: %w", target.Endpoint, err)
	}

	return conn, nil
}
