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
    base_prefix: "ha"           # The ONLY configurable segment
```

Everything below the base prefix is fixed in code (`transport/redis/namespace.go`),
which is what makes miner and relayer unable to drift apart on the key layout.

It also removes a whole class of hazard. While each family had its own knob, one
could be turned until it equalled another family's literal: `supplier_prefix:
"suppliers"` made the supplier state key and the fleet registry key the same
string, and `cache_prefix: "supplier"` made the supplier SCAN pattern
(`ha:supplier:*`) match every cache key — reachable from `redis cache --type
supplier --invalidate`, which deletes what it scans. With the layout constant,
each pattern provably matches only its own family, and a test pins it.

The base prefix stays configurable, and it must match `^[a-zA-Z0-9_-]+$` — ONE
flat segment, enforced at startup. That rule is what makes two base prefixes two
disjoint keyspaces, so one Redis can host several fleets. Position alone does not
give you that: a colon-nested base would not be disjoint at all, because a fleet
based at `ha` scans `ha:*`, which matches every key of a fleet based at
`ha:prod`, and that pattern is what `redis flush --all` deletes. A glob character
is rejected for the same family of reason — it would end up inside every SCAN
pattern the key builder produces.

If you want stronger isolation than a shared keyspace with distinct prefixes,
use a different Redis database or a different server. That isolates; a prefix
hierarchy only looks like it does.

**Upgrading from a config that set the per-family prefixes**: those keys would
move, so startup fails with a message naming each field rather than coming up
healthy against an empty keyspace. Setting one to its historical value is
accepted (nothing moves); anything else means draining the fleet and migrating
before upgrading.

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
kb.MeterMetaKey(sessionID, supplier)      // ha:meter:{sessionID}:{supplier}:meta
kb.MeterConsumedKey(sessionID, supplier)  // ha:meter:{sessionID}:{supplier}:consumed
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

---

## Supplier state: one entity key, one fleet set

Two prefixes differ by one letter, and they are not two views of the same thing:

- **`ha:supplier:{address}`** — singular, one entity. The replica of that
  supplier's on-chain state: staked, status, services, declared endpoints. The
  miner writes it every reconcile pass; the relayer reads it to decide whether
  to serve a relay. It carries a TTL of `2 × num_blocks_per_session × block_time`
  (~42 min on mainnet), which is the only thing that ever clears it, so a
  decommissioned supplier cannot freeze as "still active".
- **`ha:suppliers:index`** — plural, a set. The addresses THIS FLEET handles.
  Read by the balance monitor and by orphan-stream detection. Membership only:
  it says nothing about the supplier's state on the network.

The `redis supplier` subcommand reads the singular family, and the
`Last Updated` it shows is refreshed on every reconcile pass even when nothing
changed, so a stale timestamp means nothing is tracking that supplier — not that
it has not changed.

### Clearing the orphans left by versions before this one

Earlier versions also wrote a per-supplier JSON value at
`ha:suppliers:{address}`, **with no expiration**. Nothing read it, and it is no
longer written, but the entries already in Redis stay there forever. Clear them
once:

```bash
pocket-relay-miner redis keys --pattern "ha:suppliers:*" --stats   # look first
pocket-relay-miner redis flush --pattern "ha:suppliers:*"          # asks to confirm
```

`ha:suppliers:index` matches that pattern too, so **do not run the flush with
the fleet up** — deleting the index makes the balance monitor and orphan-stream
detection see no suppliers until a miner restarts and repopulates it. Either
delete each `ha:suppliers:{address}` individually, or do it with the fleet
stopped.

Note that `redis cache cleanup-all` never touches `ha:suppliers:*` by design
(`cmd/redis/cache_all.go`), so it will not clear these for you.



### Rebuildable Data (Optional Persist)

| Pattern                    | Type   | Purpose                     |
|----------------------------|--------|-----------------------------|
| `ha:cache:application:*`   | String | App cache (rebuild from L3) |
| `ha:cache:service:*`       | String | Service cache               |
| `ha:cache:*_params`        | String | Params cache                |
| `ha:supplier:{address}`    | String | Supplier state replica (TTL) |
| `ha:suppliers:index`       | Set    | Addresses this fleet handles |
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
