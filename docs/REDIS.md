# Redis Architecture

Redis is the central state store for distributed coordination. **It's NOT just a cache** - it stores critical revenue-generating data.

**Topology**: 1 Redis shared by the relayers and miners of a deployment. v0.1.0
was tested on 1 relayer + 1 miner and on 2 relayers + 2 miners (the latter at
lower load), always against a standalone Redis; the load tests and the capacity
figures are from 1 relayer + 1 miner ([docs/benchmarks/](benchmarks/README.md)).

**Version and memory**: Redis 8.10 or newer, with `maxmemory` set and
`maxmemory-policy noeviction`. Both binaries refuse to start against an older
Redis, a `maxmemory` of 0 or an evicting policy. The server settings the
release was measured with are
[config.redis.example.conf](../config.redis.example.conf); start from it.

## Configuration

### Connection Settings

```yaml
redis:
  url: "redis://localhost:6379"
  # pool_size: miner default 50; the relayer sizes its own from its worker pools
  # min_idle_conns: default pool_size / 4
  # pool_timeout_seconds: default 6
  # conn_max_idle_time_seconds: default 30 minutes
```

[config.relayer.example.yaml](../config.relayer.example.yaml) and
[config.miner.example.yaml](../config.miner.example.yaml) document each key and
its default.

### Pool Size (miner)

Each supplier's stream consumer holds 1 pooled connection while its read
blocks. A read blocks for at most one block interval, then returns and is
issued again, so the connection is held almost continuously.

Suppliers come from the keys the miner loads: a keys file or a keyring
([SUPPLIER_KEYS.md](SUPPLIER_KEYS.md)).

```
pool_size = numSuppliers + 20 overhead
```

**Breakdown of connections:**

| Type | Connections | Duration |
|------|-------------|----------|
| Stream consumer (per supplier) | 1 × numSuppliers | Held while the read blocks (one block interval per read) |
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

The base prefix stays configurable, and it must be ONE flat segment, enforced
at startup: it must not contain `:`, whitespace, or the glob characters
`* ? [ ]`. That rule is what makes two base prefixes two disjoint keyspaces, so
one Redis can host several fleets. Position alone does not give you that: a
colon-nested base would not be disjoint at all, because a fleet based at `ha`
scans `ha:*`, which matches every key of a fleet based at `ha:prod`, and that
pattern is what `redis flush --all` deletes. A glob character is rejected for
the same family of reason — it would end up inside every SCAN pattern the key
builder produces.

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
kb.SMSTNodesKey(supplier, sessionID)      // ha:smst:{supplier}:{sessionID}:nodes
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
| `ha:smst:{supplier}:{sessionID}:nodes`       | Hash   | SMST tree nodes for proof generation |
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
pocket-relay-miner redis keys --pattern "ha:suppliers:pokt1*" --stats   # look first
pocket-relay-miner redis flush --pattern "ha:suppliers:pokt1*"          # asks to confirm
```

The `pokt1` is load-bearing, not decoration. `ha:suppliers:index` is a sibling of
those entries and **`ha:suppliers:*` matches it**, so the shorter pattern deletes
the fleet index along with the orphans — and the balance monitor and
orphan-stream detection then see no suppliers at all until a miner restarts and
repopulates it. Every address begins with `pokt1`; the index does not.

Note that `redis cache --type all --invalidate --all` never touches `ha:suppliers:*` by design
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

[config.redis.example.conf](../config.redis.example.conf) uses RDB snapshots and
no AOF, which is what the v0.1.0 capacity report measured:

```
save ""
save 900 1 300 10
appendonly no
maxmemory-policy noeviction
```

With RDB only, a Redis crash loses what was written since the last snapshot.
AOF (`appendonly yes`, `appendfsync everysec`) narrows that to about 1 second
at the cost of extra writes; it was not measured for this release.

Keep `maxmemory` below the memory Redis may use, with headroom for the snapshot
fork and the allocator: the report ran `maxmemory` 12.8 GiB in a 16 GiB
container.

---

## Performance Tuning

The server settings (I/O threads, lazy freeing and the rest) are in
[config.redis.example.conf](../config.redis.example.conf), each with its reason;
it is the configuration the release was measured with. Settings it does not
carry, such as active defragmentation or a different `hz`, were not measured.

---

## Standalone, Sentinel and Cluster

The URL scheme selects the client: `redis://` or `rediss://` for a single
Redis, `redis-sentinel://` for Sentinel, `redis-cluster://` for Cluster (see the
`redis.url` comment in the example configs). v0.1.0 was tested and measured
only against a standalone Redis; Sentinel and Cluster are accepted by the
client but not verified. The keys carry no `{...}` hash tags except the
miner's rebroadcast keys (`ha:miner:rebroadcast:{claim}:...`).

---

## Monitoring

### Key Metrics

The miner samples Redis and publishes, among others:

```promql
ha_miner_redis_used_memory_bytes
ha_miner_redis_max_memory_bytes
ha_miner_redis_memory_usage_ratio
```

[METRICS_TRIAGE.md](METRICS_TRIAGE.md) says which to read, in order. Series
such as `redis_memory_used_bytes` or `redis_commands_processed_total` come from
a separate redis_exporter, not from this repository.

### Health Check

```bash
redis-cli INFO persistence | grep rdb_last_bgsave_status
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
