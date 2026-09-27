//go:build test

package observability

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/stats"
)

// TestQueueIsMeasuredFromBeginToOutHeaderOnly pins WHICH pair of events the
// delay is measured between, because that choice is the whole measurement and
// nothing about it is visible from the outside.
//
// stats.Begin carries BeginTime and is emitted BEFORE the transport is asked
// for a stream. The client stats.OutHeader is emitted inside NewStream AFTER
// the stream-quota loop. Only that pair brackets the wait.
//
// stats.InHeader is the tempting neighbour and it is WRONG: it arrives with the
// server's response, so it measures Begin-to-first-response-byte -- the whole
// round trip including the server's own handling time. A test that only counts
// observations cannot tell the two apart, because both produce one per RPC.
// This one can: the wrong event must produce NOTHING.
func TestQueueIsMeasuredFromBeginToOutHeaderOnly(t *testing.T) {
	const conn = "contract-test"
	h := NewGRPCStreamQueueStats(conn)

	begin := &stats.Begin{Client: true, BeginTime: time.Now()}

	t.Run("OutHeader records", func(t *testing.T) {
		before := sampleCount(t, conn)
		ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{})
		h.HandleRPC(ctx, begin)
		h.HandleRPC(ctx, &stats.OutHeader{Client: true})
		require.Equal(t, before+1, sampleCount(t, conn),
			"Begin then OutHeader is the pair that brackets the stream-quota wait")
	})

	t.Run("InHeader does not record", func(t *testing.T) {
		before := sampleCount(t, conn)
		ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{})
		h.HandleRPC(ctx, begin)
		h.HandleRPC(ctx, &stats.InHeader{Client: true})
		require.Equal(t, before, sampleCount(t, conn),
			"InHeader arrives with the server's response: recording on it would "+
				"measure the whole round trip and call it queueing")
	})

	t.Run("server-side stats are ignored", func(t *testing.T) {
		before := sampleCount(t, conn)
		ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{})
		h.HandleRPC(ctx, &stats.Begin{Client: false, BeginTime: time.Now()})
		h.HandleRPC(ctx, &stats.OutHeader{Client: false})
		require.Equal(t, before, sampleCount(t, conn),
			"this process is a gRPC client; a server-side event here would be a "+
				"different quantity sharing a series")
	})

	t.Run("OutHeader without a Begin records nothing", func(t *testing.T) {
		before := sampleCount(t, conn)
		ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{})
		h.HandleRPC(ctx, &stats.OutHeader{Client: true})
		require.Equal(t, before, sampleCount(t, conn),
			"with no start there is no delta: observing time.Since(zero) would "+
				"put a 55-year sample in the histogram and poison every quantile")
	})
}

func sampleCount(t *testing.T, conn string) uint64 {
	t.Helper()
	families, err := SharedRegistry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "ha_grpc_stream_queue_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "conn" && l.GetValue() == conn {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}
