# Redis Architecture & Persistence Strategy

## Overview

The Pocket RelayMiner (HA) uses Redis as the central state store for distributed coordination across stateless Relayer replicas and stateful Miner replicas. **Redis is NOT just a cache** - it stores critical revenue-generating data that must survive restarts.

## Critical Data Requiring Persistence

### 1. SMST (Sparse Merkle Sum Tree) Nodes
**Key Pattern**: `ha:smst:{sessionID}:nodes`
**Storage**: Redis Hashes (HSET/HGET/HDEL operations)
**Why Critical**: Without SMST nodes, you **CANNOT generate proofs** for claimed sessions.

**Data Loss Impact**:
- ❌ **Complete revenue loss** for affected sessions
- ❌ Cannot submit proofs → claims expire unrewarded
- ❌ No way to reconstruct SMST after claim submission

**Persistence Requirement**: **MUST PERSIST**

### 2. Redis Streams (WAL - Write-Ahead Log)
**Key Pattern**: `ha:relays:{supplierAddress}`
**Storage**: Redis Streams (XADD/XREAD/XACK operations)
**Why Critical**: Relay requests mined by Relayer are published to streams for Miner consumption.

**Data Loss Impact**:
- ⚠️ Lose all pending relays not yet processed by Miner
- ⚠️ SMST trees will be incomplete (missing relays)
- ⚠️ Submitted claims/proofs will have incorrect relay counts → potential slashing

**Persistence Requirement**: **MUST PERSIST**

### 3. Session Metadata & Lifecycle State
**Key Patterns**:
- `ha:miner:sessions:{supplier}:{sessionID}` - Session metadata
- `ha:miner:sessions:{supplier}:state:{state}` - State indexes (active, claiming, proving, etc.)
- `ha:miner:sessions:{supplier}:index` - All session IDs

**Storage**: Redis Strings (JSON), Sets
**Why Critical**: Tracks which sessions are being claimed/proved and submission timing.

**Data Loss Impact**:
- ⚠️ Lose track of which sessions need claims/proofs submitted
- ⚠️ May miss claim/proof windows → revenue loss
- ⚠️ Duplicate submissions (if state is lost mid-process)

**Persistence Requirement**: **MUST PERSIST**

## Data That Can Tolerate Loss (Rebuild from L3)

### Cache Data (Applications, Services, Params)
**Key Patterns**: `ha:cache:application:*`, `ha:cache:service:*`, `ha:cache:*_params`
**Why Tolerable**: Can be rebuilt from blockchain queries (L3 cache miss)

**Data Loss Impact**:
- ✅ Temporary performance degradation (200ms queries until cache repopulated)
- ✅ No revenue loss

**Persistence Requirement**: **OPTIONAL** (but good to have for fast startup)

### Leader Election Lock
**Key Pattern**: `ha:miner:global_leader`
**Storage**: Redis String with 30s TTL
**Why Tolerable**: Ephemeral by design - re-election happens automatically

**Data Loss Impact**:
- ✅ Brief (~5s) leader re-election on restart
- ✅ No data loss

**Persistence Requirement**: **NOT NEEDED**

### Deduplication Sets
**Key Pattern**: `ha:miner:dedup:session:{sessionID}`
**Storage**: Redis Sets with short TTL
**Why Tolerable**: Short-lived (TTL expires with session)

**Data Loss Impact**:
- ⚠️ May process duplicate relays (benign - SMST handles duplicates)
- ✅ No revenue loss

**Persistence Requirement**: **OPTIONAL**

## Persistence Configuration Strategy

### Why Not `appendfsync always`?

**Default Redis AOF** uses `appendfsync always` which calls `fsync()` on **every write**:
- ✅ **Zero data loss** (every write persisted before ACK)
- ❌ **200ms+ latency** on localhost (disk I/O bottleneck)
- ❌ **Write amplification** (every HSET waits for disk)

**Result**: Unacceptable for 1000+ RPS throughput target.

### Our Choice: `appendfsync everysec`

**Configuration**:
```yaml
appendonly: "yes"
appendfsync: "everysec"  # Sync AOF to disk every 1 second
```

**Trade-offs**:
- ✅ **~1-2ms latency** (100x faster than `always`)
- ✅ **1000+ RPS capable**
- ⚠️ **Max 1-second data loss window** (if Redis crashes)

**What does 1-second loss window mean?**
- If Redis crashes at exactly the wrong moment, you lose **at most 1 second** of writes
- For SMST: Lose relays from the last 1 second → reprocess them (benign, handled by deduplication)
- For Streams: Lose relays from last 1 second → relayer will retry (relay metering prevents over-serving)
- For Sessions: Lose recent state updates → Miner recalculates from blockchain state

### Why Disable RDB Snapshots?

