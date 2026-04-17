package miner

// SMSTRootLen is the expected length in bytes of a serialized SparseMerkleSumTrie
// root: 32-byte Merkle hash || 8-byte big-endian count || 8-byte big-endian sum.
//
// Any value read from Redis (claimed root, live-root checkpoint, legacy migration
// scan, loaded session snapshot, chain-bound claim payload) that does not match
// this length is treated as corrupt and discarded rather than passed to the smt
// library. Passing a shorter byte slice to smt.ImportSparseMerkleSumTrie can
// panic inside the library with `slice bounds out of range` when it attempts
// to split the payload into hash/count/sum segments.
const SMSTRootLen = 48

// isValidSMSTRoot returns true if b is exactly SMSTRootLen bytes. It does NOT
// validate the cryptographic contents — only the serialized shape — because
// that is what prevents a downstream slice panic.
func isValidSMSTRoot(b []byte) bool {
	return len(b) == SMSTRootLen
}
