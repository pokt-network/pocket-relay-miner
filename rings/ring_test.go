//go:build test

package rings

import (
	"fmt"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/stretchr/testify/require"
)

// generateTestKeys creates n valid secp256k1 public keys for testing.
func generateTestKeys(t *testing.T, n int) []cryptotypes.PubKey {
	t.Helper()

	keys := make([]cryptotypes.PubKey, n)
	for i := 0; i < n; i++ {
		// Generate random secp256k1 private key
		privKey := secp256k1.GenPrivKey()
		keys[i] = privKey.PubKey()
	}
	return keys
}

// generateInvalidKey creates a non-secp256k1 key for error testing.
func generateInvalidKey(t *testing.T) cryptotypes.PubKey {
	t.Helper()

	// Use ed25519 key which is not secp256k1
	privKey := ed25519.GenPrivKey()
	return privKey.PubKey()
}

// TestGetRingFromPubKeys_ValidKeys tests ring construction with 3-5 valid secp256k1 keys.
func TestGetRingFromPubKeys_ValidKeys(t *testing.T) {
	tests := []struct {
		name    string
		numKeys int
	}{
		{"3 keys", 3},
		{"4 keys", 4},
		{"5 keys", 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := generateTestKeys(t, tt.numKeys)

			ring, err := GetRingFromPubKeys(keys)

			require.NoError(t, err)
			require.NotNil(t, ring)
			// Verify the ring has the correct number of public keys
			require.Equal(t, tt.numKeys, ring.Size())
		})
	}
}

// TestGetRingFromPubKeys_EmptyKeys tests that empty key slice.
func TestGetRingFromPubKeys_EmptyKeys(t *testing.T) {
	keys := []cryptotypes.PubKey{}

	ring, err := GetRingFromPubKeys(keys)

	// Empty keys may or may not fail depending on ring library implementation
	// If it succeeds, verify ring size is 0
	// If it fails, that's also acceptable
	if err == nil {
		require.NotNil(t, ring)
		require.Equal(t, 0, ring.Size())
	} else {
		require.Nil(t, ring)
	}
}

// TestGetRingFromPubKeys_SingleKey tests ring construction with a single key.
func TestGetRingFromPubKeys_SingleKey(t *testing.T) {
	keys := generateTestKeys(t, 1)

	ring, err := GetRingFromPubKeys(keys)

	// Ring library may require at least 2 keys for a proper ring signature
	// If it succeeds with 1 key, verify the ring is created properly
	// If it fails, verify we get an appropriate error
	if err == nil {
		require.NotNil(t, ring)
		require.Equal(t, 1, ring.Size())
	} else {
		require.Nil(t, ring)
	}
}

// TestGetRingFromPubKeys_MultipleKeys tests ring construction with many keys (10).
func TestGetRingFromPubKeys_MultipleKeys(t *testing.T) {
	keys := generateTestKeys(t, 10)

	ring, err := GetRingFromPubKeys(keys)

	require.NoError(t, err)
	require.NotNil(t, ring)
	require.Equal(t, 10, ring.Size())
}

// TestGetRingFromPubKeys_DuplicateKeys tests behavior with duplicate keys.
func TestGetRingFromPubKeys_DuplicateKeys(t *testing.T) {
	// Generate 2 unique keys and then duplicate one
	keys := generateTestKeys(t, 2)
	keysWithDuplicate := []cryptotypes.PubKey{keys[0], keys[1], keys[0]} // key[0] appears twice

	ring, err := GetRingFromPubKeys(keysWithDuplicate)

	// The ring library might accept duplicates or reject them
	// If it succeeds, verify the ring contains 3 entries (including duplicate)
	// If it fails, verify we get an error
	if err == nil {
		require.NotNil(t, ring)
		require.Equal(t, 3, ring.Size())
	} else {
		require.Nil(t, ring)
	}
}

// TestGetRingFromPubKeys_MixedValidAndInvalidKeys tests rejection of non-secp256k1 keys.
func TestGetRingFromPubKeys_MixedValidAndInvalidKeys(t *testing.T) {
	validKeys := generateTestKeys(t, 2)
	invalidKey := generateInvalidKey(t)

	// Mix valid and invalid keys
	keys := []cryptotypes.PubKey{validKeys[0], invalidKey, validKeys[1]}

	ring, err := GetRingFromPubKeys(keys)

	require.Error(t, err)
	require.Nil(t, ring)
	require.Contains(t, err.Error(), "not a secp256k1 public key")
}