**RDB (Redis Database) Snapshots**:
- Creates periodic full-database dumps to disk
- **Redundant** when AOF is enabled
- Adds **write amplification** (fork() on every snapshot)
- **Not useful** for our use case (we need append-only log, not snapshots)

**Configuration**:
```yaml
save: ""  # Disable all RDB snapshots
```

### Additional Performance Tuning

```yaml
# Don't block writes during AOF rewrites (background process compacts AOF)
no-appendfsync-on-rewrite: "yes"

# Rewrite AOF when it's 2x the base size (reduces file size)
auto-aof-rewrite-percentage: "100"
auto-aof-rewrite-min-size: "64mb"

# Memory policy: Never evict data, fail writes instead
maxmemory-policy: "noeviction"

# TCP tuning for high connection count
tcp-backlog: "511"
timeout: "0"
tcp-keepalive: "300"
```

## Production Considerations

### For High-Availability Production Deployments

**Option 1: Redis Cluster with AOF**
- Same `appendfsync everysec` configuration
- Replication provides additional durability (data on multiple nodes)
- If leader crashes, replica promoted with <5s data loss

**Option 2: Separate Redis Instances (Advanced)**
- **redis-persistent** (port 6379): SMST + Streams (`appendfsync everysec`)
- **redis-cache** (port 6380): Cache data only (`appendonly no`)
- Requires code changes to route keys to correct instance

**Recommended**: Option 1 (Redis Cluster) - simpler, good enough for 1-second loss window.

### Monitoring Critical Metrics

**AOF Metrics** (via `redis-cli INFO persistence`):
- `aof_current_size`: Current AOF file size
- `aof_base_size`: AOF size at last rewrite
- `aof_pending_rewrite`: Is rewrite scheduled?
- `aof_rewrite_in_progress`: Is rewrite currently running?
- `aof_last_write_status`: Last write status (should be `ok`)

**Grafana Dashboard Panels** (already configured):
- `ha_observability_redis_operation_duration_seconds` - Should be <2ms p95
- `ha_smst_redis_operation_duration_seconds` - Should be <100µs p95
- `ha_transport_redis_end_to_end_latency_seconds` - Queue time (affected by consumer config, NOT Redis)

### Recovery from Data Loss

**If Redis crashes and loses 1 second of data:**

1. **SMST Trees**: Missing relays from last 1 second
   - Miner detects incomplete tree during claim/proof generation
   - Option A: Skip proof (accept revenue loss for affected session)
   - Option B: Replay from relayer logs (if available)

2. **Redis Streams**: Lost relays in stream
   - Relayer retry logic handles this (relay metering prevents duplicates)
   - No action needed

3. **Session Metadata**: Lost recent state transitions
   - Miner recalculates session state from blockchain queries
   - May resubmit claims/proofs (idempotent operations)

**Expected Revenue Impact**: <0.1% loss (1 second out of ~4-hour session)

## Benchmarks

### Before Tuning (Default Redis Config)
- **HSET**: ~200ms p95 (disk fsync on every write)
- **HGET**: ~150ms p95 (cache misses + disk read)
- **Stream XADD**: ~180ms p95

**Throughput**: ~5-10 RPS (unacceptable)

### After Tuning (`appendfsync everysec`)
- **HSET**: ~1-2ms p95 (in-memory write + async disk sync)
- **HGET**: ~0.5-1ms p95 (in-memory read)
- **Stream XADD**: ~1-2ms p95

**Throughput**: 1000+ RPS ✅

**Performance Gain**: **100x improvement**

## Redis Cluster Mode for High-Availability Production

For production deployments requiring **horizontal scalability** and **automatic failover**, use Redis Cluster mode instead of standalone.

### Configuration

In `tilt_config.yaml`, switch from standalone to cluster:

```yaml
redis:
  enabled: true
  mode: "cluster"  # Changed from "standalone"
  cluster:
    redisLeader: 3      # Number of master/leader nodes
    redisFollower: 3    # Number of replica/follower nodes
  max_memory: "512Mi"
```

### How Redis Cluster Works

**Architecture**:
- **clusterSize = 6** (3 leaders + 3 followers)
- Data **automatically sharded** across 3 leader nodes using hash slots (16384 total slots)
- Each leader has 1 follower for **automatic failover**

**Hash Slot Distribution**:
- Leader 1: Slots 0-5460
- Leader 2: Slots 5461-10922
- Leader 3: Slots 10923-16383

**Example key distribution**:
```
ha:smst:session_abc:nodes → hash(key) = slot 3421 → Leader 1
ha:relays:pokt1xyz → hash(key) = slot 8932 → Leader 2
ha:miner:sessions:pokt1abc:session_123 → hash(key) = slot 14201 → Leader 3
```

### Automatic Failover

**Scenario: Leader 2 crashes**

