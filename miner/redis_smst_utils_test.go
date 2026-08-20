//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/pokt-network/smt/kvstore/simplemap"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/suite"
)

// RedisSMSTTestSuite is the base test suite for all Redis SMST tests.
// One client on the shared real Redis serves the whole suite, under a
// namespace of its own.
//
// CRITICAL: All tests must use this suite pattern for Rule #1 compliance:
// - No flaky tests (deterministic, no timing dependencies)
// - No race conditions (pass -race flag)
// - No fakes (a real Redis 8, not miniredis and not mocks)
// - No timeout weird tests (no time.Sleep)
type RedisSMSTTestSuite struct {
	suite.Suite
	redisPrefix string
	redisClient *redisutil.Client
	ctx         context.Context
}

// SetupSuite runs ONCE before all tests in the suite.
func (s *RedisSMSTTestSuite) SetupSuite() {
	s.ctx = context.Background()
	s.redisClient, s.redisPrefix = newTestRedis(s.T())
}

// SetupTest runs BEFORE each test.
//
// It deletes this suite's OWN subtree, never FLUSHALL: the server is shared
// with every other package running in parallel, so flushing it would delete
// their keys mid-test and the failure would read as a bug in their code.
func (s *RedisSMSTTestSuite) SetupTest() {
	testredis.DeletePrefix(s.T(), s.redisClient, s.redisPrefix)
}

// --- Redis inspection helpers ---
//
// These replace the miniredis handle the suite used to hold. Each goes through
// the suite's client, so it sees exactly what the code under test wrote, in the
// namespace it was configured with.

// keyExists reports whether key is present.
func (s *RedisSMSTTestSuite) keyExists(key string) bool {
	s.T().Helper()
	n, err := s.redisClient.Exists(s.ctx, key).Result()
	s.Require().NoError(err)
	return n == 1
}

// requireTTLNear asserts key's remaining TTL is want, within a second.
//
// miniredis froze time, so an exact equality held. A real server starts
// counting the moment the expiry is set; the tolerance is what that costs, and
// it is far smaller than any wrong answer — a missing TTL, or one from a
// different configured window.
func (s *RedisSMSTTestSuite) requireTTLNear(key string, want time.Duration, msgAndArgs ...any) {
	s.T().Helper()
	got, err := s.redisClient.PTTL(s.ctx, key).Result()
	s.Require().NoError(err)
	s.Require().InDeltaf(want.Seconds(), got.Seconds(), 1.0,
		"TTL on %s: want ~%s, got %s (%v)", key, want, got, msgAndArgs)
}

// requirePersistent asserts key exists with NO expiry set.
//
// Redis answers -1 for "exists, no TTL" and -2 for "no such key"; go-redis
// surfaces both as negative durations. miniredis returned 0 for both, so the
// old assertion could not tell a persistent key from an absent one. This one
// can, and checks that the key is really there.
func (s *RedisSMSTTestSuite) requirePersistent(key string, msgAndArgs ...any) {
	s.T().Helper()
	s.Require().Truef(s.keyExists(key), "%s must exist to be persistent (%v)", key, msgAndArgs)
	got, err := s.redisClient.TTL(s.ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equalf(time.Duration(-1), got,
		"%s must have no TTL, got %s (%v)", key, got, msgAndArgs)
}

// ageKeyTo leaves key with exactly remaining time to live.
//
// It replaces miniredis's FastForward. A real server has no clock to wind, but
// the remaining TTL IS the observable the test is about, so setting it directly
// produces the same state winding the clock forward would have — without a
// sleep and without a fake clock.
func (s *RedisSMSTTestSuite) ageKeyTo(key string, remaining time.Duration) {
	s.T().Helper()
	ok, err := s.redisClient.PExpire(s.ctx, key, remaining).Result()
	s.Require().NoError(err)
	s.Require().Truef(ok, "%s must exist for its TTL to be aged", key)
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
	key := s.redisPrefix + ":test:key"
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

// TestRedisSMSTTestSuite_FlushIsolation verifies SetupTest clears data between
// tests.
//
// The key sits UNDER the suite's prefix, and must: the sweep deletes that
// subtree and nothing else, so a raw key written outside it would survive
// SetupTest and be left behind on a server every other package shares. That is
// the whole contract, and writing the key raw is how you break it.
func (s *RedisSMSTTestSuite) TestRedisSMSTTestSuite_FlushIsolation() {
	// Set a value
	key := s.redisPrefix + ":isolation:test:key"
	value := "isolation:test:value"
	err := s.redisClient.Set(s.ctx, key, value, 0).Err()
	s.Require().NoError(err)

	// Verify it exists
	got, err := s.redisClient.Get(s.ctx, key).Result()
	s.Require().NoError(err)
	s.Require().Equal(value, got)

	// Clear this suite's subtree, which is what SetupTest does between tests.
	testredis.DeletePrefix(s.T(), s.redisClient, s.redisPrefix)

	// Verify it's gone
	_, err = s.redisClient.Get(s.ctx, key).Result()
	s.Require().Equal(redis.Nil, err, "key should not exist after the subtree is cleared")
}
