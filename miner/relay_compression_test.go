//go:build test

package miner

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// compressedTestRelays returns n distinct relays above the compression
// threshold, and each one's compressed form as the relayer would send it.
func compressedTestRelays(t *testing.T, n int) (raw, compressed [][]byte) {
	t.Helper()
	for i := 0; i < n; i++ {
		r := append(transport.SyntheticRelayBytes(transport.RelayShapeText, transport.RelayCompressionThreshold+i),
			[]byte(fmt.Sprint(i))...)
		c, outcome := transport.CompressRelayBytes(r)
		require.Equal(t, transport.CompressionOutcomeCompressed, outcome, "premise: relay %d compresses", i)
		raw = append(raw, r)
		compressed = append(compressed, c)
	}
	return raw, compressed
}

// sessionRootAndProof flushes a session's tree and proves one path in it.
func sessionRootAndProof(t *testing.T, f *handlerTestFixture, sessionID string, path []byte) (root, proof []byte) {
	t.Helper()
	root, err := f.smstMgr.FlushTree(f.ctx, sessionID)
	require.NoError(t, err)
	proof, err = f.smstMgr.ProveClosest(f.ctx, sessionID, path)
	require.NoError(t, err)
	return root, proof
}

// TestHandleRelay_CompressedRelaysBuildTheSameTreeAsRaw is what the chain sees:
// the same relays, sent compressed, give the SAME root and the same proof bytes
// as sent raw. The third session is the control that makes the equality mean
// something: the same relays with their compressed bytes inserted as the leaf
// value give a DIFFERENT root, so a miner that skipped the decompression could
// not pass the first assertion by accident.
func TestHandleRelay_CompressedRelaysBuildTheSameTreeAsRaw(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1supplier")
	raw, compressed := compressedTestRelays(t, 3)

	for i := range raw {
		hash := sha256.Sum256(raw[i])

		plain := newStreamMessage(f.supplierAddr, "sess-raw", "", 100)
		plain.Message.RelayHash, plain.Message.RelayBytes = hash[:], raw[i]
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, plain))

		s2msg := newStreamMessage(f.supplierAddr, "sess-s2", "", 100)
		s2msg.Message.RelayHash, s2msg.Message.RelayBytes, s2msg.Message.RelayBytesS2 = hash[:], nil, compressed[i]
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, s2msg))

		wrong := newStreamMessage(f.supplierAddr, "sess-compressed-leaf", "", 100)
		wrong.Message.RelayHash, wrong.Message.RelayBytes = hash[:], compressed[i]
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, wrong))
	}

	path := sha256.Sum256(raw[1])
	rawRoot, rawProof := sessionRootAndProof(t, f, "sess-raw", path[:])
	s2Root, s2Proof := sessionRootAndProof(t, f, "sess-s2", path[:])
	wrongRoot, _ := sessionRootAndProof(t, f, "sess-compressed-leaf", path[:])

	require.Equal(t, rawRoot, s2Root, "the root of compressed relays is the root of the same relays raw")
	require.Equal(t, rawProof, s2Proof, "and so is the proof")
	require.NotEqual(t, rawRoot, wrongRoot, "control: a leaf built from the compressed bytes changes the root")

	for _, sessionID := range []string{"sess-raw", "sess-s2"} {
		snap, err := f.sessionStore.Get(f.ctx, sessionID)
		require.NoError(t, err)
		require.NotNil(t, snap)
		require.Equal(t, int64(3), snap.RelayCount, sessionID)
		require.Equal(t, uint64(300), snap.TotalComputeUnits, sessionID)
	}
}

// TestHandleRelay_CompressedRelayWithEmptyHashRecomputesFromTheOriginal: the
// defensive recompute hashes the relay's bytes, so it must run on the restored
// bytes. Hashed compressed, the relay would land under a key the relayer never
// signed for and the claim would carry a leaf no proof can match.
func TestHandleRelay_CompressedRelayWithEmptyHashRecomputesFromTheOriginal(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1supplier")
	raw, compressed := compressedTestRelays(t, 1)

	msg := newStreamMessage(f.supplierAddr, "sess-s2-nohash", "", 100)
	msg.Message.RelayHash, msg.Message.RelayBytes, msg.Message.RelayBytesS2 = nil, nil, compressed[0]
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msg))

	original := sha256.Sum256(raw[0])
	ofCompressed := sha256.Sum256(compressed[0])
	isDup, err := f.dedup.IsDuplicate(f.ctx, original[:], "sess-s2-nohash")
	require.NoError(t, err)
	require.True(t, isDup, "the recomputed key is the hash of the ORIGINAL bytes")
	isDup, err = f.dedup.IsDuplicate(f.ctx, ofCompressed[:], "sess-s2-nohash")
	require.NoError(t, err)
	require.False(t, isDup, "and never the hash of the compressed ones")
}

// TestHandleRelay_UnrestorableRelayIsDroppedNotRetried: a relay whose bytes
// cannot be restored is a defect in its producer that no retry repairs. It is
// acknowledged, counted with its own reason, and never reaches the session.
func TestHandleRelay_UnrestorableRelayIsDroppedNotRetried(t *testing.T) {
	_, compressed := compressedTestRelays(t, 1)
	cases := []struct {
		name string
		raw  []byte
		s2   []byte
	}{
		{name: "neither field"},
		{name: "both fields", raw: []byte("raw"), s2: compressed[0]},
		{name: "truncated block", s2: compressed[0][:len(compressed[0])/2]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandlerTestFixture(t, "pokt1supplier")
			rejected := relaysRejected.WithLabelValues(f.supplierAddr, "relay_bytes_corrupt", "svc-1")
			before := testutil.ToFloat64(rejected)

			msg := newStreamMessage(f.supplierAddr, "sess-corrupt", "x", 100)
			msg.Message.RelayBytes, msg.Message.RelayBytesS2 = tc.raw, tc.s2

			require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msg), "acknowledged, not retried")
			require.Equal(t, before+1, testutil.ToFloat64(rejected))
			snap, err := f.sessionStore.Get(f.ctx, "sess-corrupt")
			require.NoError(t, err)
			if snap != nil {
				require.Zero(t, snap.RelayCount, "an unrestorable relay is never counted")
			}
		})
	}
}