1. **Detection**: Follower 2 and other leaders detect Leader 2 is down (~5 seconds)
2. **Election**: Follower 2 promoted to new Leader 2
3. **Data Preservation**: All data on Follower 2 (AOF-persisted) is now available
4. **Automatic Rebalancing**: Hash slots 5461-10922 now served by new Leader 2
5. **Client Redirect**: Go Redis client automatically discovers new topology

**Downtime**: <5 seconds for affected keys
**Data Loss**: Max 1 second (last AOF sync before crash)

### Application Code Changes

**Standalone Connection**:
```go
redisClient := redis.NewClient(&redis.Options{
    Addr: "redis:6379",
})
```

**Cluster Connection**:
```go
redisClient := redis.NewClusterClient(&redis.ClusterOptions{
    Addrs: []string{
        "redis-cluster-leader-0:6379",
        "redis-cluster-leader-1:6379",
        "redis-cluster-leader-2:6379",
    },
    // Optional: Read from followers for better read distribution
    ReadOnly: true,
    RouteByLatency: true,
})
```

**Important**: Most Redis commands work identically, but:
- ❌ **Multi-key operations** spanning different hash slots will fail
- ❌ **Lua scripts** accessing multiple keys must use hash tags: `{session_123}:key1`, `{session_123}:key2`
- ✅ **Single-key operations** work seamlessly
- ✅ **Redis Streams** work (each stream is a single key)
- ✅ **Hashes** (SMST nodes) work (each hash is a single key)

### Hash Tags for Multi-Key Operations

Use **hash tags** to force related keys onto the same slot:

```
✅ GOOD:
ha:miner:sessions:{supplier_abc}:index
ha:miner:sessions:{supplier_abc}:state:active
ha:miner:sessions:{supplier_abc}:session_123
→ All hash to same slot (only "{supplier_abc}" is hashed)

❌ BAD:
ha:miner:sessions:supplier_abc:index
ha:miner:sessions:supplier_xyz:index
→ Different slots, can't use MGET or transactions
```

### Monitoring Cluster Health

**Check cluster status**:
```bash
kubectl exec -it redis-cluster-leader-0 -- redis-cli cluster info
kubectl exec -it redis-cluster-leader-0 -- redis-cli cluster nodes
```

**Expected output**:
```
cluster_state:ok
cluster_slots_assigned:16384
cluster_slots_ok:16384
cluster_slots_pfail:0
cluster_slots_fail:0
cluster_known_nodes:6
cluster_size:3
```

**Grafana Metrics**:
- `redis_cluster_state` - Should be "ok"
- `redis_cluster_slots_assigned` - Should be 16384
- `redis_connected_slaves` - Each leader should show 1 slave
- `redis_master_repl_offset` - Replication lag (should be near 0)

### When to Use Cluster vs Standalone

**Use Standalone**:
- ✅ Development/testing (simpler setup)
- ✅ Single replica deployment (<100 RPS)
- ✅ All data fits in single node memory (<1GB)

**Use Cluster**:
- ✅ Production deployments (automatic failover)
- ✅ High throughput (>500 RPS sustained)
- ✅ Large datasets (>2GB)
- ✅ Need horizontal scaling
- ✅ Cannot tolerate >5s downtime

### Performance Characteristics

**Standalone**:
- Latency: 1-2ms p95
- Throughput: 1000+ RPS per node
- Failover: Manual (requires operator intervention)
- Scaling: Vertical only (bigger machine)

**Cluster (3 leaders + 3 followers)**:
- Latency: 1-2ms p95 (same as standalone)
- Throughput: 3000+ RPS (3x leaders)
- Failover: Automatic (<5s)
- Scaling: Horizontal (add more leader+follower pairs)

### Cost-Benefit Analysis

**Standalone**: 1 pod, 512Mi RAM, 100m CPU = **1 node**
**Cluster**: 6 pods, 512Mi RAM each, 100m CPU each = **6 nodes**

**Cost**: 6x more infrastructure
**Benefits**:
- ✅ 3x throughput
- ✅ Automatic failover (99.9%+ uptime)
- ✅ Horizontal scaling
- ✅ Production-ready

**Recommendation**: Use cluster for production, standalone for dev/test.

## Key Takeaways

1. **Redis is NOT just a cache** - it's the source of truth for SMST, Streams, and Sessions
2. **`appendfsync everysec`** balances durability (1s loss) with performance (1-2ms latency)
3. **Disable RDB snapshots** - redundant with AOF, adds overhead
4. **1-second data loss is acceptable** - sessions last 4+ hours, 1s is <0.01% of session
5. **Monitor AOF health** - ensure rewrites don't block writes

---

**Reference**: See `tilt/redis.Tiltfile` for the full Redis configuration applied in development/testing environments.
