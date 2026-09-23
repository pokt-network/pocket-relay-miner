//go:build test

package redis

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestCompressedRelayRoundTripsThroughRedis is the round trip the miner depends
// on, end to end through a real Redis: Publish, the dispatcher's XADD, the stream
// entry as the consumer reads it, Unmarshal, and OriginalRelayBytes. The bytes that
// come back must be the bytes that went in, byte for byte, at every size either
// side of the threshold and for every shape of the corpus; and a relay that went
// compressed must have carried NO RelayBytes, so a reader that skips
// OriginalRelayBytes finds nothing rather than the compressed form.
func TestCompressedRelayRoundTripsThroughRedis(t *testing.T) {
	p, client, prefix := newBatcher(t, time.Hour)
	ctx := context.Background()
	const supplier = "pokt1roundtrip"
	sizes := []int{
		1 << 10, transport.RelayCompressionThreshold - 1, transport.RelayCompressionThreshold,
		transport.RelayCompressionThreshold + 1, 1 << 20,
	}

	var sent [][]byte
	for _, shape := range transport.RelayShapes {
		for _, n := range sizes {
			raw := transport.SyntheticRelayBytes(shape, n)
			msg := mined(supplier, "s-roundtrip", 0)
			msg.RelayBytes = raw
			require.NoError(t, p.Publish(ctx, msg))
			require.Equal(t, raw, msg.RelayBytes, "%s/%d: Publish must leave the caller's bytes alone", shape, n)
			sent = append(sent, bytes.Clone(raw))
		}
	}
	p.dispatchAll(ctx)

	entries, err := client.XRange(ctx, transport.SupplierStreamName(prefix, supplier), "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, len(sent))
	compressed := 0
	for i, e := range entries {
		data, ok := e.Values["data"].(string)
		require.True(t, ok)
		var got transport.MinedRelayMessage
		require.NoError(t, got.Unmarshal([]byte(data)))

		restored, err := got.OriginalRelayBytes()
		require.NoError(t, err, "entry %d", i)
		require.Equal(t, sent[i], restored, "entry %d: the bytes out of Redis are the bytes that went in", i)

		if len(got.RelayBytesS2) > 0 {
			compressed++
			require.GreaterOrEqual(t, len(sent[i]), transport.RelayCompressionThreshold,
				"entry %d: a relay below the threshold must travel raw", i)
			require.Empty(t, got.RelayBytes, "entry %d: a compressed relay carries no raw bytes", i)
		}
	}
	// Three sizes at or above the threshold, for the three shapes that compress.
	require.Equal(t, 3*3, compressed)
}

// TestRelayCompressionIsCounted pins the two metrics an operator reads to see
// what compression is doing: every relay is counted with its outcome, and the
// bytes before and after are counted only for the relays actually compressed.
func TestRelayCompressionIsCounted(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)
	ctx := context.Background()
	const svc = "svc-compression-metrics"
	outcome := func(o string) float64 { return testutil.ToFloat64(relayCompressionTotal.WithLabelValues(svc, o)) }
	stage := func(s string) float64 { return testutil.ToFloat64(relayCompressionBytes.WithLabelValues(svc, s)) }
	below, incompressible, done := outcome(transport.CompressionOutcomeBelowThreshold),
		outcome(transport.CompressionOutcomeProbeIncompressible), outcome(transport.CompressionOutcomeCompressed)
	in, out := stage("in"), stage("out")

	text := transport.SyntheticRelayBytes(transport.RelayShapeText, 1<<20)
	for i, raw := range [][]byte{
		transport.SyntheticRelayBytes(transport.RelayShapeText, 1<<10),
		transport.SyntheticRelayBytes(transport.RelayShapeBinary, 1<<20),
		text,
	} {
		msg := mined("pokt1metrics", "s1", i)
		msg.ServiceId = svc
		msg.RelayBytes = raw
		require.NoError(t, p.Publish(ctx, msg))
	}

	require.Equal(t, below+1, outcome(transport.CompressionOutcomeBelowThreshold))
	require.Equal(t, incompressible+1, outcome(transport.CompressionOutcomeProbeIncompressible))
	require.Equal(t, done+1, outcome(transport.CompressionOutcomeCompressed))
	require.Equal(t, in+float64(len(text)), stage("in"), "only the compressed relay's bytes count")
	added := stage("out") - out
	require.Positive(t, added)
	require.Less(t, added, float64(len(text))*0.9)
}

// TestQueuedBytesCountsTheCompressedEntry: the queue's admission gate compares
// what the queue HOLDS, and a compressed relay holds its compressed form. A
// count taken before compression would close admission for memory the queue
// does not use.
func TestQueuedBytesCountsTheCompressedEntry(t *testing.T) {
	p, _, _ := newBatcher(t, time.Hour)
	msg := mined("pokt1queued", "s1", 0)
	msg.RelayBytes = transport.SyntheticRelayBytes(transport.RelayShapeText, 1<<20)

	require.NoError(t, p.Publish(context.Background(), msg))

	require.Less(t, p.QueuedBytes(), 1<<19, "a 1 MiB text relay compresses to about a third")
}

// TestChannelBytesCountsACompressedRelay: a compressed relay waits in the
// delivery channel as RelayBytesS2, with RelayBytes empty. The channel gauge
// must count what it holds, and give it all back when the miner takes it.
func TestChannelBytesCountsACompressedRelay(t *testing.T) {
	const supplier = "pokt1channelbytes"
	gauge := consumerChannelBytes.WithLabelValues(supplier)
	before := testutil.ToFloat64(gauge)
	raw := transport.SyntheticRelayBytes(transport.RelayShapeText, 1<<20)
	compressed, outcome := transport.CompressRelayBytes(raw)
	require.Equal(t, transport.CompressionOutcomeCompressed, outcome, "premise")
	msg := transport.StreamMessage{Message: &transport.MinedRelayMessage{
		SupplierOperatorAddress: supplier, RelayBytesS2: compressed}}

	trackChannelSend(msg)
	require.Equal(t, before+float64(len(compressed)), testutil.ToFloat64(gauge),
		"a compressed relay in the channel holds its compressed bytes")

	MarkDelivered(msg)
	require.Equal(t, before, testutil.ToFloat64(gauge), "taken from the channel, it holds nothing")
}
