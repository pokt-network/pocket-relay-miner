# Redis Production Optimization Summary

## Current State

### Redis Metrics (Production)
- **Memory**: 14.75GB / 28GB (53%)
- **Operations**: 1770 ops/sec
- **Clients**: 865 connected (640 blocked on XREADGROUP)
- **Cache Hit Rate**: 99.99% (1,935,521 hits / 13 misses)
- **AOF File**: 13.3GB (rewriting in progress)

### Go-Redis Client Settings
**Current** (defaults via `redis.ParseURL()` + `redis.NewClient()`):
- PoolSize: `10 × runtime.GOMAXPROCS` = **160 connections** (with 16 CPU cores)
- MinIdleConns: **0** (connections created on demand)
- MaxRetries: 3
- DialTimeout: 5s
- ReadTimeout: 3s
- WriteTimeout: 3s

**Source**: [go-redis/options.go](https://github.com/redis/go-redis/blob/v9.7.0/options.go)

---

## Recommended Optimizations

### 1. Redis Server Configuration (ConfigMap)

```yaml
# =============================================================================
# MEMORY MANAGEMENT
# =============================================================================
maxmemory 30064771072  # 88% of 32Gi = ~28GB
maxmemory-policy allkeys-lru

# Active defragmentation
activedefrag yes
active-defrag-ignore-bytes 100mb
active-defrag-threshold-lower 10
active-defrag-threshold-upper 25

# =============================================================================
# MULTI-THREADING (Redis 6.0+)
# =============================================================================
io-threads 3  # Use 3 of 4 CPU cores for I/O
io-threads-do-reads yes  # Parallel read/write

# Expected improvement: Up to 72% better throughput
# Source: https://aws.amazon.com/blogs/database/enhanced-io-multiplexing-for-amazon-elasticache-for-redis/

# =============================================================================
# AOF PERSISTENCE
# =============================================================================
appendonly yes
appendfsync everysec
no-appendfsync-on-rewrite yes
auto-aof-rewrite-percentage 100
auto-aof-rewrite-min-size 512mb  # Increased from 64MB
aof-rewrite-incremental-fsync yes
aof-use-rdb-preamble yes  # Hybrid format = faster loading

# =============================================================================
# LAZY FREEING (non-blocking deletes)
# =============================================================================
lazyfree-lazy-eviction yes
lazyfree-lazy-expire yes
lazyfree-lazy-server-del yes
lazyfree-lazy-user-del yes

# =============================================================================
# EVENT LOOP FREQUENCY
# =============================================================================
hz 100  # Increased from 10 for better stream eviction

# =============================================================================
# NETWORKING
# =============================================================================
tcp-backlog 511
tcp-keepalive 300
timeout 600  # 10 min idle timeout
```

### 2. Go-Redis Client Configuration (Recommended)

**For miner/relayer `cmd/cmd_*.go`:**

```go
// Current (using defaults):
redisOpts, err := redis.ParseURL(redisURL)
redisClient := redis.NewClient(redisOpts)

// Recommended (explicit pooling for production):
redisOpts, err := redis.ParseURL(redisURL)
if err != nil {
    return err
}

// Production connection pool tuning
redisOpts.PoolSize = 200          // Increase from 160 for burst capacity
redisOpts.MinIdleConns = 50       // Keep 50 warm connections (avoid dial latency)
redisOpts.PoolTimeout = 4 * time.Second   // Wait up to 4s for connection from pool
redisOpts.ConnMaxIdleTime = 5 * time.Minute  // Recycle idle connections
redisOpts.ConnMaxLifetime = 0     // No max lifetime (connections don't expire)

redisClient := redis.NewClient(redisOpts)
```

**Rationale:**
- **PoolSize: 200**: Room for 865 clients + burst growth (was 160)
- **MinIdleConns: 50**: Eliminates connection establishment latency (~1-5ms per dial)
- **PoolTimeout: 4s**: Fail fast if pool exhausted (better than hanging)
- **ConnMaxIdleTime: 5min**: Recycle stale connections, prevent resource leaks

---

## Expected Improvements

### Redis Server
- **Throughput**: +50-72% (io-threads + lazy freeing)
- **P99 Latency**: -30-50% (non-blocking operations)
- **Restart Time**: -40% (AOF hybrid format)
- **Memory Efficiency**: +10-15% (active defrag)

### Go-Redis Client
- **Connection Latency**: -100% for warm connections (MinIdleConns)
- **Burst Capacity**: +25% (200 vs 160 pool size)
- **Resource Usage**: More predictable (idle connection management)

---

## Production Deployment Plan

### Phase 1: Redis Server Config (GitOps)
1. Apply ConfigMap with optimized settings
2. Restart Redis pod (expected: 30-60s AOF reload)
3. Monitor metrics:
   - `redis_instantaneous_ops_per_sec` (target: 3000+)
   - `redis_used_memory_bytes` (watch for fragmentation)
   - `ha_relayer_*` (relay processing latency)

### Phase 2: Go-Redis Client Pool
1. Update `cmd/cmd_miner.go` lines ~157-161
2. Update `cmd/cmd_relayer.go` lines ~157-161
3. Build and deploy new images
4. Monitor:
   - Connection pool exhaustion (should not occur)
   - Dial latency reduction (check startup logs)

### Phase 3: Validation
1. Run load test: 1000+ RPS sustained
2. Verify batch ACK performance (2000ms → 40ms target)
3. Check Redis CPU usage (should distribute across 4 cores)

---

## Rollback Plan

If issues occur:

**Redis Config**:
```bash
kubectl --context=nodes -n redis delete configmap redis-standalone-ext-config
# Operator will use defaults (appendonly yes, no tuning)
kubectl --context=nodes -n redis delete pod redis-standalone-0
```

**Go-Redis Client**:
- Redeploy previous image (defaults work fine)
- Pool exhaustion unlikely with current workload

---

## Monitoring Queries

**Redis Throughput**:
```promql
rate(redis_commands_processed_total[5m])
```

**Connection Pool Usage** (not exposed by go-redis by default):
```go
// Add custom metrics:
poolStats := redisClient.PoolStats()
prometheus.NewGauge(...).Set(float64(poolStats.Hits))
prometheus.NewGauge(...).Set(float64(poolStats.Misses))
prometheus.NewGauge(...).Set(float64(poolStats.Timeouts))
prometheus.NewGauge(...).Set(float64(poolStats.TotalConns))
prometheus.NewGauge(...).Set(float64(poolStats.IdleConns))
```

**Relayer Performance**:
```promql
# P99 relay latency
histogram_quantile(0.99, rate(ha_relayer_relay_latency_seconds_bucket[5m]))

# Batch ACK latency (should drop from 2000ms to 40ms)
histogram_quantile(0.99, rate(ha_miner_batch_ack_latency_seconds_bucket[5m]))
```

---

## References

- [AWS ElastiCache Enhanced I/O Multiplexing](https://aws.amazon.com/blogs/database/enhanced-io-multiplexing-for-amazon-elasticache-for-redis/)
- [go-redis v9 Options](https://github.com/redis/go-redis/blob/v9.7.0/options.go)
- [Redis I/O Threading Guide](https://redis.uptrace.dev/guide/go-redis-debugging.html)