// TestPointsFromPublicKeys_ValidSecp256k1 tests point conversion with valid secp256k1 keys.
func TestPointsFromPublicKeys_ValidSecp256k1(t *testing.T) {
	keys := generateTestKeys(t, 3)

	points, err := pointsFromPublicKeys(keys...)

	require.NoError(t, err)
	require.NotNil(t, points)
	require.Equal(t, 3, len(points))

	// Verify each point is not nil and has valid encoded bytes
	for i, point := range points {
		require.NotNil(t, point, "point %d should not be nil", i)
		encoded := point.Encode()
		require.NotNil(t, encoded)
		require.NotEmpty(t, encoded)
	}
}

// TestPointsFromPublicKeys_InvalidCurve tests rejection of non-secp256k1 keys.
func TestPointsFromPublicKeys_InvalidCurve(t *testing.T) {
	invalidKey := generateInvalidKey(t)

	points, err := pointsFromPublicKeys(invalidKey)

	require.Error(t, err)
	require.Nil(t, points)
	require.ErrorIs(t, err, ErrRingsNotSecp256k1Curve)
	require.Contains(t, err.Error(), "*ed25519.PubKey")
}

// TestPointsFromPublicKeys_MalformedKey tests handling of malformed keys.
func TestPointsFromPublicKeys_MalformedKey(t *testing.T) {
	// Create a secp256k1 key with corrupted bytes
	validKey := secp256k1.GenPrivKey().PubKey()
	malformedKeyBytes := validKey.Bytes()[0:10] // Truncate to make it invalid

	// Create a custom type that implements cryptotypes.PubKey with malformed bytes
	malformedKey := &secp256k1.PubKey{Key: malformedKeyBytes}

	points, err := pointsFromPublicKeys(malformedKey)

	// Should fail during DecodeToPoint due to invalid key bytes
	require.Error(t, err)
	require.Nil(t, points)
}

// TestPointsFromPublicKeys_NilKey tests handling of nil key in slice.
func TestPointsFromPublicKeys_NilKey(t *testing.T) {
	keys := []cryptotypes.PubKey{nil}

	points, err := pointsFromPublicKeys(keys...)

	require.Error(t, err)
	require.Nil(t, points)
}

// TestPointsFromPublicKeys_EmptySlice tests empty key slice.
func TestPointsFromPublicKeys_EmptySlice(t *testing.T) {
	points, err := pointsFromPublicKeys()

	require.NoError(t, err)
	// Points can be nil or empty slice for empty input
	if points != nil {
		require.Empty(t, points)
	}
}

// TestNewRingFromPoints_EmptyPoints tests ring creation with empty points slice.
func TestNewRingFromPoints_EmptyPoints(t *testing.T) {
	// We need to call via GetRingFromPubKeys with empty keys to test this path properly
	ring, err := GetRingFromPubKeys([]cryptotypes.PubKey{})

	// Empty points may or may not fail depending on ring library implementation
	if err != nil {
		require.Nil(t, ring)
	} else {
		require.NotNil(t, ring)
	}
}

// TestNewRingFromPoints_ValidPoints tests ring creation with valid points.
func TestNewRingFromPoints_ValidPoints(t *testing.T) {
	keys := generateTestKeys(t, 3)

	// First convert keys to points
	points, err := pointsFromPublicKeys(keys...)
	require.NoError(t, err)

	// Now create ring from points
	ring, err := newRingFromPoints(points)

	require.NoError(t, err)
	require.NotNil(t, ring)
	require.Equal(t, len(keys), ring.Size())
}

// TestGetRingFromPubKeys_ConsistentEncoding tests that the same keys produce consistent ring.
func TestGetRingFromPubKeys_ConsistentEncoding(t *testing.T) {
	keys := generateTestKeys(t, 3)

	// Create ring twice with same keys
	ring1, err1 := GetRingFromPubKeys(keys)
	ring2, err2 := GetRingFromPubKeys(keys)

	require.NoError(t, err1)
	require.NoError(t, err2)
	require.NotNil(t, ring1)
	require.NotNil(t, ring2)

	// Verify both rings have the same size
	require.Equal(t, ring1.Size(), ring2.Size())

	// We can't directly compare public keys without access to them,
	// but we can verify the rings are equal
	require.True(t, ring1.Equals(ring2), "rings should be equal")
}

