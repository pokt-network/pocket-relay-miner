//go:build test

package transport

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/klauspost/compress/s2"
	"github.com/stretchr/testify/require"
)

// TestCompressRelayBytesDecidesPerRelay pins the rule on the synthetic corpus:
// nothing below the threshold is touched, and above it what compresses is
// compressed and what does not is sent raw without paying a full encode. Every
// compressed relay restores to its exact original bytes.
func TestCompressRelayBytesDecidesPerRelay(t *testing.T) {
	sizes := []int{1 << 10, RelayCompressionThreshold - 1, RelayCompressionThreshold, RelayCompressionThreshold + 1, 1 << 20}
	for _, shape := range RelayShapes {
		for _, n := range sizes {
			raw := SyntheticRelayBytes(shape, n)
			original := bytes.Clone(raw)

			compressed, outcome := CompressRelayBytes(raw)

			require.Equal(t, original, raw, "%s/%d: the input must not be modified", shape, n)
			want := CompressionOutcomeCompressed
			switch {
			case n < RelayCompressionThreshold:
				want = CompressionOutcomeBelowThreshold
			case shape == RelayShapeBinary:
				want = CompressionOutcomeProbeIncompressible
			}
			require.Equal(t, want, outcome, "%s/%d", shape, n)
			if want != CompressionOutcomeCompressed {
				require.Nil(t, compressed, "%s/%d: a relay that stays raw returns no compressed form", shape, n)
				continue
			}
			require.LessOrEqual(t, len(compressed), n-n/10, "%s/%d: compressed only when it saves at least 10%%", shape, n)

			msg := &MinedRelayMessage{RelayBytesS2: compressed}
			restored, err := msg.OriginalRelayBytes(0)
			require.NoError(t, err, "%s/%d", shape, n)
			require.Equal(t, original, restored, "%s/%d: the restored bytes are the original bytes", shape, n)
		}
	}
}

// TestCompressRelayBytesNotSmallerAfterAPassingProbe covers the second bar: a
// relay whose middle compresses but whose whole saves less than 10% is sent raw.
//
// The input is built so that the whole saves between 0 and 10%, and the premise
// checks it: random bytes with text only in the middle do NOT do that, because
// S2's encoder speeds past incompressible input and skips the text with it, so
// that relay comes out LARGER and could not tell the 10% bar from "any saving".
func TestCompressRelayBytesNotSmallerAfterAPassingProbe(t *testing.T) {
	const n = 256 << 10
	raw := ChainedHashBytes("not-smaller", n)
	copy(raw, SyntheticRelayBytes(RelayShapeText, 16<<10))
	middle := SyntheticRelayBytes(RelayShapeText, compressionProbeBytes)
	copy(raw[(n-len(middle))/2:], middle)
	whole := len(s2.Encode(nil, raw))
	require.True(t, whole < n && whole > n-n/10, "premise: the whole saves between 0 and 10%% (%d of %d)", whole, n)

	compressed, outcome := CompressRelayBytes(raw)

	require.Equal(t, CompressionOutcomeNotSmaller, outcome)
	require.Nil(t, compressed)
}

func TestCompressRelayBytesOverMaxTravelsRaw(t *testing.T) {
	raw := make([]byte, MaxCompressedRelayBytes+1)

	compressed, outcome := CompressRelayBytes(raw)

	require.Equal(t, CompressionOutcomeOverMax, outcome, "zeros would compress: only the size can decide this")
	require.Nil(t, compressed)
}

// TestOriginalRelayBytesRefusesWhatCannotBeTheRelay: a message must carry exactly
// one of the two fields, and a compressed field must decode. Each refusal is
// ErrRelayBytesCorrupt, so the miner drops it instead of building a leaf.
func TestOriginalRelayBytesRefusesWhatCannotBeTheRelay(t *testing.T) {
	valid := s2.Encode(nil, SyntheticRelayBytes(RelayShapeText, RelayCompressionThreshold))
	truncated := valid[:len(valid)/2]
	overMax := binary.AppendUvarint(nil, MaxCompressedRelayBytes+1)

	cases := []struct {
		name string
		msg  *MinedRelayMessage
	}{
		{"neither field", &MinedRelayMessage{}},
		{"both fields", &MinedRelayMessage{RelayBytes: []byte("raw"), RelayBytesS2: valid}},
		{"truncated block", &MinedRelayMessage{RelayBytesS2: truncated}},
		{"length prefix past the contract", &MinedRelayMessage{RelayBytesS2: append(overMax, valid...)}},
		{"not a varint", &MinedRelayMessage{RelayBytesS2: bytes.Repeat([]byte{0xff}, 11)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.msg.OriginalRelayBytes(0)
			require.ErrorIs(t, err, ErrRelayBytesCorrupt)
			require.Nil(t, got)
		})
	}
}

