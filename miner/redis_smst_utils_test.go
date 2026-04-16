//go:build test

package miner

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/pokt-network/smt/kvstore/simplemap"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/suite"
)

// RedisSMSTTestSuite is the base test suite for all Redis SMST tests.
// It provides a shared miniredis instance to avoid creating thousands of instances.
//
// CRITICAL: All tests must use this suite pattern for Rule #1 compliance:
// - No flaky tests (deterministic, no timing dependencies)
// - No race conditions (pass -race flag)
// - No mocks (real miniredis, not mocks)
// - No timeout weird tests (no time.Sleep)
type RedisSMSTTestSuite struct {
	suite.Suite
	miniRedis   *miniredis.Miniredis
	redisClient *redisutil.Client
	ctx         context.Context
}

// SetupSuite runs ONCE before all tests in the suite.
// Creates a single shared miniredis instance to prevent CPU exhaustion.
func (s *RedisSMSTTestSuite) SetupSuite() {
	// Create miniredis instance
	mr, err := miniredis.Run()
	s.Require().NoError(err, "failed to create miniredis")
	s.miniRedis = mr

	s.ctx = context.Background()

	// Create Redis client pointing to miniredis
	redisURL := fmt.Sprintf("redis://%s", mr.Addr())
	client, err := redisutil.NewClient(s.ctx, redisutil.ClientConfig{
		URL: redisURL,
	})
	s.Require().NoError(err, "failed to create Redis client")
	s.redisClient = client
}

// SetupTest runs BEFORE each test.
// Flushes all data from miniredis to ensure test isolation.
func (s *RedisSMSTTestSuite) SetupTest() {
	s.miniRedis.FlushAll()
}

// TearDownSuite runs ONCE after all tests complete.
// Closes the shared miniredis instance.
func (s *RedisSMSTTestSuite) TearDownSuite() {
	if s.miniRedis != nil {
		s.miniRedis.Close()
	}
	if s.redisClient != nil {
		s.redisClient.Close()
	}
}

// Helper Functions

// testMapStoreSupplier is the default supplier address used by the low-level
// RedisMapStore tests. These tests exercise raw hash operations, not supplier
// semantics, so they all share one supplier scope. Tests that need to assert
// supplier-isolation (multi-supplier) set their own addresses explicitly.
const testMapStoreSupplier = "pokt1test_mapstore_default_supplier"

// createTestRedisStore creates a RedisMapStore for testing.
func (s *RedisSMSTTestSuite) createTestRedisStore(sessionID string) *RedisMapStore {
	store := NewRedisMapStore(s.ctx, s.redisClient, testMapStoreSupplier, sessionID)
	// Type assertion - NewRedisMapStore returns kvstore.MapStore interface
	redisStore, ok := store.(*RedisMapStore)
	s.Require().True(ok, "NewRedisMapStore should return *RedisMapStore")
	return redisStore
}

// createTestRedisSMSTManager creates a RedisSMSTManager for testing.
func (s *RedisSMSTTestSuite) createTestRedisSMSTManager(supplierAddr string) *RedisSMSTManager {
	config := RedisSMSTManagerConfig{
		SupplierAddress: supplierAddr,
		CacheTTL:        0, // No TTL in tests (manual cleanup)
	}

	// Create a no-op logger for tests (discards all output)
	logger := zerolog.Nop()

	return NewRedisSMSTManager(logger, s.redisClient, config)
}

// createInMemorySMST creates an in-memory SMST for comparison testing.
// This uses the same hash functions as Redis SMST to ensure root hash equivalence.
func (s *RedisSMSTTestSuite) createInMemorySMST() smt.SparseMerkleSumTrie {
	store := simplemap.NewSimpleMap()
	hasher := protocol.NewTrieHasher()
	valueHasher := protocol.SMTValueHasher()
	return smt.NewSparseMerkleSumTrie(store, hasher, valueHasher)
}

// assertRootHashEqual compares two root hashes and fails with a detailed message if they don't match.
func (s *RedisSMSTTestSuite) assertRootHashEqual(inMemory, redis []byte, msgAndArgs ...interface{}) {
	s.Require().Equal(inMemory, redis, msgAndArgs...)
}

// Test Data Generators

// testRelay represents a single relay for testing purposes.
type testRelay struct {
	key    []byte
	value  []byte
	weight uint64
}

// generateTestRelays generates deterministic test relays.
// Uses a seed-based approach to ensure reproducibility (Rule #1: no flaky tests).
func (s *RedisSMSTTestSuite) generateTestRelays(count int, seed byte) []testRelay {
	relays := make([]testRelay, count)
	for i := 0; i < count; i++ {
		relays[i] = testRelay{
			key:    []byte(fmt.Sprintf("relay_key_%d_%d", seed, i)),
			value:  []byte(fmt.Sprintf("relay_value_%d_%d", seed, i)),
			weight: uint64((i + 1) * 100), // Incremental weights: 100, 200, 300, ...
		}
	}
	return relays
}

// generateRandomBytes generates cryptographically random bytes.
// Only use this when testing randomness handling (e.g., edge cases).
// For normal tests, use generateTestRelays with a seed for determinism.
func (s *RedisSMSTTestSuite) generateRandomBytes(length int) []byte {
	b := make([]byte, length)
	_, err := rand.Read(b)
	s.Require().NoError(err, "failed to generate random bytes")
	return b
}

