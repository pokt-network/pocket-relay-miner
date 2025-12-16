package miner

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/smt/kvstore"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// RedisMapStore implements kvstore.MapStore using Redis hashes with pipelining optimization.
// This enables shared storage across HA instances, avoiding local disk IOPS issues.
//
// The RedisMapStore uses a single Redis hash to store all key-value pairs for a session's SMST.
// This provides O(1) access for Get/Set/Delete operations and enables instant failover since
// all instances can access the same Redis data.
//
// Pipelining Optimization:
// During SMST Commit(), the library calls Set() 10-20 times for dirty nodes.
// Instead of 10-20 round trips (20-40ms), we buffer operations and flush in one HSET (2-3ms).
// This provides 8-10× speedup for relay processing.
//
// Redis Hash Structure:
//
//	Key: "ha:smst:{sessionID}:nodes"
//	Fields: hex-encoded SMST node keys
//	Values: raw SMST node data
type RedisMapStore struct {
	redisClient redis.UniversalClient
	hashKey     string // Redis hash key: "ha:smst:{sessionID}:nodes"
	ctx         context.Context

	// Pipeline buffer for batching Set() operations during Commit()
	pipelineMu      sync.Mutex
	pipelineEnabled bool              // true when buffering Set() operations
	pipelineBuffer  map[string][]byte // field -> value buffer
}

// NewRedisMapStore creates a new Redis-backed MapStore for a session.
// The store uses a Redis hash to persist SMST nodes, enabling shared access across HA instances.
//
// Parameters:
//   - ctx: Context for Redis operations
//   - redisClient: Redis client (supports standalone, sentinel, and cluster)
//   - sessionID: Unique session identifier used to namespace the Redis hash
//
// Returns:
//
//	A MapStore implementation backed by Redis
func NewRedisMapStore(
	ctx context.Context,
	redisClient redis.UniversalClient,
	sessionID string,
) kvstore.MapStore {
	return &RedisMapStore{
		redisClient:    redisClient,
		hashKey:        fmt.Sprintf("ha:smst:%s:nodes", sessionID),
		ctx:            ctx,
		pipelineBuffer: make(map[string][]byte),
	}
}

// Get retrieves a value from the Redis hash.
//
// The key is hex-encoded before being used as a Redis hash field name,
// since Redis requires string field names but SMST keys are byte arrays.
//
// Returns nil, nil if the key doesn't exist (per MapStore interface contract).
func (s *RedisMapStore) Get(key []byte) ([]byte, error) {
	start := time.Now()
	defer func() {
		observability.SMSTRedisOperationDuration.WithLabelValues("get").Observe(time.Since(start).Seconds())
	}()

	// Convert key to hex string for Redis field name
	field := hex.EncodeToString(key)

	val, err := s.redisClient.HGet(s.ctx, s.hashKey, field).Bytes()
	if err == redis.Nil {
		observability.SMSTRedisOperations.WithLabelValues("get", "not_found").Inc()
		return nil, nil // Key not found - return nil, nil per interface contract
	}
	if err != nil {
		observability.SMSTRedisOperations.WithLabelValues("get", "error").Inc()
		observability.SMSTRedisErrors.WithLabelValues("get", "redis_error").Inc()
		return nil, err
	}
	observability.SMSTRedisOperations.WithLabelValues("get", "success").Inc()
	return val, nil
}

// Set stores a value in the Redis hash.
//
// The key is hex-encoded before being used as a Redis hash field name.
// If the key already exists, its value is overwritten.
//
// When pipelining is enabled (via BeginPipeline), Set() buffers the operation
// instead of executing it immediately. Call FlushPipeline() to execute all buffered operations.
func (s *RedisMapStore) Set(key, value []byte) error {
	field := hex.EncodeToString(key)

	// Check if we're in pipeline mode
	s.pipelineMu.Lock()
	if s.pipelineEnabled {
		// Buffer the operation instead of executing immediately
		// Make a copy of value to avoid memory aliasing issues
		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)
		s.pipelineBuffer[field] = valueCopy
		s.pipelineMu.Unlock()
		return nil
	}
	s.pipelineMu.Unlock()

	// Not in pipeline mode, execute immediately
	start := time.Now()
	defer func() {
		observability.SMSTRedisOperationDuration.WithLabelValues("set").Observe(time.Since(start).Seconds())
	}()

	err := s.redisClient.HSet(s.ctx, s.hashKey, field, value).Err()
	if err != nil {
		observability.SMSTRedisOperations.WithLabelValues("set", "error").Inc()
		observability.SMSTRedisErrors.WithLabelValues("set", "redis_error").Inc()
		return err
	}
	observability.SMSTRedisOperations.WithLabelValues("set", "success").Inc()
	return nil
}

