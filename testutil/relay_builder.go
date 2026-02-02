//go:build test

package testutil

import (
	"fmt"
)

// TestRelay represents a single relay for testing purposes.
// This mirrors the testRelay struct from miner/redis_smst_utils_test.go
// but is exported for use across packages.
type TestRelay struct {
	Key    []byte
	Value  []byte
	Weight uint64
}

// RelayBuilder provides a fluent API for building deterministic test relays.
// The same seed will always produce the same relay, ensuring reproducible tests.
//
// Usage:
//
//	relay := testutil.NewRelayBuilder(42).
//	    WithSessionID("session-123").
//	    WithWeight(100).
//	    Build()
type RelayBuilder struct {
	seed int

	// Configurable fields with defaults derived from seed
	sessionID *string
	payload   []byte
	weight    *uint64
	keySize   *int
	valueSize *int
}

// NewRelayBuilder creates a new RelayBuilder with the given seed.
// All fields will be deterministically generated from this seed unless overridden.
func NewRelayBuilder(seed int) *RelayBuilder {
	return &RelayBuilder{seed: seed}
}

// WithSessionID sets the session ID used to generate the relay key.
func (b *RelayBuilder) WithSessionID(id string) *RelayBuilder {
	b.sessionID = &id
	return b
}

// WithPayload sets a custom payload for the relay value.
func (b *RelayBuilder) WithPayload(payload []byte) *RelayBuilder {
	b.payload = payload
	return b
}

// WithWeight sets the weight (compute units) for the relay.
func (b *RelayBuilder) WithWeight(weight uint64) *RelayBuilder {
	b.weight = &weight
	return b
}

// WithKeySize sets the size of the generated key.
func (b *RelayBuilder) WithKeySize(size int) *RelayBuilder {
	b.keySize = &size
	return b
}

// WithValueSize sets the size of the generated value.
func (b *RelayBuilder) WithValueSize(size int) *RelayBuilder {
	b.valueSize = &size
	return b
}

// Build creates a TestRelay with the configured or default values.
// Default values are deterministically generated from the seed.
func (b *RelayBuilder) Build() TestRelay {
	// Default key size is 32 bytes (SHA-256 hash size for SMST)
	keySize := 32
	if b.keySize != nil {
		keySize = *b.keySize
	}

	// Default value size is 64 bytes
	valueSize := 64
	if b.valueSize != nil {
		valueSize = *b.valueSize
	}

	// Generate key deterministically
	var key []byte
	if b.sessionID != nil {
		// If session ID provided, incorporate it into the key
		key = []byte(fmt.Sprintf("relay_key_%s_%d", *b.sessionID, b.seed))
		// Pad or truncate to keySize
		if len(key) < keySize {
			padding := GenerateDeterministicBytes(b.seed, keySize-len(key))
			key = append(key, padding...)
		} else if len(key) > keySize {
			key = key[:keySize]
		}
	} else {
		// Pure deterministic key from seed
		key = GenerateDeterministicBytes(b.seed, keySize)
	}

	// Generate value deterministically
	var value []byte
	if b.payload != nil {
		value = b.payload
	} else {
		value = GenerateDeterministicBytes(b.seed+1000, valueSize)
	}

	// Default weight: seed * 100 + 100 (ensures non-zero)
	weight := uint64((b.seed + 1) * 100)
	if b.weight != nil {
		weight = *b.weight
	}

	return TestRelay{
		Key:    key,
		Value:  value,
		Weight: weight,
	}
}

// BuildN creates n TestRelays with incremented seeds.
// Each relay has seed = baseSeed + index.
func (b *RelayBuilder) BuildN(n int) []TestRelay {
	relays := make([]TestRelay, n)
	for i := 0; i < n; i++ {
		// Create a new builder with incremented seed
		builder := NewRelayBuilder(b.seed + i)

		// Copy over any explicit overrides
		if b.sessionID != nil {
			builder.sessionID = b.sessionID
		}
		if b.payload != nil {
			builder.payload = b.payload
		}
		if b.weight != nil {
			// For bulk builds, increment weight to ensure variety
			incrementedWeight := *b.weight + uint64(i*100)
			builder.weight = &incrementedWeight
		}
		if b.keySize != nil {
			builder.keySize = b.keySize
		}
		if b.valueSize != nil {
			builder.valueSize = b.valueSize
		}

		relays[i] = builder.Build()
	}
	return relays
}

// String returns a string representation of what would be built.
// Useful for debugging tests.
func (b *RelayBuilder) String() string {
	relay := b.Build()
	return fmt.Sprintf("TestRelay{KeyLen: %d, ValueLen: %d, Weight: %d}",
		len(relay.Key),
		len(relay.Value),
		relay.Weight,
	)
}

// GenerateTestRelays is a convenience function for generating multiple deterministic relays.
// This mirrors the pattern from miner/redis_smst_utils_test.go.
//
// Usage:
//
//	relays := testutil.GenerateTestRelays(10, 42)
func GenerateTestRelays(count int, seed int) []TestRelay {
	return NewRelayBuilder(seed).BuildN(count)
}

// GenerateKnownBitPath generates a key with a known bit path for SMST testing.
// This is useful for testing max height cases and specific tree structures.
//
// Patterns:
//   - "all_zeros": path that goes left at every branch
//   - "all_ones": path that goes right at every branch
//   - "alternating": creates predictable tree structure (0xAA pattern)
func GenerateKnownBitPath(pattern string) []byte {
	// For SHA-256, we need 32 bytes (256 bits)
	key := make([]byte, 32)

	switch pattern {
	case "all_zeros":
		// All zeros - goes left at every branch
		// Already zero-initialized
	case "all_ones":
		// All ones - goes right at every branch
		for i := range key {
			key[i] = 0xFF
		}
	case "alternating":
		// Alternating 0xAA pattern (10101010...)
		for i := range key {
			key[i] = 0xAA
		}
	case "inverse_alternating":
		// Inverse alternating 0x55 pattern (01010101...)
		for i := range key {
			key[i] = 0x55
		}
	default:
		// Unknown pattern - return zeros
		// Tests should fail explicitly if they use an unknown pattern
	}

	return key
}