// generateKnownBitPath generates a key with a known bit path.
// Useful for testing max height cases and specific tree structures.
//
// For example:
// - All zeros: path that goes left at every branch
// - All ones: path that goes right at every branch
// - Alternating: creates predictable tree structure
func (s *RedisSMSTTestSuite) generateKnownBitPath(pattern string) []byte {
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
	default:
		s.FailNow("unknown pattern: " + pattern)
	}

	return key
}

// Test Suite Registration

// TestRedisSMSTTestSuite runs the test suite using testify/suite.
// This is the entry point for all Redis SMST tests.
func TestRedisSMSTTestSuite(t *testing.T) {
	suite.Run(t, new(RedisSMSTTestSuite))
}

// Verification: Ensure test utilities work correctly

// TestRedisSMSTTestSuite_MiniredisConnection verifies miniredis connection works.
func (s *RedisSMSTTestSuite) TestRedisSMSTTestSuite_MiniredisConnection() {
	// Verify we can ping Redis
	err := s.redisClient.Ping(s.ctx).Err()
	s.Require().NoError(err, "miniredis should be reachable")

	// Verify we can set and get a value
	key := "test:key"
	value := "test:value"
	err = s.redisClient.Set(s.ctx, key, value, 0).Err()
	s.Require().NoError(err, "should be able to set value")

	got, err := s.redisClient.Get(s.ctx, key).Result()
	s.Require().NoError(err, "should be able to get value")
	s.Require().Equal(value, got, "value should match")
}

// TestRedisSMSTTestSuite_RedisStoreCreation verifies RedisMapStore creation.
func (s *RedisSMSTTestSuite) TestRedisSMSTTestSuite_RedisStoreCreation() {
	store := s.createTestRedisStore("test-session-1")
	s.Require().NotNil(store, "store should be created")

	// Verify store is empty
	count, err := store.Len()
	s.Require().NoError(err, "Len should not error")
	s.Require().Equal(0, count, "new store should be empty")
}

// TestRedisSMSTTestSuite_ManagerCreation verifies RedisSMSTManager creation.
func (s *RedisSMSTTestSuite) TestRedisSMSTTestSuite_ManagerCreation() {
	manager := s.createTestRedisSMSTManager("pokt1test123")
	s.Require().NotNil(manager, "manager should be created")
	s.Require().Equal(0, manager.GetTreeCount(), "new manager should have no trees")
}

// TestRedisSMSTTestSuite_InMemoryCreation verifies in-memory SMST creation.
func (s *RedisSMSTTestSuite) TestRedisSMSTTestSuite_InMemoryCreation() {
	smst := s.createInMemorySMST()
	s.Require().NotNil(smst, "in-memory SMST should be created")

	// Verify empty tree has correct root
	root := smst.Root()
	s.Require().NotNil(root, "root should not be nil")
	s.Require().NotEmpty(root, "root should not be empty")
}

// TestRedisSMSTTestSuite_TestDataGeneration verifies test data generators.
func (s *RedisSMSTTestSuite) TestRedisSMSTTestSuite_TestDataGeneration() {
	// Test deterministic relay generation
	relays1 := s.generateTestRelays(10, 42)
	relays2 := s.generateTestRelays(10, 42)
	s.Require().Len(relays1, 10, "should generate 10 relays")
	s.Require().Len(relays2, 10, "should generate 10 relays")

	// Verify determinism (same seed = same output)
	for i := 0; i < 10; i++ {
		s.Require().Equal(relays1[i].key, relays2[i].key, "keys should be deterministic")
		s.Require().Equal(relays1[i].value, relays2[i].value, "values should be deterministic")
		s.Require().Equal(relays1[i].weight, relays2[i].weight, "weights should be deterministic")
	}

	// Test known bit paths
	allZeros := s.generateKnownBitPath("all_zeros")
	allOnes := s.generateKnownBitPath("all_ones")
	alternating := s.generateKnownBitPath("alternating")

	s.Require().Len(allZeros, 32, "should generate 32 bytes")
	s.Require().Len(allOnes, 32, "should generate 32 bytes")
	s.Require().Len(alternating, 32, "should generate 32 bytes")
	s.Require().NotEqual(allZeros, allOnes, "patterns should differ")
	s.Require().NotEqual(allZeros, alternating, "patterns should differ")
}

// TestRedisSMSTTestSuite_FlushIsolation verifies SetupTest flushes data between tests.
func (s *RedisSMSTTestSuite) TestRedisSMSTTestSuite_FlushIsolation() {
	// This test verifies that SetupTest (FlushAll) works correctly

	// Set a value
	key := "isolation:test:key"
	value := "isolation:test:value"
	err := s.redisClient.Set(s.ctx, key, value, 0).Err()
	s.Require().NoError(err)

	// Verify it exists
	got, err := s.redisClient.Get(s.ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(value, got)

	// Manually flush (simulating what happens between tests)
	s.miniRedis.FlushAll()

	// Verify it's gone
	_, err = s.redisClient.Get(s.ctx, key).Result()
	s.Require().Equal(redis.Nil, err, "key should not exist after flush")
}
