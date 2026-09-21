package miner

import "fmt"

// A node in the nodes hash is either a raw smt node or one zstd frame holding
// one, and WHICH of the two is readable from its first byte. That is what makes
// the mixed format permanent rather than a migration step:
//
//   - smt prefixes every node with its kind: 0x00 leaf, 0x01 inner, 0x02
//     extension (smt@v0.15.0 node_encoders.go:23-25; :39-41 asserts the three
//     prefixes are one byte each).
//   - a zstd frame starts with the magic 0x28 B5 2F FD.
//
// No uncompressed node can therefore be taken for a frame. A node this miner
// never compressed -- every inner node, because a hash does not compress and
// the smaller of the two wins -- stays readable by a reader that knows nothing
// about compression, and a new binary reads a Redis written by the previous
// version BY CONSTRUCTION, not by care. The old binary is NOT supported: it
// would hand a frame to the smt library. That is a breaking change and it is
// the operator's to plan.
//
// There is deliberately no header and no version byte. The kind prefix already
// discriminates, and a header would not help the reader that ignores it --
// which is the defect this change is about: readColdLeaves parses these bytes
// by fixed offsets and would have skipped every leaf in silence.
var zstdFrameMagic = [4]byte{0x28, 0xB5, 0x2F, 0xFD}

// compressNode returns whichever is smaller, the node or its zstd frame.
//
// Smaller and not "compressed": an inner node is two hashes and grows under
// zstd, so it is stored raw and costs one compression attempt. What pays is the
// leaf, which carries the relay bytes -- about 200-300 B of the ~1.34 KB a relay
// writes, the rest being hashes. Expect ~18% on small requests, NOT the 7x that
// only appears with large payloads.
//
// A codec that fails to build returns the node raw: every reader still reads it,
// and coldCodec's sync.Once means the failure is permanent rather than
// intermittent, so the store degrades to exactly its previous behaviour.
func compressNode(value []byte) []byte {
	if len(value) == 0 {
		return value
	}
	enc, _, err := coldCodec()
	if err != nil {
		return value
	}
	framed := enc.EncodeAll(value, nil)
	if len(framed) >= len(value) {
		return value
	}
	return framed
}

// isCompressedNode reports whether value is a zstd frame written by
// compressNode. See zstdFrameMagic for why the first bytes decide it.
func isCompressedNode(value []byte) bool {
	if len(value) < len(zstdFrameMagic) {
		return false
	}
	for i, b := range zstdFrameMagic {
		if value[i] != b {
			return false
		}
	}
	return true
}

// decompressNode returns the node a stored value holds. A value that is not a
// frame is returned as it is -- that is the old binary's node, and the inner
// nodes this one writes.
func decompressNode(value []byte) ([]byte, error) {
	if !isCompressedNode(value) {
		return value, nil
	}
	_, dec, err := coldCodec()
	if err != nil {
		return nil, fmt.Errorf("zstd codec: %w", err)
	}
	node, err := dec.DecodeAll(value, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress node: %w", err)
	}
	return node, nil
}

// DecodeStoredNode returns the node a stored value holds, for a reader outside
// this package: the `redis smst` CLI prints what a tree holds and has to show
// the size of the NODE, not of its frame. The format belongs to the store, and
// this is the one door to it — a second decoder somewhere else is how the two
// copies drift.
func DecodeStoredNode(stored []byte) ([]byte, error) { return decompressNode(stored) }
