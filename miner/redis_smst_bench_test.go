//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/rs/zerolog"
)

// Benchmark Suite Setup
// Uses the same pattern as tests: single shared miniredis instance

type RedisSMSTBenchSuite struct {
	miniRedis   *miniredis.Miniredis
	redisClient *redisutil.Client
	ctx         context.Context
}

func setupBenchSuite(b *testing.B) *RedisSMSTBenchSuite {
	b.Helper()

	// Create miniredis instance
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("failed to create miniredis: %v", err)
	}

	ctx := context.Background()

	// Create Redis client
	redisURL := fmt.Sprintf("redis://%s", mr.Addr())
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{URL: redisURL})
	if err != nil {
		b.Fatalf("failed to create Redis client: %v", err)
	}

	return &RedisSMSTBenchSuite{
		miniRedis:   mr,
		redisClient: client,
		ctx:         ctx,
	}
}

func (s *RedisSMSTBenchSuite) Cleanup() {
	if s.miniRedis != nil {
		s.miniRedis.Close()
	}
	if s.redisClient != nil {
		s.redisClient.Close()
	}
}

func (s *RedisSMSTBenchSuite) createTestRedisStore(sessionID string) *RedisMapStore {
	store := NewRedisMapStore(s.ctx, s.redisClient, "pokt1bench_mapstore_default", sessionID)
	redisStore, ok := store.(*RedisMapStore)
	if !ok {
		panic("NewRedisMapStore should return *RedisMapStore")
	}
	return redisStore
}

func (s *RedisSMSTBenchSuite) createTestRedisSMSTManager(supplierAddr string) *RedisSMSTManager {
	config := RedisSMSTManagerConfig{
		SupplierAddress: supplierAddr,
		CacheTTL:        0,
	}
	logger := zerolog.Nop()
	return NewRedisSMSTManager(logger, s.redisClient, config)
}

// RedisMapStore Operation Benchmarks

// BenchmarkRedisMapStore_Get benchmarks HGET operation.
func BenchmarkRedisMapStore_Get(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	store := suite.createTestRedisStore("bench-session-get")

	// Prepare: Set a value
	key := []byte("benchmark-key")
	value := []byte("benchmark-value")
	if err := store.Set(key, value); err != nil {
		b.Fatalf("failed to set value: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.Get(key)
		if err != nil {
			b.Fatalf("Get failed: %v", err)
		}
	}
}

// BenchmarkRedisMapStore_Set benchmarks HSET operation.
func BenchmarkRedisMapStore_Set(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	store := suite.createTestRedisStore("bench-session-set")

	key := []byte("benchmark-key")
	value := []byte("benchmark-value")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := store.Set(key, value); err != nil {
			b.Fatalf("Set failed: %v", err)
		}
	}
}

// BenchmarkRedisMapStore_Delete benchmarks HDEL operation.
func BenchmarkRedisMapStore_Delete(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	store := suite.createTestRedisStore("bench-session-delete")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		key := []byte(fmt.Sprintf("key-%d", i))
		value := []byte(fmt.Sprintf("value-%d", i))
		if err := store.Set(key, value); err != nil {
			b.Fatalf("Set failed: %v", err)
		}
		b.StartTimer()

		if err := store.Delete(key); err != nil {
			b.Fatalf("Delete failed: %v", err)
		}
	}
}