// TestOriginalRelayBytesReturnsRawBytesAsTheyAre: an uncompressed relay is read
// without a copy, so the path every relay below the threshold takes costs
// nothing new.
func TestOriginalRelayBytesReturnsRawBytesAsTheyAre(t *testing.T) {
	raw := []byte("relay")
	got, err := (&MinedRelayMessage{RelayBytes: raw}).OriginalRelayBytes(0)
	require.NoError(t, err)
	require.Equal(t, raw, got)
	require.Same(t, &raw[0], &got[0])
}

// TestOriginalRelayBytesCorruptPrefixAllocatesNothing: an S2 block states its
// decoded length first, and s2.Decode allocates that length before it validates
// the block, up to 4 GiB. A prefix claiming 512 MiB must be refused before that
// allocation (512 MiB and not 4 GiB so that the same test, run against a decoder
// with no bound, fails on its measurement rather than on the machine).
func TestOriginalRelayBytesCorruptPrefixAllocatesNothing(t *testing.T) {
	msg := &MinedRelayMessage{RelayBytesS2: append(binary.AppendUvarint(nil, 512<<20), 0x00, 0x01)}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	_, err := msg.OriginalRelayBytes(0)

	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, ErrRelayBytesCorrupt)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20), "a corrupt prefix must not allocate its claimed length")
}

// TestReleasedMessageForgetsItsCompressedBytes pins that the pool's reset clears
// the new field: a recycled message that kept RelayBytesS2 would hand the next
// relay's reader two fields, or the previous relay's bytes.
func TestReleasedMessageForgetsItsCompressedBytes(t *testing.T) {
	m := AcquireMinedRelayMessage()
	m.RelayBytesS2 = []byte("compressed")

	ReleaseMinedRelayMessage(m)

	require.Empty(t, m.RelayBytesS2)
}

// TestOriginalRelayBytesLeavesTheSpareTheLeafNeeds pins the contract the miner's
// memory depends on: whatever branch the bytes come from, the slice returned has
// at least spare bytes of capacity past its length, so the SMST appends the
// leaf's weight and count in place instead of copying the relay. A decode buffer
// sized exactly -- s2.Decode(nil, ...) -- is what took the miner's heap from
// 872 MiB to 2.3 GiB on 2026-09-23.
func TestOriginalRelayBytesLeavesTheSpareTheLeafNeeds(t *testing.T) {
	const spare = 16
	for _, shape := range []string{RelayShapeEVMJSON, RelayShapeCosmosJSON, RelayShapeText} {
		raw := SyntheticRelayBytes(shape, 1<<20)
		compressed, outcome := CompressRelayBytes(raw)
		require.Equal(t, CompressionOutcomeCompressed, outcome, "premise: %s compresses", shape)

		got, err := (&MinedRelayMessage{RelayBytesS2: compressed}).OriginalRelayBytes(spare)

		require.NoError(t, err)
		require.Equal(t, raw, got, shape)
		require.GreaterOrEqual(t, cap(got)-len(got), spare, "%s: decompressed with room for the leaf suffix", shape)
	}

	// Raw with room already, as Unmarshal leaves it almost always: returned as
	// it is, no copy.
	roomy := append([]byte(nil), ChainedHashBytes("roomy", 1<<20+1)...)
	require.GreaterOrEqual(t, cap(roomy)-len(roomy), spare, "premise: a page-rounded capacity leaves room")
	got, err := (&MinedRelayMessage{RelayBytes: roomy}).OriginalRelayBytes(spare)
	require.NoError(t, err)
	require.Same(t, &roomy[0], &got[0], "a raw relay with room is not copied")

	// Raw without room: a length that is an exact multiple of the 8 KiB page
	// leaves Unmarshal's append with no spare capacity at all.
	tight := append([]byte(nil), ChainedHashBytes("tight", 1<<20)...)
	require.Less(t, cap(tight)-len(tight), spare, "premise: a page-multiple length leaves no room")
	got, err = (&MinedRelayMessage{RelayBytes: tight}).OriginalRelayBytes(spare)
	require.NoError(t, err)
	require.Equal(t, tight, got)
	require.GreaterOrEqual(t, cap(got)-len(got), spare, "copied once, with the room the leaf needs")
}
