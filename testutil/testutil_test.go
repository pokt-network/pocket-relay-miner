//go:build test

package testutil

import (
	"bytes"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// TestGetTestConcurrency verifies the concurrency helper returns correct values.
func TestGetTestConcurrency(t *testing.T) {
	t.Run("default mode returns 100", func(t *testing.T) {
		// Clear any existing TEST_MODE
		os.Unsetenv("TEST_MODE")
		defer os.Unsetenv("TEST_MODE")

		result := GetTestConcurrency()
		require.Equal(t, DefaultConcurrency, result)
		require.Equal(t, 100, result)
	})

	t.Run("nightly mode returns 1000", func(t *testing.T) {
		os.Setenv("TEST_MODE", "nightly")
		defer os.Unsetenv("TEST_MODE")

		result := GetTestConcurrency()
		require.Equal(t, NightlyConcurrency, result)
		require.Equal(t, 1000, result)
	})
}

// TestGetTestIterations verifies the iterations helper returns correct values.
func TestGetTestIterations(t *testing.T) {
	t.Run("default mode returns 10", func(t *testing.T) {
		os.Unsetenv("TEST_MODE")
		defer os.Unsetenv("TEST_MODE")

		result := GetTestIterations()
		require.Equal(t, DefaultIterations, result)
		require.Equal(t, 10, result)
	})

	t.Run("nightly mode returns 100", func(t *testing.T) {
		os.Setenv("TEST_MODE", "nightly")
		defer os.Unsetenv("TEST_MODE")

		result := GetTestIterations()
		require.Equal(t, NightlyIterations, result)
		require.Equal(t, 100, result)
	})
}

// TestIsNightlyMode verifies the nightly mode detection.
func TestIsNightlyMode(t *testing.T) {
	t.Run("default is not nightly", func(t *testing.T) {
		os.Unsetenv("TEST_MODE")
		defer os.Unsetenv("TEST_MODE")

		require.False(t, IsNightlyMode())
	})

	t.Run("TEST_MODE=nightly is nightly", func(t *testing.T) {
		os.Setenv("TEST_MODE", "nightly")
		defer os.Unsetenv("TEST_MODE")

		require.True(t, IsNightlyMode())
	})

	t.Run("TEST_MODE=other is not nightly", func(t *testing.T) {
		os.Setenv("TEST_MODE", "other")
		defer os.Unsetenv("TEST_MODE")

		require.False(t, IsNightlyMode())
	})
}

// TestDeterministicBytes verifies deterministic byte generation.
func TestDeterministicBytes(t *testing.T) {
	t.Run("same seed produces identical output", func(t *testing.T) {
		seed := 42
		length := 32

		bytes1 := GenerateDeterministicBytes(seed, length)
		bytes2 := GenerateDeterministicBytes(seed, length)

		require.Equal(t, bytes1, bytes2, "same seed should produce identical bytes")
	})

	t.Run("different seeds produce different output", func(t *testing.T) {
		length := 32

		bytes1 := GenerateDeterministicBytes(42, length)
		bytes2 := GenerateDeterministicBytes(43, length)

		require.NotEqual(t, bytes1, bytes2, "different seeds should produce different bytes")
	})

	t.Run("generates correct length", func(t *testing.T) {
		for _, length := range []int{0, 1, 16, 32, 64, 128, 256} {
			bytes := GenerateDeterministicBytes(42, length)
			require.Len(t, bytes, length, "should generate %d bytes", length)
		}
	})

	t.Run("determinism across multiple calls", func(t *testing.T) {
		// Run 100 times to ensure stability
		seed := 12345
		length := 64
		reference := GenerateDeterministicBytes(seed, length)

		for i := 0; i < 100; i++ {
			result := GenerateDeterministicBytes(seed, length)
			require.Equal(t, reference, result, "iteration %d should match reference", i)
		}
	})
}

// TestDeterministicString verifies deterministic string generation.
func TestDeterministicString(t *testing.T) {
	t.Run("same seed produces identical string", func(t *testing.T) {
		str1 := GenerateDeterministicString(42, 16)
		str2 := GenerateDeterministicString(42, 16)
		require.Equal(t, str1, str2)
	})

	t.Run("generates correct length", func(t *testing.T) {
		str := GenerateDeterministicString(42, 20)
		require.Len(t, str, 20)
	})

	t.Run("only contains hex characters", func(t *testing.T) {
		str := GenerateDeterministicString(42, 100)
		for _, c := range str {
			require.Contains(t, "0123456789abcdef", string(c))
		}
	})
}

// TestDeterministicSessionID verifies deterministic session ID generation.
func TestDeterministicSessionID(t *testing.T) {
	t.Run("same seed produces identical session ID", func(t *testing.T) {
		id1 := GenerateDeterministicSessionID(42)
		id2 := GenerateDeterministicSessionID(42)
		require.Equal(t, id1, id2)
	})

	t.Run("has correct prefix", func(t *testing.T) {
		id := GenerateDeterministicSessionID(42)
		require.Contains(t, id, "session-")
	})

	t.Run("different seeds produce different IDs", func(t *testing.T) {
		id1 := GenerateDeterministicSessionID(42)
		id2 := GenerateDeterministicSessionID(43)
		require.NotEqual(t, id1, id2)
	})
}

// TestTestKeys verifies the hardcoded test keys work correctly.
func TestTestKeys(t *testing.T) {
	t.Run("supplier key is valid secp256k1", func(t *testing.T) {
		privKey := TestSupplierPrivKey()
		require.NotNil(t, privKey)
		require.Len(t, privKey.Bytes(), 32)

		pubKey := TestSupplierPubKey()
		require.NotNil(t, pubKey)

		addr := TestSupplierAddress()
		require.NotEmpty(t, addr)
		require.Contains(t, addr, "cosmos") // Default cosmos bech32 prefix
	})

	t.Run("app key is valid secp256k1", func(t *testing.T) {
		privKey := TestAppPrivKey()
		require.NotNil(t, privKey)
		require.Len(t, privKey.Bytes(), 32)

		pubKey := TestAppPubKey()
		require.NotNil(t, pubKey)

		addr := TestAppAddress()
		require.NotEmpty(t, addr)
	})

	t.Run("keys are different from each other", func(t *testing.T) {
		supplierAddr := TestSupplierAddress()
		appAddr := TestAppAddress()
		supplier2Addr := TestSupplier2Address()
		app2Addr := TestApp2Address()

		require.NotEqual(t, supplierAddr, appAddr)
		require.NotEqual(t, supplierAddr, supplier2Addr)
		require.NotEqual(t, appAddr, app2Addr)
		require.NotEqual(t, supplier2Addr, app2Addr)
	})

	t.Run("keys are deterministic", func(t *testing.T) {
		// Multiple calls should return identical keys
		addr1 := TestSupplierAddress()
		addr2 := TestSupplierAddress()
		require.Equal(t, addr1, addr2)
	})
}

// TestRelayBuilderDeterminism verifies RelayBuilder produces deterministic output.
func TestRelayBuilderDeterminism(t *testing.T) {
	t.Run("same seed produces identical relay", func(t *testing.T) {
		relay1 := NewRelayBuilder(42).Build()
		relay2 := NewRelayBuilder(42).Build()

		require.True(t, bytes.Equal(relay1.Key, relay2.Key), "keys should be identical")
		require.True(t, bytes.Equal(relay1.Value, relay2.Value), "values should be identical")
		require.Equal(t, relay1.Weight, relay2.Weight)
	})

	t.Run("different seeds produce different relays", func(t *testing.T) {
		relay1 := NewRelayBuilder(42).Build()
		relay2 := NewRelayBuilder(43).Build()

		require.False(t, bytes.Equal(relay1.Key, relay2.Key), "keys should differ")
	})

	t.Run("WithWeight override works", func(t *testing.T) {
		relay := NewRelayBuilder(42).WithWeight(999).Build()
		require.Equal(t, uint64(999), relay.Weight)
	})

	t.Run("WithSessionID affects key generation", func(t *testing.T) {
		relay1 := NewRelayBuilder(42).WithSessionID("session-a").Build()
		relay2 := NewRelayBuilder(42).WithSessionID("session-b").Build()

		require.False(t, bytes.Equal(relay1.Key, relay2.Key), "different session IDs should produce different keys")
	})

	t.Run("BuildN produces unique relays", func(t *testing.T) {
		relays := NewRelayBuilder(42).BuildN(10)
		require.Len(t, relays, 10)

		// Verify all keys are unique
		keys := make(map[string]bool)
		for _, r := range relays {
			keyStr := string(r.Key)
			require.False(t, keys[keyStr], "duplicate key found")
			keys[keyStr] = true
		}
	})

	t.Run("BuildN is deterministic", func(t *testing.T) {
		relays1 := NewRelayBuilder(42).BuildN(5)
		relays2 := NewRelayBuilder(42).BuildN(5)

		for i := 0; i < 5; i++ {
			require.True(t, bytes.Equal(relays1[i].Key, relays2[i].Key))
			require.True(t, bytes.Equal(relays1[i].Value, relays2[i].Value))
			require.Equal(t, relays1[i].Weight, relays2[i].Weight)
		}
	})

	t.Run("GenerateTestRelays convenience function", func(t *testing.T) {
		relays := GenerateTestRelays(5, 42)
		require.Len(t, relays, 5)

		// Should be identical to using builder
		builderRelays := NewRelayBuilder(42).BuildN(5)
		for i := 0; i < 5; i++ {
			require.True(t, bytes.Equal(relays[i].Key, builderRelays[i].Key))
		}
	})
}

// TestGenerateKnownBitPath verifies known bit path generation.
func TestGenerateKnownBitPath(t *testing.T) {
	t.Run("all_zeros pattern", func(t *testing.T) {
		key := GenerateKnownBitPath("all_zeros")
		require.Len(t, key, 32)
		for _, b := range key {
			require.Equal(t, byte(0), b)
		}
	})

	t.Run("all_ones pattern", func(t *testing.T) {
		key := GenerateKnownBitPath("all_ones")
		require.Len(t, key, 32)
		for _, b := range key {
			require.Equal(t, byte(0xFF), b)
		}
	})

	t.Run("alternating pattern", func(t *testing.T) {
		key := GenerateKnownBitPath("alternating")
		require.Len(t, key, 32)
		for _, b := range key {
			require.Equal(t, byte(0xAA), b)
		}
	})

	t.Run("inverse_alternating pattern", func(t *testing.T) {
		key := GenerateKnownBitPath("inverse_alternating")
		require.Len(t, key, 32)
		for _, b := range key {
			require.Equal(t, byte(0x55), b)
		}
	})

	t.Run("unknown pattern returns zeros", func(t *testing.T) {
		key := GenerateKnownBitPath("unknown")
		require.Len(t, key, 32)
		for _, b := range key {
			require.Equal(t, byte(0), b)
		}
	})
}

// RedisTestSuiteTests embeds RedisTestSuite to test its functionality.
type RedisTestSuiteTests struct {
	RedisTestSuite
}

// TestRedisTestSuite runs the Redis test suite tests.
func TestRedisTestSuite(t *testing.T) {
	suite.Run(t, new(RedisTestSuiteTests))
}

// TestRedisTestSuite_MiniredisConnection verifies miniredis connection works.
func (s *RedisTestSuiteTests) TestRedisTestSuite_MiniredisConnection() {
	// Verify we can ping Redis
	s.Ping()

	// Verify we can set and get a value
	s.SetKey("test:key", "test:value")
	val := s.GetKey("test:key")
	s.Require().Equal("test:value", val)
}

// TestRedisTestSuite_KeyHelpers verifies key helper methods work.
func (s *RedisTestSuiteTests) TestRedisTestSuite_KeyHelpers() {
	// Initially no keys
	s.RequireKeyCount(0)

	// Add a key
	s.SetKey("key1", "value1")
	s.RequireKeyExists("key1")
	s.RequireKeyCount(1)

	// Add another key
	s.SetKey("key2", "value2")
	s.RequireKeyCount(2)

	// Delete a key
	s.DeleteKey("key1")
	s.RequireKeyNotExists("key1")
	s.RequireKeyCount(1)
}

// TestRedisTestSuite_HashHelpers verifies hash helper methods work.
func (s *RedisTestSuiteTests) TestRedisTestSuite_HashHelpers() {
	// Set hash fields
	s.HSet("hash:test", "field1", "value1")
	s.HSet("hash:test", "field2", "value2")

	// Get hash fields
	s.Require().Equal("value1", s.HGet("hash:test", "field1"))
	s.Require().Equal("value2", s.HGet("hash:test", "field2"))

	// Check hash length
	s.Require().Equal(int64(2), s.HLen("hash:test"))

	// Non-existent field returns empty string
	s.Require().Equal("", s.HGet("hash:test", "nonexistent"))
}

// TestRedisTestSuite_SetHelpers verifies set helper methods work.
func (s *RedisTestSuiteTests) TestRedisTestSuite_SetHelpers() {
	// Add to set
	s.SAdd("set:test", "member1", "member2", "member3")

	// Check cardinality
	s.Require().Equal(int64(3), s.SCard("set:test"))

	// Get members
	members := s.SMembers("set:test")
	s.Require().Len(members, 3)
	s.Require().Contains(members, "member1")
	s.Require().Contains(members, "member2")
	s.Require().Contains(members, "member3")
}

// TestRedisTestSuite_FlushIsolation verifies FlushAll between tests.
func (s *RedisTestSuiteTests) TestRedisTestSuite_FlushIsolation() {
	// Set a value
	key := "isolation:test:key"
	value := "isolation:test:value"
	s.SetKey(key, value)
	s.RequireKeyExists(key)

	// Manually flush (simulating what happens between tests)
	s.MiniRedis.FlushAll()

	// Verify it's gone
	_, err := s.RedisClient.Get(s.Ctx, key).Result()
	s.Require().Equal(redis.Nil, err, "key should not exist after flush")
}

// TestRedisTestSuite_IsolationBetweenTests_A sets a key.
func (s *RedisTestSuiteTests) TestRedisTestSuite_IsolationBetweenTests_A() {
	// This test runs before B due to alphabetical ordering
	// Set a key that should NOT be visible in test B
	s.SetKey("cross-test-key", "should-not-leak")
	s.RequireKeyExists("cross-test-key")
}

// TestRedisTestSuite_IsolationBetweenTests_B verifies key from A is not visible.
func (s *RedisTestSuiteTests) TestRedisTestSuite_IsolationBetweenTests_B() {
	// This test runs after A
	// The key from test A should NOT exist due to FlushAll in SetupTest
	s.RequireKeyNotExists("cross-test-key")
	s.RequireKeyCount(0)
}