// BenchmarkRedisMapStore_Len benchmarks HLEN operation.
func BenchmarkRedisMapStore_Len(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	store := suite.createTestRedisStore("bench-session-len")

	// Prepare: Add 100 keys
	for i := 0; i < 100; i++ {
		key := []byte(fmt.Sprintf("key-%d", i))
		value := []byte(fmt.Sprintf("value-%d", i))
		if err := store.Set(key, value); err != nil {
			b.Fatalf("Set failed: %v", err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := store.Len()
		if err != nil {
			b.Fatalf("Len failed: %v", err)
		}
	}
}

// BenchmarkRedisMapStore_Pipeline benchmarks batched HSET with pipeline.
func BenchmarkRedisMapStore_Pipeline(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	store := suite.createTestRedisStore("bench-session-pipeline")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		suite.miniRedis.FlushAll() // Clean between iterations
		b.StartTimer()

		store.BeginPipeline()

		// Buffer 20 Set operations (typical SMST Commit size)
		for j := 0; j < 20; j++ {
			key := []byte(fmt.Sprintf("key-%d", j))
			value := []byte(fmt.Sprintf("value-%d", j))
			if err := store.Set(key, value); err != nil {
				b.Fatalf("Set failed: %v", err)
			}
		}

		if err := store.FlushPipeline(); err != nil {
			b.Fatalf("FlushPipeline failed: %v", err)
		}
	}
}

// SMST Operation Benchmarks

// BenchmarkRedisSMST_Update benchmarks single relay update.
func BenchmarkRedisSMST_Update(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	manager := suite.createTestRedisSMSTManager("pokt1bench_update")
	sessionID := "bench-session-update"

	_, err := manager.GetOrCreateTree(suite.ctx, sessionID)
	if err != nil {
		b.Fatalf("GetOrCreateTree failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		key := []byte(fmt.Sprintf("relay-key-%d", i))
		value := []byte(fmt.Sprintf("relay-value-%d", i))
		weight := uint64((i % 1000) + 1)

		if err := manager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
			b.Fatalf("UpdateTree failed: %v", err)
		}
	}
}

// BenchmarkRedisSMST_UpdateWithPipeline benchmarks update with pipeline optimization.
func BenchmarkRedisSMST_UpdateWithPipeline(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	manager := suite.createTestRedisSMSTManager("pokt1bench_update_pipeline")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sessionID := fmt.Sprintf("bench-session-pipeline-%d", i)
		_, err := manager.GetOrCreateTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("GetOrCreateTree failed: %v", err)
		}
		b.StartTimer()

		// Update 20 relays (typical session size)
		for j := 0; j < 20; j++ {
			key := []byte(fmt.Sprintf("relay-key-%d", j))
			value := []byte(fmt.Sprintf("relay-value-%d", j))
			weight := uint64((j % 1000) + 1)

			if err := manager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
				b.Fatalf("UpdateTree failed: %v", err)
			}
		}
	}
}

// BenchmarkRedisSMST_FlushTree benchmarks finalizing root hash.
func BenchmarkRedisSMST_FlushTree(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	manager := suite.createTestRedisSMSTManager("pokt1bench_flush")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sessionID := fmt.Sprintf("bench-session-flush-%d", i)
		_, err := manager.GetOrCreateTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("GetOrCreateTree failed: %v", err)
		}

		// Add 10 relays
		for j := 0; j < 10; j++ {
			key := []byte(fmt.Sprintf("relay-key-%d", j))
			value := []byte(fmt.Sprintf("relay-value-%d", j))
			weight := uint64((j % 1000) + 1)
			if err := manager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
				b.Fatalf("UpdateTree failed: %v", err)
			}
		}
		b.StartTimer()

		_, err = manager.FlushTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("FlushTree failed: %v", err)
		}
	}
}

// BenchmarkRedisSMST_ProveClosest benchmarks proof generation.
func BenchmarkRedisSMST_ProveClosest(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	manager := suite.createTestRedisSMSTManager("pokt1bench_prove")
	sessionID := "bench-session-prove"

	_, err := manager.GetOrCreateTree(suite.ctx, sessionID)
	if err != nil {
		b.Fatalf("GetOrCreateTree failed: %v", err)
	}

	// Add 10 relays
	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("relay-key-%d", i))
		value := []byte(fmt.Sprintf("relay-value-%d", i))
		weight := uint64((i % 1000) + 1)
		if err := manager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
			b.Fatalf("UpdateTree failed: %v", err)
		}
	}

	_, err = manager.FlushTree(suite.ctx, sessionID)
	if err != nil {
		b.Fatalf("FlushTree failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		path := protocol.GetPathForProof([]byte("relay-key-0"), sessionID)
		_, err := manager.ProveClosest(suite.ctx, sessionID, path)
		if err != nil {
			b.Fatalf("ProveClosest failed: %v", err)
		}
	}
}

