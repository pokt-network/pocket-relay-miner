package observability

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/stats"
)

// grpcStreamQueueSeconds measures how long an outgoing RPC waits before its
// HTTP/2 stream is created.
//
// WHY IT EXISTS. grpc-go caps a client transport at defaultMaxStreamsClient
// (100, internal/transport/defaults.go) and RAISES that only when the server
// sends SETTINGS_MAX_CONCURRENT_STREAMS. The grpc-go server sends that setting
// only when its limit is NOT math.MaxUint32 (internal/transport/http2_server.go),
// and both grpc-go's default and cosmos-sdk's server (server/grpc/server.go)
// leave it at exactly that -- so the server says "unlimited" by saying nothing,
// and the client stays at 100 forever.
//
// Past that ceiling the caller does not get an error: http2_client.go's
// checkForStreamQuota parks it on streamsQuotaAvailable until a stream frees
// up. No error, no log, nothing exported. It surfaces later as a transport
// failure that carries none of the cause -- which is why this is the only way
// to ask "did we ever hit the ceiling".
//
// WHAT THE NUMBER IS, exactly. stats.Begin is emitted with BeginTime BEFORE the
// transport is asked for a stream (stream.go, newAttemptLocked), and the client
// stats.OutHeader is emitted inside NewStream AFTER the stream-quota loop
// (http2_client.go). The delta therefore CONTAINS the queueing.
//
// It contains a little more than the queueing -- header-list-size checks, and
// on the first RPC the connection handshake -- so it is an UPPER BOUND, and it
// is dominated by the wait exactly when the wait is the thing that matters.
// Read the tail, not the median.
var grpcStreamQueueSeconds = SharedFactory.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "ha",
		Subsystem: "grpc",
		Name:      "stream_queue_seconds",
		Help:      "Time from RPC start to HTTP/2 stream creation, which contains the stream-quota wait",
		// From 100us (a healthy local RPC never queues) out to ~50s, because a
		// blocked caller waits for a stream to free rather than failing.
		Buckets: prometheus.ExponentialBuckets(0.0001, 3, 12),
	},
	[]string{"conn"},
)

// queueTimerKey carries one RPC's start time from TagRPC to HandleRPC.
type queueTimerKey struct{}

// queueTimer holds the start as UnixNano in an atomic. stats.Begin and
// stats.OutHeader are emitted on the calling goroutine today, but nothing in
// the stats.Handler contract promises that, and a handler that races under
// -race is worse than no handler.
type queueTimer struct{ beginUnixNano atomic.Int64 }

// GRPCStreamQueueStats is a stats.Handler that records the stream-creation
// delay for one connection. conn is a BOUNDED label naming the connection's
// role -- "query" or "tx" -- not the endpoint, which would be unbounded.
type GRPCStreamQueueStats struct{ conn string }

// NewGRPCStreamQueueStats returns the handler for a connection with this role.
func NewGRPCStreamQueueStats(conn string) *GRPCStreamQueueStats {
	// Create the child series eagerly: a histogram with no children exports
	// nothing, and "the metric is absent" and "nothing queued" must not look
	// the same to whoever reads the dashboard.
	grpcStreamQueueSeconds.WithLabelValues(conn)
	return &GRPCStreamQueueStats{conn: conn}
}

func (h *GRPCStreamQueueStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, queueTimerKey{}, &queueTimer{})
}

func (h *GRPCStreamQueueStats) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	if !rs.IsClient() {
		return
	}
	t, ok := ctx.Value(queueTimerKey{}).(*queueTimer)
	if !ok || t == nil {
		return
	}
	switch s := rs.(type) {
	case *stats.Begin:
		t.beginUnixNano.Store(s.BeginTime.UnixNano())
	case *stats.OutHeader:
		if began := t.beginUnixNano.Load(); began != 0 {
			grpcStreamQueueSeconds.WithLabelValues(h.conn).
				Observe(time.Since(time.Unix(0, began)).Seconds())
		}
	}
}

func (h *GRPCStreamQueueStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h *GRPCStreamQueueStats) HandleConn(context.Context, stats.ConnStats) {}
