//go:build test

package miner

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// leafNodeBytes builds a leaf the way the smt library encodes one:
// 0x00 | path(32) | value | weight(8) | count(8).
func leafNodeBytes(path byte, value []byte) []byte {
	node := make([]byte, 0, 1+coldLeafPathLen+len(value)+coldLeafMetaLen)
	node = append(node, 0x00)
	node = append(node, bytes.Repeat([]byte{path}, coldLeafPathLen)...)
	node = append(node, value...)
	var meta [16]byte
	binary.BigEndian.PutUint64(meta[0:8], 1)
	binary.BigEndian.PutUint64(meta[8:16], 1)
	return append(node, meta[:]...)
}

// TestANodeSurvivesTheRoundTripWhateverItCompressesTo is the property the root
// depends on: the digest is taken over the value BEFORE it is stored
// (smt@v0.15.0 hasher.go:154-158), so compression is only allowed to change how
// it is stored, never what comes back.
func TestANodeSurvivesTheRoundTripWhateverItCompressesTo(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1codec_roundtrip", "sess-codec-roundtrip"
	store, ok := NewRedisMapStore(ctx, client, supplier, sessionID).(*RedisMapStore)
	require.True(t, ok)
	hashKey := client.KB().SMSTNodesKey(supplier, sessionID)

	compressible := leafNodeBytes(0x11, bytes.Repeat([]byte("relay payload "), 40))
	incompressible := leafNodeBytes(0x22, nodeSizedValue(512))

	for name, node := range map[string][]byte{"compressible": compressible, "incompressible": incompressible} {
		key := []byte(name)
		require.NoError(t, store.Set(key, node))
		got, err := store.Get(key)
		require.NoError(t, err)
		require.Equal(t, node, got, "%s: the bytes the tree hashed must be the bytes it reads back", name)
	}

	// CONTROL: without this the test above passes even if nothing is ever
	// compressed -- "it reads back" and "it was stored raw" look identical from
	// Get alone. Read what Redis holds, not what the store returns.
	storedCompressible, err := client.HGet(ctx, hashKey, hex.EncodeToString([]byte("compressible"))).Bytes()
	require.NoError(t, err)
	require.True(t, isCompressedNode(storedCompressible),
		"control: a leaf carrying a compressible payload must be stored as a frame, or this test proves nothing")
	require.Less(t, len(storedCompressible), len(compressible), "and the frame must be smaller than the node")

	// The other direction is asserted as the PROPERTY the code guarantees, not
	// as a prediction about one input: storing never grows a node. Measured
	// while writing this test -- a leaf whose value is incompressible still
	// framed, because its 32-byte path and its 16 bytes of metadata give zstd
	// enough to win by a hair. "This input stays raw" would have been a guess
	// about the encoder; "never larger" is what compressNode promises.
	storedIncompressible, err := client.HGet(ctx, hashKey, hex.EncodeToString([]byte("incompressible"))).Bytes()
	require.NoError(t, err)
	require.LessOrEqual(t, len(storedIncompressible), len(incompressible),
		"the smaller of the two wins, so storing a node never grows it")
}

// TestANodeWrittenByThePreviousBinaryIsStillRead is Jorge's acceptance
// criterion as a test: the new binary has to start against a Redis the previous
// one wrote. Those nodes are raw, and stay raw forever -- an inner node is
// hashes and never compresses, so the mixed format is permanent, not a
// migration step.
//
// It is written as a test and not as an argument about bytes on purpose: the
// compatibility rests today on smt's node prefixes (0x00/0x01/0x02) never
// colliding with zstd's magic (0x28), and nothing else asserts it. The day
// someone adds a header, this goes red.
func TestANodeWrittenByThePreviousBinaryIsStillRead(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1codec_oldbinary", "sess-codec-oldbinary"
	store, ok := NewRedisMapStore(ctx, client, supplier, sessionID).(*RedisMapStore)
	require.True(t, ok)
	hashKey := client.KB().SMSTNodesKey(supplier, sessionID)

	// What the previous binary left behind: the node, uncompressed, written
	// straight into the hash.
	old := leafNodeBytes(0x33, bytes.Repeat([]byte("old binary payload "), 20))
	field := hex.EncodeToString([]byte("legacy-node"))
	require.NoError(t, client.HSet(ctx, hashKey, field, old).Err())

	got, err := store.Get([]byte("legacy-node"))
	require.NoError(t, err)
	require.Equal(t, old, got, "a node the previous binary wrote must read back unchanged")

	var seen [][]byte
	require.NoError(t, store.RangeNodes(ctx, func(_ string, node []byte, storedBytes int) error {
		seen = append(seen, node)
		require.Equal(t, len(old), storedBytes, "an uncompressed node costs its own length in Redis")
		return nil
	}))
	require.Len(t, seen, 1)
	require.Equal(t, old, seen[0], "the cold path reads it through the store, so it reads it too")
}

// TestTheFirstByteTellsAFrameFromANode pins what the mixed format rests on.
func TestTheFirstByteTellsAFrameFromANode(t *testing.T) {
	// smt@v0.15.0 node_encoders.go:23-25: leaf 0x00, inner 0x01, extension 0x02.
	for _, prefix := range []byte{0x00, 0x01, 0x02} {
		node := append([]byte{prefix}, nodeSizedValue(64)...)
		require.False(t, isCompressedNode(node), "a node with prefix %#x is not a frame", prefix)
	}

	compressible := leafNodeBytes(0x44, bytes.Repeat([]byte("aaaa"), 200))
	framed := compressNode(compressible)
	require.True(t, isCompressedNode(framed), "a compressed node is recognisable as a frame")
	require.Less(t, len(framed), len(compressible))
	back, err := decompressNode(framed)
	require.NoError(t, err)
	require.Equal(t, compressible, back)

	// An inner node is two hashes: compressing it would grow it, so it is left
	// alone. This is why the format stays mixed forever.
	inner := append([]byte{0x01}, nodeSizedValue(64)...)
	require.Equal(t, inner, compressNode(inner), "a node that does not compress is stored as it is")
}