// Full Session Benchmarks

// BenchmarkRedisSMST_Session100Relays benchmarks full session with 100 relays.
func BenchmarkRedisSMST_Session100Relays(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	manager := suite.createTestRedisSMSTManager("pokt1bench_session100")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sessionID := fmt.Sprintf("bench-session-100-%d", i)

		// Create tree
		_, err := manager.GetOrCreateTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("GetOrCreateTree failed: %v", err)
		}

		// Add 100 relays
		for j := 0; j < 100; j++ {
			key := []byte(fmt.Sprintf("relay-key-%d", j))
			value := []byte(fmt.Sprintf("relay-value-%d", j))
			weight := uint64((j % 1000) + 1)
			if err := manager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
				b.Fatalf("UpdateTree failed: %v", err)
			}
		}

		// Flush tree
		_, err = manager.FlushTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("FlushTree failed: %v", err)
		}

		// Generate proof
		path := protocol.GetPathForProof([]byte("relay-key-0"), sessionID)
		_, err = manager.ProveClosest(suite.ctx, sessionID, path)
		if err != nil {
			b.Fatalf("ProveClosest failed: %v", err)
		}
	}
}

// BenchmarkRedisSMST_Session1000Relays benchmarks large session with 1000 relays.
func BenchmarkRedisSMST_Session1000Relays(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	manager := suite.createTestRedisSMSTManager("pokt1bench_session1000")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sessionID := fmt.Sprintf("bench-session-1000-%d", i)

		// Create tree
		_, err := manager.GetOrCreateTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("GetOrCreateTree failed: %v", err)
		}

		// Add 1000 relays
		for j := 0; j < 1000; j++ {
			key := []byte(fmt.Sprintf("relay-key-%d", j))
			value := []byte(fmt.Sprintf("relay-value-%d", j))
			weight := uint64((j % 1000) + 1)
			if err := manager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
				b.Fatalf("UpdateTree failed: %v", err)
			}
		}

		// Flush tree
		_, err = manager.FlushTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("FlushTree failed: %v", err)
		}

		// Generate proof
		path := protocol.GetPathForProof([]byte("relay-key-0"), sessionID)
		_, err = manager.ProveClosest(suite.ctx, sessionID, path)
		if err != nil {
			b.Fatalf("ProveClosest failed: %v", err)
		}
	}
}

// Warmup Benchmarks

