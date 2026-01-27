# Redis Architecture

Redis is the central state store for distributed coordination. **It's NOT just a cache** - it stores critical revenue-generating data.

## Configuration

### Connection Settings

```yaml
redis:
  url: "redis://localhost:6379"
  pool_size: 50                      # Formula: numSuppliers + 20 (see below)
  min_idle_conns: 10                 # Warm connections
  pool_timeout_seconds: 4            # Wait time for connection from pool
  conn_max_idle_time_seconds: 300    # Close idle connections after 5 minutes
```

### Pool Size Formula

**CRITICAL**: Stream consumption uses `BLOCK 0` (TRUE PUSH) which holds 1 connection per supplier indefinitely.

**Note**: Suppliers are auto-discovered from keyring keys. The number of suppliers equals the number of keys configured in the keyring.

```
pool_size = numSuppliers + 20 overhead
```

**Breakdown of connections:**

| Type | Connections | Duration |
|------|-------------|----------|
| Stream consumer (per supplier) | 1 × numSuppliers | Held indefinitely (BLOCK 0) |
| Block event pub/sub | 1 | Held indefinitely |
| Cache invalidation pub/sub | 2-3 | Held indefinitely |
| Supplier registry pub/sub | 1 | Held indefinitely |
| SMST/Session/Cache ops | ~10-15 | Fast, shared from pool |

**Examples:**

| Suppliers | Formula | Pool Size |
|-----------|---------|-----------|
| 1 | 1 + 20 | 21 |
| 10 | 10 + 20 | 30 |
| 30 | 30 + 20 | 50 (default) |
| 100 | 100 + 20 | 120 |

**Symptoms of insufficient pool size:**
- `redis: connection pool timeout` errors
- Delayed relay consumption
- Leader heartbeat failures

### Namespace Settings

All keys use configurable prefixes (default shown):

```yaml
redis:
  namespace:
    base_prefix: "ha"           # Root prefix for all keys
    cache_prefix: "cache"       # Cache data
    events_prefix: "events"     # Pub/sub channels
    streams_prefix: "relays"    # Redis Streams (WAL)
    miner_prefix: "miner"       # Miner state
    supplier_prefix: "supplier" # Supplier registry
    meter_prefix: "meter"       # Relay metering
    params_prefix: "params"     # Cached params
```

---

## KeyBuilder

All Redis keys MUST be built via `KeyBuilder` - never hardcode key strings.

```go
// Get KeyBuilder from Redis client
kb := redisClient.KB()

// Examples
kb.MinerSessionKey(supplier, sessionID)   // ha:miner:sessions:{supplier}:{sessionID}
kb.MinerSMSTNodesKey(sessionID)           // ha:smst:{sessionID}:nodes
kb.StreamKey(supplier)                     // ha:relays:{supplier}
kb.CacheKey("application", address)       // ha:cache:application:{address}
kb.MeterKey(sessionID)                    // ha:meter:{sessionID}
kb.ServiceFactorDefaultKey()              // ha:service_factor:default
kb.ServiceFactorServiceKey(serviceID)     // ha:service_factor:service:{serviceID}
```

Reference: `transport/redis/namespace.go`

---

## Key Patterns

### Critical Data (Must Persist)

| Pattern                                      | Type   | Purpose                              |
|----------------------------------------------|--------|--------------------------------------|
| `ha:smst:{sessionID}:nodes`                  | Hash   | SMST tree nodes for proof generation |
| `ha:relays:{supplierAddress}`                | Stream | WAL for mined relays                 |
| `ha:miner:sessions:{supplier}:{sessionID}`   | String | Session metadata                     |
| `ha:miner:sessions:{supplier}:state:{state}` | Set    | Session state indexes                |
| `ha:miner:sessions:{supplier}:index`         | Set    | All session IDs                      |

**Loss Impact**: Cannot generate proofs → revenue loss

### Rebuildable Data (Optional Persist)

| Pattern                    | Type   | Purpose                     |
|----------------------------|--------|-----------------------------|
| `ha:cache:application:*`   | String | App cache (rebuild from L3) |
| `ha:cache:service:*`       | String | Service cache               |
| `ha:cache:*_params`        | String | Params cache                |
| `ha:miner:global_leader`   | String | Leader lock (30s TTL)       |
| `ha:miner:dedup:session:*` | Set    | Relay deduplication         |

---

## Persistence Configuration

```yaml
# AOF with 1-second sync (100x faster than fsync always)
appendonly: "yes"
appendfsync: "everysec"
no-appendfsync-on-rewrite: "yes"
auto-aof-rewrite-percentage: "100"
auto-aof-rewrite-min-size: "512mb"
aof-use-rdb-preamble: "yes"

# Disable RDB (redundant with AOF)
save: ""

# Memory policy
maxmemory-policy: "noeviction"
```

**Trade-off**: Max 1-second data loss on crash (<0.01% of 4-hour session)

---

## Performance Tuning

### Server Config (Redis 8.2+)

```yaml
# Multi-threading (+50-72% throughput)
io-threads: 3
io-threads-do-reads: "yes"

# Lazy freeing (non-blocking deletes)
lazyfree-lazy-eviction: "yes"
lazyfree-lazy-expire: "yes"
lazyfree-lazy-server-del: "yes"

# Active defragmentation
activedefrag: "yes"
active-defrag-threshold-lower: 10
active-defrag-threshold-upper: 25

# Event loop frequency
hz: 100
```

### Go-Redis Client

```go
// Formula: numSuppliers + 20 overhead
// Default handles up to 30 suppliers (auto-discovered from keyring)
redisOpts.PoolSize = 50                         // numSuppliers + 20
redisOpts.MinIdleConns = 10                     // Keep connections warm
redisOpts.PoolTimeout = 4 * time.Second         // pool_timeout_seconds
redisOpts.ConnMaxIdleTime = 5 * time.Minute     // conn_max_idle_time_seconds
```

---

## Standalone vs Cluster

| Aspect     | Standalone                 | Cluster (3+3)   |
|------------|----------------------------|-----------------|
| Throughput | 1000+ RPS                  | 3000+ RPS       |
| Failover   | Manual                     | Automatic (<5s) |
| Latency    | 1-2ms p95                  | 1-2ms p95       |
| Use Case   | Dev/test/Production(risky) | Production      |

### Cluster Connection

```go
redis.NewClusterClient(&redis.ClusterOptions{
    Addrs: []string{"leader-0:6379", "leader-1:6379", "leader-2:6379"},
    RouteByLatency: true,
})
```

**Note**: Use hash tags `{supplier}` to colocate related keys on same slot.

---

## Monitoring

### Key Metrics

```promql
# Throughput
rate(redis_commands_processed_total[5m])

# Memory
redis_memory_used_bytes / redis_memory_max_bytes

# Replication lag (cluster)
redis_master_repl_offset
```

### Health Check

```bash
redis-cli INFO persistence | grep aof_last_write_status
# Expected: ok
```

---

## Debug Commands

```bash
# Check keys by pattern
pocket-relay-miner redis keys --pattern "ha:smst:*" --stats

# Inspect session state
pocket-relay-miner redis sessions --supplier pokt1abc...

# View SMST tree
pocket-relay-miner redis smst --session session_123

# Monitor streams
pocket-relay-miner redis streams --supplier pokt1abc...
```