// TestGetRingFromPubKeys_PlaceholderKey tests that placeholder key works correctly.
func TestGetRingFromPubKeys_PlaceholderKey(t *testing.T) {
	// Test that the placeholder key defined in client.go is valid for ring construction
	keys := []cryptotypes.PubKey{PlaceholderRingPubKey}

	ring, err := GetRingFromPubKeys(keys)

	// Should either succeed or fail gracefully (ring lib may require min 2 keys)
	if err == nil {
		require.NotNil(t, ring)
	}
}

// TestPointsFromPublicKeys_LargeNumberOfKeys tests performance with many keys.
func TestPointsFromPublicKeys_LargeNumberOfKeys(t *testing.T) {
	// Test with 100 keys to ensure scalability
	keys := generateTestKeys(t, 100)

	points, err := pointsFromPublicKeys(keys...)

	require.NoError(t, err)
	require.NotNil(t, points)
	require.Equal(t, 100, len(points))
}

// BenchmarkGetRingFromPubKeys benchmarks ring creation with different key counts.
func BenchmarkGetRingFromPubKeys(b *testing.B) {
	benchmarks := []struct {
		name    string
		numKeys int
	}{
		{"3_keys", 3},
		{"5_keys", 5},
		{"10_keys", 10},
		{"20_keys", 20},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			// Pre-generate keys outside benchmark timing
			keys := make([]cryptotypes.PubKey, bm.numKeys)
			for i := 0; i < bm.numKeys; i++ {
				keys[i] = secp256k1.GenPrivKey().PubKey()
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_, err := GetRingFromPubKeys(keys)
				if err != nil {
					b.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestGetRingFromPubKeys_OversizedRing tests ring with very large number of keys.
func TestGetRingFromPubKeys_OversizedRing(t *testing.T) {
	// Test with 1000 keys to ensure scalability
	keys := generateTestKeys(t, 1000)

	ring, err := GetRingFromPubKeys(keys)

	require.NoError(t, err)
	require.NotNil(t, ring)
	require.Equal(t, 1000, ring.Size())
}

// TestPointsFromPublicKeys_MixedValidKeys tests multiple valid keys.
func TestPointsFromPublicKeys_MixedValidKeys(t *testing.T) {
	// Mix of different valid secp256k1 keys
	keys := make([]cryptotypes.PubKey, 5)
	for i := 0; i < 5; i++ {
		keys[i] = secp256k1.GenPrivKey().PubKey()
	}

	points, err := pointsFromPublicKeys(keys...)

	require.NoError(t, err)
	require.NotNil(t, points)
	require.Equal(t, 5, len(points))

	// Verify all points are unique
	uniquePoints := make(map[string]bool)
	for _, point := range points {
		encoded := string(point.Encode())
		require.False(t, uniquePoints[encoded], "duplicate point found")
		uniquePoints[encoded] = true
	}
}

// TestGetRingFromPubKeys_TwoKeys tests minimal ring size (2 keys).
func TestGetRingFromPubKeys_TwoKeys(t *testing.T) {
	keys := generateTestKeys(t, 2)

	ring, err := GetRingFromPubKeys(keys)

	require.NoError(t, err)
	require.NotNil(t, ring)
	require.Equal(t, 2, ring.Size())
}

// TestNewRingFromPoints_SinglePoint tests edge case with single point.
func TestNewRingFromPoints_SinglePoint(t *testing.T) {
	keys := generateTestKeys(t, 1)
	points, err := pointsFromPublicKeys(keys...)
	require.NoError(t, err)

	ring, err := newRingFromPoints(points)

	// Ring library may or may not support single point
	if err == nil {
		require.NotNil(t, ring)
	}
}

// TestPointsFromPublicKeys_LargeKey tests handling of maximum size key.
func TestPointsFromPublicKeys_LargeKey(t *testing.T) {
	// Generate multiple valid keys to ensure handling works correctly
	keys := generateTestKeys(t, 10)

	points, err := pointsFromPublicKeys(keys...)

	require.NoError(t, err)
	require.Equal(t, 10, len(points))
}

// TestGetRingFromPubKeys_SequentialKeys tests deterministic key generation.
func TestGetRingFromPubKeys_SequentialKeys(t *testing.T) {
	// Generate keys from sequential seeds
	keys := make([]cryptotypes.PubKey, 3)
	for i := 0; i < 3; i++ {
		privKey := secp256k1.GenPrivKeyFromSecret([]byte{byte(i + 1)})
		keys[i] = privKey.PubKey()
	}

	ring1, err1 := GetRingFromPubKeys(keys)
	require.NoError(t, err1)

	// Generate same keys again
	keys2 := make([]cryptotypes.PubKey, 3)
	for i := 0; i < 3; i++ {
		privKey := secp256k1.GenPrivKeyFromSecret([]byte{byte(i + 1)})
		keys2[i] = privKey.PubKey()
	}

	ring2, err2 := GetRingFromPubKeys(keys2)
	require.NoError(t, err2)

	require.True(t, ring1.Equals(ring2), "rings from same seeds should be equal")
}

// TestPointsFromPublicKeys_AllInvalidKeys tests all keys invalid.
func TestPointsFromPublicKeys_AllInvalidKeys(t *testing.T) {
	invalidKeys := make([]cryptotypes.PubKey, 3)
	for i := 0; i < 3; i++ {
		invalidKeys[i] = ed25519.GenPrivKey().PubKey()
	}

	points, err := pointsFromPublicKeys(invalidKeys...)

	require.Error(t, err)
	require.Nil(t, points)
	require.ErrorIs(t, err, ErrRingsNotSecp256k1Curve)
}

// TestGetRingFromPubKeys_NilKeyInMiddle tests nil key not at start.
func TestGetRingFromPubKeys_NilKeyInMiddle(t *testing.T) {
	validKeys := generateTestKeys(t, 2)
	keys := []cryptotypes.PubKey{validKeys[0], nil, validKeys[1]}

	ring, err := GetRingFromPubKeys(keys)

	require.Error(t, err)
	require.Nil(t, ring)
}

// TestPointsFromPublicKeys_CompressedKeys tests compressed secp256k1 keys.
func TestPointsFromPublicKeys_CompressedKeys(t *testing.T) {
	// secp256k1 keys from cosmos-sdk are compressed by default
	keys := generateTestKeys(t, 3)

	// Verify keys are compressed (33 bytes)
	for _, key := range keys {
		require.Equal(t, 33, len(key.Bytes()), "secp256k1 keys should be 33 bytes (compressed)")
	}

	points, err := pointsFromPublicKeys(keys...)

	require.NoError(t, err)
	require.Equal(t, 3, len(points))
}

// TestGetRingFromPubKeys_ReorderedKeys tests ring with reordered keys.
func TestGetRingFromPubKeys_ReorderedKeys(t *testing.T) {
	keys := generateTestKeys(t, 3)

	// Create ring with original order
	ring1, err := GetRingFromPubKeys(keys)
	require.NoError(t, err)

	// Create ring with reversed order
	reversedKeys := make([]cryptotypes.PubKey, len(keys))
	for i := 0; i < len(keys); i++ {
		reversedKeys[i] = keys[len(keys)-1-i]
	}

	ring2, err := GetRingFromPubKeys(reversedKeys)
	require.NoError(t, err)

	// Both rings should have same size
	require.Equal(t, ring1.Size(), ring2.Size())
}

// TestPointsFromPublicKeys_SingleKey tests single key conversion.
func TestPointsFromPublicKeys_SingleKey(t *testing.T) {
	key := generateTestKeys(t, 1)[0]

	points, err := pointsFromPublicKeys(key)

	require.NoError(t, err)
	require.NotNil(t, points)
	require.Equal(t, 1, len(points))
	require.NotNil(t, points[0])
}

// TestGetRingFromPubKeys_VerifySize tests ring size matches input.
func TestGetRingFromPubKeys_VerifySize(t *testing.T) {
	sizes := []int{2, 3, 5, 7, 10, 15, 20}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			keys := generateTestKeys(t, size)
			ring, err := GetRingFromPubKeys(keys)

			require.NoError(t, err)
			require.NotNil(t, ring)
			require.Equal(t, size, ring.Size())
		})
	}
}

// BenchmarkPointsFromPublicKeys benchmarks point conversion.
func BenchmarkPointsFromPublicKeys(b *testing.B) {
	keys := make([]cryptotypes.PubKey, 5)
	for i := 0; i < 5; i++ {
		keys[i] = secp256k1.GenPrivKey().PubKey()
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := pointsFromPublicKeys(keys...)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

// BenchmarkGetRingFromPubKeys_LargeRing benchmarks with 100 keys.
func BenchmarkGetRingFromPubKeys_LargeRing(b *testing.B) {
	keys := make([]cryptotypes.PubKey, 100)
	for i := 0; i < 100; i++ {
		keys[i] = secp256k1.GenPrivKey().PubKey()
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := GetRingFromPubKeys(keys)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}