// BenchmarkRedisSMST_WarmupSingleTree benchmarks restoring 1 tree from Redis.
func BenchmarkRedisSMST_WarmupSingleTree(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	// Prepare: Create and persist a tree
	prepManager := suite.createTestRedisSMSTManager("pokt1bench_warmup_prep")
	sessionID := "bench-session-warmup-single"

	_, err := prepManager.GetOrCreateTree(suite.ctx, sessionID)
	if err != nil {
		b.Fatalf("GetOrCreateTree failed: %v", err)
	}

	// Add 50 relays
	for i := 0; i < 50; i++ {
		key := []byte(fmt.Sprintf("relay-key-%d", i))
		value := []byte(fmt.Sprintf("relay-value-%d", i))
		weight := uint64((i % 1000) + 1)
		if err := prepManager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
			b.Fatalf("UpdateTree failed: %v", err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Create new manager (simulates restart)
		manager := suite.createTestRedisSMSTManager("pokt1bench_warmup")

		// Warmup from Redis
		count, err := manager.WarmupFromRedis(suite.ctx)
		if err != nil {
			b.Fatalf("WarmupFromRedis failed: %v", err)
		}
		if count != 1 {
			b.Fatalf("Expected 1 tree, got %d", count)
		}
	}
}

// BenchmarkRedisSMST_WarmupMultipleTrees benchmarks restoring 10 trees from Redis.
func BenchmarkRedisSMST_WarmupMultipleTrees(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	// Prepare: Create and persist 10 trees
	prepManager := suite.createTestRedisSMSTManager("pokt1bench_warmup_multi_prep")

	for t := 0; t < 10; t++ {
		sessionID := fmt.Sprintf("bench-session-warmup-multi-%d", t)

		_, err := prepManager.GetOrCreateTree(suite.ctx, sessionID)
		if err != nil {
			b.Fatalf("GetOrCreateTree failed: %v", err)
		}

		// Add 20 relays per tree
		for i := 0; i < 20; i++ {
			key := []byte(fmt.Sprintf("relay-key-%d", i))
			value := []byte(fmt.Sprintf("relay-value-%d", i))
			weight := uint64((i % 1000) + 1)
			if err := prepManager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
				b.Fatalf("UpdateTree failed: %v", err)
			}
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Create new manager (simulates restart)
		manager := suite.createTestRedisSMSTManager("pokt1bench_warmup_multi")

		// Warmup from Redis
		count, err := manager.WarmupFromRedis(suite.ctx)
		if err != nil {
			b.Fatalf("WarmupFromRedis failed: %v", err)
		}
		if count != 10 {
			b.Fatalf("Expected 10 trees, got %d", count)
		}
	}
}

// Concurrency Benchmark

// BenchmarkRedisSMST_ConcurrentUpdates benchmarks parallel relay processing.
func BenchmarkRedisSMST_ConcurrentUpdates(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	manager := suite.createTestRedisSMSTManager("pokt1bench_concurrent")
	sessionID := "bench-session-concurrent"

	_, err := manager.GetOrCreateTree(suite.ctx, sessionID)
	if err != nil {
		b.Fatalf("GetOrCreateTree failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := []byte(fmt.Sprintf("relay-key-%d", i))
			value := []byte(fmt.Sprintf("relay-value-%d", i))
			weight := uint64((i % 1000) + 1)

			if err := manager.UpdateTree(suite.ctx, sessionID, key, value, weight); err != nil {
				b.Fatalf("UpdateTree failed: %v", err)
			}
			i++
		}
	})
}

// Pipeline Speedup Verification Benchmark

// BenchmarkRedisSMST_PipelineSpeedup verifies pipeline provides 8-10x speedup.
func BenchmarkRedisSMST_PipelineSpeedup(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	// Benchmark WITHOUT pipeline (individual HSETs)
	b.Run("NoPipeline", func(b *testing.B) {
		store := suite.createTestRedisStore("bench-no-pipeline")

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			b.StopTimer()
			suite.miniRedis.FlushAll()
			b.StartTimer()

			// 20 individual Set operations
			for j := 0; j < 20; j++ {
				key := []byte(fmt.Sprintf("key-%d", j))
				value := []byte(fmt.Sprintf("value-%d", j))
				if err := store.Set(key, value); err != nil {
					b.Fatalf("Set failed: %v", err)
				}
			}
		}
	})

	// Benchmark WITH pipeline (batched HSET)
	b.Run("WithPipeline", func(b *testing.B) {
		store := suite.createTestRedisStore("bench-with-pipeline")

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			b.StopTimer()
			suite.miniRedis.FlushAll()
			b.StartTimer()

			store.BeginPipeline()

			// Buffer 20 Set operations
			for j := 0; j < 20; j++ {
				key := []byte(fmt.Sprintf("key-%d", j))
				value := []byte(fmt.Sprintf("value-%d", j))
				if err := store.Set(key, value); err != nil {
					b.Fatalf("Set failed: %v", err)
				}
			}

			if err := store.FlushPipeline(); err != nil {
				b.Fatalf("FlushPipeline failed: %v", err)
			}
		}
	})

	// NOTE: To verify speedup claim, run:
	//   go test -bench=BenchmarkRedisSMST_PipelineSpeedup -benchtime=10s ./miner
	// The "WithPipeline" variant should be 8-10x faster than "NoPipeline"
}