// Delete removes a key from the Redis hash.
//
// If the key doesn't exist, this operation is a no-op and returns nil.
func (s *RedisMapStore) Delete(key []byte) error {
	start := time.Now()
	defer func() {
		observability.SMSTRedisOperationDuration.WithLabelValues("delete").Observe(time.Since(start).Seconds())
	}()

	field := hex.EncodeToString(key)
	err := s.redisClient.HDel(s.ctx, s.hashKey, field).Err()
	if err != nil {
		observability.SMSTRedisOperations.WithLabelValues("delete", "error").Inc()
		observability.SMSTRedisErrors.WithLabelValues("delete", "redis_error").Inc()
		return err
	}
	observability.SMSTRedisOperations.WithLabelValues("delete", "success").Inc()
	return nil
}

// Len returns the number of keys in the Redis hash.
//
// This operation is O(1) as it uses Redis's HLEN command.
func (s *RedisMapStore) Len() (int, error) {
	start := time.Now()
	defer func() {
		observability.SMSTRedisOperationDuration.WithLabelValues("len").Observe(time.Since(start).Seconds())
	}()

	count, err := s.redisClient.HLen(s.ctx, s.hashKey).Result()
	if err != nil {
		observability.SMSTRedisOperations.WithLabelValues("len", "error").Inc()
		observability.SMSTRedisErrors.WithLabelValues("len", "redis_error").Inc()
		return 0, err
	}
	observability.SMSTRedisOperations.WithLabelValues("len", "success").Inc()
	return int(count), nil
}

// ClearAll deletes the entire Redis hash.
//
// This is an atomic operation that removes all SMST nodes for the session.
// After calling ClearAll, Len() will return 0.
func (s *RedisMapStore) ClearAll() error {
	start := time.Now()
	defer func() {
		observability.SMSTRedisOperationDuration.WithLabelValues("clear_all").Observe(time.Since(start).Seconds())
	}()

	err := s.redisClient.Del(s.ctx, s.hashKey).Err()
	if err != nil {
		observability.SMSTRedisOperations.WithLabelValues("clear_all", "error").Inc()
		observability.SMSTRedisErrors.WithLabelValues("clear_all", "redis_error").Inc()
		return err
	}
	observability.SMSTRedisOperations.WithLabelValues("clear_all", "success").Inc()
	return nil
}

// BeginPipeline enables buffering mode for Set() operations.
// All subsequent Set() calls will be buffered until FlushPipeline() is called.
// This is used during SMST Commit() to batch 10-20 HSET operations into a single round trip.
func (s *RedisMapStore) BeginPipeline() {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()

	s.pipelineEnabled = true
	// Clear buffer from any previous pipeline
	s.pipelineBuffer = make(map[string][]byte)
}

// FlushPipeline executes all buffered Set() operations in a single Redis HSET command.
// This provides 8-10× speedup compared to individual HSET calls during SMST Commit().
//
// After flushing, pipeline mode is disabled and subsequent Set() calls execute immediately.
func (s *RedisMapStore) FlushPipeline() error {
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()

	// If no buffered operations, nothing to do
	if len(s.pipelineBuffer) == 0 {
		s.pipelineEnabled = false
		return nil
	}

	start := time.Now()
	defer func() {
		observability.SMSTRedisOperationDuration.WithLabelValues("flush_pipeline").Observe(time.Since(start).Seconds())
	}()

	// Build field-value pairs for HSET
	// Redis HSET accepts: HSET key field1 value1 field2 value2 ...
	args := make([]interface{}, 0, len(s.pipelineBuffer)*2)
	for field, value := range s.pipelineBuffer {
		args = append(args, field, value)
	}

	// Execute batched HSET
	err := s.redisClient.HSet(s.ctx, s.hashKey, args...).Err()
	if err != nil {
		observability.SMSTRedisOperations.WithLabelValues("flush_pipeline", "error").Inc()
		observability.SMSTRedisErrors.WithLabelValues("flush_pipeline", "redis_error").Inc()
		s.pipelineEnabled = false
		return fmt.Errorf("failed to flush pipeline: %w", err)
	}

	// Track metrics (count as bulk operation)
	observability.SMSTRedisOperations.WithLabelValues("flush_pipeline", "success").Inc()
	observability.SMSTRedisOperations.WithLabelValues("set", "success").Add(float64(len(s.pipelineBuffer)))

	// Clear buffer and disable pipeline mode
	s.pipelineBuffer = make(map[string][]byte)
	s.pipelineEnabled = false

	return nil
}

// Verify interface compliance at compile time.
var _ kvstore.MapStore = (*RedisMapStore)(nil)
