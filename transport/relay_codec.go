package transport

import (
	"errors"
	"fmt"

	"github.com/klauspost/compress/s2"
)

// RelayCompressionThreshold is the RelayBytes size below which a relay travels
// as it is: it is neither compressed nor probed.
//
// NOT settled: the maintainer proposed 64 KiB and 16 KiB was recommended against
// the S2 cost curve. The curve measures relayer CPU, while the point of
// compressing is the memory the WAL holds in the relayer's queue, in Redis and in
// the miner's delivery channel.
const RelayCompressionThreshold = 64 << 10

// MaxCompressedRelayBytes is the largest relay the relayer compresses, and the
// largest DecodedLen the miner accepts. It is a contract between the two, not a
// size limit on relays: a larger relay travels uncompressed, as every relay did
// before compression existed.
//
// The miner needs the bound because an S2 block states its decoded length in a
// varint that s2.Decode allocates before it validates anything (s2 v1.18.6
// decode.go, Decode): one corrupt prefix would otherwise ask for up to 4 GiB.
const MaxCompressedRelayBytes = 64 << 20

// compressionProbeBytes is the window a relay's compressibility is estimated on
// before the whole relay is compressed. It is taken from the MIDDLE of the relay:
// its first bytes are the request's session header and signature, which do not
// compress whatever the payload is.
const compressionProbeBytes = 8 << 10

// Outcomes of CompressRelayBytes, used as a metric label.
const (
	CompressionOutcomeCompressed          = "compressed"
	CompressionOutcomeBelowThreshold      = "below_threshold"
	CompressionOutcomeOverMax             = "over_max"
	CompressionOutcomeProbeIncompressible = "probe_incompressible"
	CompressionOutcomeNotSmaller          = "not_smaller"
)

// ErrRelayBytesCorrupt reports a relay whose original bytes cannot be restored:
// both byte fields set, neither set, or an S2 block that does not decode within
// MaxCompressedRelayBytes.
var ErrRelayBytesCorrupt = errors.New("relay bytes corrupt")

// shrinksEnough is the 10% bar both the probe and the whole relay must clear: a
// smaller saving does not pay the decode the miner does for every such relay.
func shrinksEnough(compressed, original int) bool {
	return compressed <= original-original/10
}

// CompressRelayBytes returns raw compressed with S2 when that pays, and nil
// otherwise, with the outcome that decided it. raw is never modified.
func CompressRelayBytes(raw []byte) ([]byte, string) {
	if len(raw) < RelayCompressionThreshold {
		return nil, CompressionOutcomeBelowThreshold
	}
	if len(raw) > MaxCompressedRelayBytes {
		return nil, CompressionOutcomeOverMax
	}
	start := (len(raw) - compressionProbeBytes) / 2
	window := raw[start : start+compressionProbeBytes]
	// EstimateBlockSize writes no output, so an incompressible relay costs a scan
	// of the window and not a MaxEncodedLen allocation. It returns -1 when it
	// finds no saving at all.
	if est := s2.EstimateBlockSize(window); est < 0 || !shrinksEnough(est, len(window)) {
		return nil, CompressionOutcomeProbeIncompressible
	}
	compressed := s2.Encode(nil, raw)
	if !shrinksEnough(len(compressed), len(raw)) {
		return nil, CompressionOutcomeNotSmaller
	}
	return compressed, CompressionOutcomeCompressed
}

// OriginalRelayBytes returns the relay exactly as the relayer hashed it: RelayBytes,
// or RelayBytesS2 decompressed. It is the one way the miner reads a relay's bytes,
// so a relay that arrived compressed can never reach the hash or the SMST leaf in
// its compressed form, and one that arrived with no bytes fails here instead of
// becoming an empty leaf.
//
// The slice returned has at least spare bytes of capacity past its length. The
// miner needs that: the SMST appends the leaf's weight and count to the value it
// is given, in place when the capacity allows and otherwise by copying the whole
// relay into a slice grown by a quarter -- measured live on 2026-09-23 as a
// decode buffer with no spare capacity (s2.Decode sizes it exactly) that took the
// miner's heap from 872 MiB to 2.3 GiB. RelayBytes as Unmarshal leaves it has
// the spare almost always (its capacity is rounded up to a page) and is returned
// as it is; in the rare case it does not, it is copied once here, at the size
// the append needs, instead of by the append at a quarter more.
//
// The spare is private to the caller: the SMST leaf keeps this buffer and writes
// into it, so it must never come from, or go back to, a pool.
func (m *MinedRelayMessage) OriginalRelayBytes(spare int) ([]byte, error) {
	switch {
	case len(m.RelayBytes) > 0 && len(m.RelayBytesS2) > 0:
		return nil, fmt.Errorf("%w: both relay_bytes and relay_bytes_s2 are set", ErrRelayBytesCorrupt)
	case len(m.RelayBytes) > 0:
		if cap(m.RelayBytes)-len(m.RelayBytes) >= spare {
			return m.RelayBytes, nil
		}
		raw := make([]byte, len(m.RelayBytes), len(m.RelayBytes)+spare)
		copy(raw, m.RelayBytes)
		return raw, nil
	case len(m.RelayBytesS2) == 0:
		return nil, fmt.Errorf("%w: neither relay_bytes nor relay_bytes_s2 is set", ErrRelayBytesCorrupt)
	}
	n, err := s2.DecodedLen(m.RelayBytesS2)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelayBytesCorrupt, err)
	}
	if n > MaxCompressedRelayBytes {
		return nil, fmt.Errorf("%w: decoded length %d exceeds %d", ErrRelayBytesCorrupt, n, MaxCompressedRelayBytes)
	}
	// s2.Decode writes into dst when its capacity holds the decoded length, and
	// returns dst[:n] with dst's capacity: the spare survives.
	raw, err := s2.Decode(make([]byte, 0, n+spare), m.RelayBytesS2)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelayBytesCorrupt, err)
	}
	return raw, nil
}
