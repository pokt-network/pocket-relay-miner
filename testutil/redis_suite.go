//go:build test

package testutil

import (
	"context"
	"fmt"

	"github.com/alicebob/miniredis/v2"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/suite"
)

// RedisTestSuite provides a shared miniredis instance for tests.
// Embed this in your test suite to get automatic Redis setup/teardown.
//
// CRITICAL: This suite enforces Rule #1 compliance from CLAUDE.md:
// - No flaky tests (FlushAll between each test for isolation)
// - No race conditions (pass -race flag)
// - No mock/fake tests (uses real miniredis, not mocks)
// - No timeout weird tests (no time.Sleep dependencies)
//
// Usage:
//
//	type MyTestSuite struct {
//	    testutil.RedisTestSuite
//	}
//
//	func (s *MyTestSuite) TestSomething() {
//	    // Use s.RedisClient, s.MiniRedis, s.Ctx
//	    err := s.RedisClient.Set(s.Ctx, "key", "value", 0).Err()
//	    s.Require().NoError(err)
//	}
//
//	func TestMyTestSuite(t *testing.T) {
//	    suite.Run(t, new(MyTestSuite))
//	}
type RedisTestSuite struct {
	suite.Suite

	// MiniRedis is the embedded miniredis instance.
	// Use this for direct miniredis operations (e.g., inspecting internal state).
	MiniRedis *miniredis.Miniredis

	// RedisClient is the Redis client connected to miniredis.
	// Use this for normal Redis operations.
	RedisClient *redisutil.Client

	// Ctx is a background context for Redis operations.
	Ctx context.Context
}

// SetupSuite runs ONCE before all tests in the suite.
// Creates a single shared miniredis instance to prevent CPU exhaustion.
func (s *RedisTestSuite) SetupSuite() {
	// Create miniredis instance
	mr, err := miniredis.Run()
	s.Require().NoError(err, "failed to create miniredis")
	s.MiniRedis = mr

	s.Ctx = context.Background()

	// Create Redis client pointing to miniredis
	redisURL := fmt.Sprintf("redis://%s", mr.Addr())
	client, err := redisutil.NewClient(s.Ctx, redisutil.ClientConfig{
		URL: redisURL,
	})
	s.Require().NoError(err, "failed to create Redis client")
	s.RedisClient = client
}

// SetupTest runs BEFORE each test.
// Flushes all data from miniredis to ensure test isolation.
func (s *RedisTestSuite) SetupTest() {
	s.MiniRedis.FlushAll()
}

// TearDownSuite runs ONCE after all tests complete.
// Closes the shared miniredis instance.
func (s *RedisTestSuite) TearDownSuite() {
	if s.MiniRedis != nil {
		s.MiniRedis.Close()
	}
	if s.RedisClient != nil {
		s.RedisClient.Close()
	}
}

// Helper Methods

// RequireKeyExists asserts that a key exists in Redis.
func (s *RedisTestSuite) RequireKeyExists(key string) {
	exists, err := s.RedisClient.Exists(s.Ctx, key).Result()
	s.Require().NoError(err, "failed to check key existence")
	s.Require().Equal(int64(1), exists, "key %q should exist", key)
}

// RequireKeyNotExists asserts that a key does NOT exist in Redis.
func (s *RedisTestSuite) RequireKeyNotExists(key string) {
	exists, err := s.RedisClient.Exists(s.Ctx, key).Result()
	s.Require().NoError(err, "failed to check key existence")
	s.Require().Equal(int64(0), exists, "key %q should not exist", key)
}

// GetKeyCount returns the total number of keys in Redis.
func (s *RedisTestSuite) GetKeyCount() int {
	keys, err := s.RedisClient.Keys(s.Ctx, "*").Result()
	s.Require().NoError(err, "failed to get keys")
	return len(keys)
}

// SetKey is a helper to set a string key-value pair.
func (s *RedisTestSuite) SetKey(key, value string) {
	err := s.RedisClient.Set(s.Ctx, key, value, 0).Err()
	s.Require().NoError(err, "failed to set key %q", key)
}

// GetKey is a helper to get a string value by key.
func (s *RedisTestSuite) GetKey(key string) string {
	val, err := s.RedisClient.Get(s.Ctx, key).Result()
	if err == redis.Nil {
		return ""
	}
	s.Require().NoError(err, "failed to get key %q", key)
	return val
}

// DeleteKey is a helper to delete a key.
func (s *RedisTestSuite) DeleteKey(key string) {
	err := s.RedisClient.Del(s.Ctx, key).Err()
	s.Require().NoError(err, "failed to delete key %q", key)
}

// HSet is a helper to set a hash field.
func (s *RedisTestSuite) HSet(key, field string, value interface{}) {
	err := s.RedisClient.HSet(s.Ctx, key, field, value).Err()
	s.Require().NoError(err, "failed to set hash field %q:%q", key, field)
}

// HGet is a helper to get a hash field.
func (s *RedisTestSuite) HGet(key, field string) string {
	val, err := s.RedisClient.HGet(s.Ctx, key, field).Result()
	if err == redis.Nil {
		return ""
	}
	s.Require().NoError(err, "failed to get hash field %q:%q", key, field)
	return val
}

// HLen is a helper to get the number of fields in a hash.
func (s *RedisTestSuite) HLen(key string) int64 {
	count, err := s.RedisClient.HLen(s.Ctx, key).Result()
	s.Require().NoError(err, "failed to get hash length for %q", key)
	return count
}

// SAdd is a helper to add members to a set.
func (s *RedisTestSuite) SAdd(key string, members ...interface{}) {
	err := s.RedisClient.SAdd(s.Ctx, key, members...).Err()
	s.Require().NoError(err, "failed to add to set %q", key)
}

// SMembers is a helper to get all members of a set.
func (s *RedisTestSuite) SMembers(key string) []string {
	members, err := s.RedisClient.SMembers(s.Ctx, key).Result()
	s.Require().NoError(err, "failed to get set members for %q", key)
	return members
}

// SCard is a helper to get the cardinality of a set.
func (s *RedisTestSuite) SCard(key string) int64 {
	count, err := s.RedisClient.SCard(s.Ctx, key).Result()
	s.Require().NoError(err, "failed to get set cardinality for %q", key)
	return count
}

// RequireKeyCount asserts the exact number of keys in Redis.
func (s *RedisTestSuite) RequireKeyCount(expected int) {
	actual := s.GetKeyCount()
	s.Require().Equal(expected, actual, "expected %d keys, got %d", expected, actual)
}

// Ping verifies the Redis connection is alive.
func (s *RedisTestSuite) Ping() {
	err := s.RedisClient.Ping(s.Ctx).Err()
	s.Require().NoError(err, "Redis ping failed")
}
