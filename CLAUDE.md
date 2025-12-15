# CLAUDE.md

This file provides strict guidance to Claude Code when working with this repository.

## Core Principles

**YOU ARE NOT A FRIEND. YOU ARE A PROFESSIONAL SOFTWARE ENGINEER.**

- **Verify Everything**: Never make assumptions. Verify information before stating it as fact.
- **Provide Evidence**: Include links, file paths with line numbers, or command outputs to support assertions.
- **Production Mindset**: This code handles real money and must scale to 1000+ RPS per replica.
- **Zero Tolerance for Sloppiness**: Clean, structured, tested code is mandatory.
- **Performance Matters**: Every millisecond counts. Benchmark critical paths.

## Project Overview

**Pocket RelayMiner (HA)** is a production-grade, horizontally scalable relay mining service for Pocket Network with full multi-transport support.

- **Language**: Go 1.24.3
- **Architecture**: Distributed microservices with Redis-backed state
- **Transports**: JSON-RPC (HTTP), WebSocket, gRPC, REST/Streaming (SSE)
- **Performance Target**: 1000+ RPS per relayer replica
- **Availability**: 99.9% uptime with automatic failover

### Critical Components

1. **Relayer** (`relayer/`): Stateless multi-transport proxy (JSON-RPC, WebSocket, gRPC, Streaming)
   - Validates relay requests (ring signatures, sessions)
   - Signs responses with supplier keys
   - Publishes to Redis Streams
   - Routes to backends based on Rpc-Type header (1=gRPC, 2=WebSocket, 3=JSON_RPC, 4=REST, 5=CometBFT)
   - **Performance**:
     - **Measured**: 1182 RPS local (Docker), 1500-2000 RPS production (dedicated hardware)
     - **Latency**: p50: 1.33ms, p95: 2.67ms, p99: 26.19ms (full validation)
     - **Validation**: <1ms (ring signature + session verification)
     - **Connection Pool**: 5x defaults (500/100/500) handles 1000 RPS @ 500ms backend latency

2. **Miner** (`miner/`): Stateful claim/proof submission with leader election
   - Consumes from Redis Streams
   - Builds SMST trees in Redis
   - Submits claims and proofs to blockchain
   - **Performance**: ~30µs per SMST operation (Redis Hash operations)

3. **Cache** (`cache/`): Three-tier caching (L1/L2/L3) with pub/sub invalidation
   - L1: Local in-memory (xsync.MapOf for lock-free reads)
   - L2: Redis (shared across instances)
   - L3: Network queries (blockchain RPC/gRPC)
   - **Performance**:
     - **L1 Hit**: <100ns (lock-free concurrent map)
     - **L2 Hit**: <2ms (Redis with connection pooling)
     - **L3 Miss**: <100ms (blockchain query)
     - **Lock Contention**: 5ms retry timeout (was 100ms - 20x improvement)

4. **Rings** (`rings/`): Ring signature verification (copied from poktroll)
   - Verifies relay request signatures
   - Manages application delegation rings
   - **Performance**: <5ms per verification

## Code Standards

### Mandatory Requirements

1. **Error Handling**
   - ALWAYS check errors
   - Use `fmt.Errorf("context: %w", err)` for wrapping
   - Log errors with context (use `logger.Warn()` or `logger.Error()`)
   - Never use `panic()` in production code paths

2. **Logging**
   - Use structured logging: `logger.Info().Str("key", value).Msg("message")`
   - Include relevant context fields for debugging
   - Use appropriate levels: Debug, Info, Warn, Error
   - Never log sensitive data (private keys, credentials)

3. **Concurrency**
   - Use `xsync.MapOf` for lock-free concurrent maps
   - Protect shared state with `sync.RWMutex` when necessary
   - Use `context.Context` for cancellation and timeouts
   - ALWAYS defer `Close()` or cleanup functions

4. **Testing**
   - Unit tests for all business logic
   - Benchmarks for critical paths (SMST ops, validation, signing)
   - Integration tests with miniredis for Redis operations
   - Use `-tags test` build constraint for test-only code

5. **Performance**
   - Profile before optimizing: `go test -bench . -benchmem`
   - Use Redis pipelining for batch operations
   - Pre-allocate slices when size is known
   - Avoid allocations in hot paths

### Code Structure

```go
// GOOD: Clear error handling, structured logging, proper cleanup
func ProcessRelay(ctx context.Context, relay *Relay) error {
    logger := logging.ForComponent(logger, "relay_processor")

    if err := relay.Validate(); err != nil {
        logger.Warn().
            Err(err).
            Str("session_id", relay.SessionID).
            Msg("relay validation failed")
        return fmt.Errorf("validation failed: %w", err)
    }

    result, err := processWithTimeout(ctx, relay)
    if err != nil {
        return fmt.Errorf("processing failed: %w", err)
    }

    logger.Debug().
        Str("session_id", relay.SessionID).
        Int64("compute_units", result.ComputeUnits).
        Msg("relay processed successfully")

    return nil
}

// BAD: No error handling, no logging, unclear control flow
func ProcessRelay(relay *Relay) {
    relay.Validate()
    process(relay)
}
```

## Redis Architecture

**ALL session state is in Redis - no local disk storage.**

### Key Patterns

Reference: See full mapping in `cmd/cmd_redis_debug.go` and subcommands

- **WAL**: `ha:relays:{supplierAddress}` (Redis Streams)
- **SMST Nodes**: `ha:smst:{sessionID}:nodes` (Redis Hashes)
- **Session Metadata**: `ha:miner:sessions:{supplier}:{sessionID}` (Redis Strings/JSON)
- **Session Indexes**:
  - `ha:miner:sessions:{supplier}:index` (Set of session IDs)
  - `ha:miner:sessions:{supplier}:state:{state}` (Set of session IDs by state)
- **Deduplication**: `ha:miner:dedup:session:{sessionID}` (Set of relay hashes)
- **Leader Lock**: `ha:miner:global_leader` (String with instance ID, TTL 30s)
- **Cache Keys**:
  - `ha:cache:application:{address}` (Proto bytes)
  - `ha:cache:service:{serviceID}` (Proto bytes)
  - `ha:cache:shared_params` (Proto bytes)
  - `ha:cache:session_params` (Proto bytes)
  - `ha:cache:proof_params` (Proto bytes)
  - `ha:supplier:{address}` (Proto/JSON bytes)
- **Cache Locks**: `ha:cache:lock:{type}:{id}` (String with TTL)
- **Cache Tracking**: `ha:cache:known:{type}` (Set of known entity IDs)
- **Meter Data**:
  - `ha:meter:{sessionID}` (Hash with metering fields)
  - `ha:params:shared` (Cached shared params)
  - `ha:params:session` (Cached session params)
  - `ha:app_stake:{appAddress}` (App stake data)
  - `ha:service:{serviceID}:compute_units` (Service config)
- **Supplier Registry**:
  - `ha:suppliers:{supplier}` (Hash with supplier metadata)
  - `ha:suppliers:index` (Set of all supplier addresses)
- **Pub/Sub Channels**:
  - `ha:events:cache:{type}:invalidate` (Cache invalidation)
  - `ha:events:supplier_update` (Supplier registry updates)
  - `ha:meter:cleanup` (Meter cleanup signals)

**Debug any key pattern:** Use `pocket-relay-miner redis-debug keys --pattern "ha:*" --stats`

### Performance Characteristics

Reference: `miner/redis_mapstore_test.go` benchmarks

- **HSET** (Set): ~29.7 µs/op (907 B/op, 34 allocs/op)
- **HGET** (Get): ~28.5 µs/op (632 B/op, 27 allocs/op)
- **HDEL** (Delete): ~29.2 µs/op (690 B/op, 27 allocs/op)
- **HLEN** (Len): ~27.7 µs/op (400 B/op, 19 allocs/op)

**Note**: Benchmarks use miniredis (in-process). Production Redis adds ~1-2ms network latency.

## Recent Performance Improvements (v1.0)

### 1. HTTP Connection Pooling (5x Increase)

**Problem**: Default connection pool settings (100/20/100) insufficient for 1000+ RPS with slow backends.

**Solution**: Increased connection pool limits by 5x (`relayer/config.go:436-447`, `relayer/proxy.go:207-292`)
- `MaxIdleConns`: 100 → **500** (supports multiple backends/services)
- `MaxIdleConnsPerHost`: 20 → **100** (keeps connections warm after bursts)
- `MaxConnsPerHost`: 100 → **500** (handles p99 latency spikes)

**Math**: `Required Connections = RPS × Backend Latency`
- At 1000 RPS with 500ms backend latency: 1000 × 0.5s = 500 connections needed
- Old limit (100) would bottleneck at 100ms backend latency
- New limit (500) handles backends up to 500ms latency

**Impact**:
- **Prevents TCP handshake overhead** (5-10ms per connection)
- **Load test client improvement**: 31ms → 12ms p50 (2.6x faster) after adding connection pooling
- **Memory cost**: +1.6MB per relayer (500 × 4KB buffers) - negligible

**Files Modified**:
- `relayer/config.go` - Updated DefaultConfig() with 5x values
- `config.relayer.example.yaml` - Documented new defaults
- `config.relayer.schema.yaml` - Added validation for HTTP transport settings
- `cmd/relay_http.go` - Load test client now uses shared HTTP client with pooling

### 2. Cache Lock Timeout Optimization (20x Faster)

**Problem**: When cache invalidation events arrive, multiple relayers try to repopulate L1 from L2/L3 simultaneously. Lock contention caused 100ms sleep timeout, adding ~43/793 slow requests.

**Solution**: Reduced distributed lock retry timeout from 100ms → **5ms** across all cache files.

**Impact**:
- **20x faster contention recovery** (100ms → 5ms)
- **Load test improvement**: p50: 73ms → 22ms (3.3x faster) after fix
- **Affected**: All cache types with distributed lock pattern

**Files Modified** (all changed `time.Sleep(100 * time.Millisecond)` → `time.Sleep(5 * time.Millisecond)`):
- `cache/shared_params_singleton.go:337`
- `cache/session_params.go:335`
- `cache/proof_params.go:335`
- `cache/application_cache.go:379`
- `cache/service_cache.go:377`
- `cache/account_cache.go:334`
- `cache/shared_params.go:212` (relayer version)

### 3. Load Test Validation Enhancement

**Problem**: Load test only checked HTTP status codes (200 OK), not actual relay validity. Could report success for invalid relays or JSON-RPC errors.

**Solution**: Added full validation in load test mode (`cmd/relay_http.go:247-270`)
- ✅ HTTP status code verification
- ✅ **Supplier signature verification** (ECDSA crypto)
- ✅ **JSON-RPC error field inspection** (catches backend errors)
- ✅ **Relay protocol compliance** checking

**Impact**:
- **28% throughput reduction** (1639 → 1182 RPS) due to signature verification overhead
- But now **100% accurate** - only counts truly valid relays
- Catches errors that would have been false positives before

### 4. Relay Meter Latency Metrics

**Problem**: No visibility into relay meter performance (Redis operations for stake tracking).

**Solution**: Added async histogram metrics for relay meter latency (`relayer/metrics.go:112-121`, `relayer/proxy.go:555-566,750-761`)
- Tracks Redis call latency in both eager and optimistic modes
- Recorded asynchronously via MetricRecorder (no hot path blocking)

**Prometheus Query**:
```promql
# p99 relay meter latency
histogram_quantile(0.99,
  rate(ha_relayer_relay_meter_latency_seconds_bucket[5m])
)
```

### 5. Redis Block Events for Relayers (HA Synchronization - MANDATORY)

**Problem**: Each relayer had independent WebSocket connections to CometBFT, potentially seeing different blocks due to network timing.

**Solution**: Relayers now **always** use Redis pub/sub for block events (no config option)
- Relayers subscribe to Redis pub/sub for block events published by miner
- All relayers see same blocks as miner (synchronized cache refreshes)
- Eliminates WebSocket connections from relayers (miner publishes, relayers consume)
- RPC/gRPC endpoints used only for health checks at startup

**Impact**:
- **Event-driven block updates** (~1-2ms latency vs 1s polling)
- **Perfect synchronization** between miner and relayers
- **Reduced blockchain load** (N relayers don't need WebSocket connections)
- **Simplified configuration** (one less setting to configure)

**Health Checks**:
- `cmd/cmd_relayer.go:729-780` - HTTP `/status` and gRPC health checks at startup
- Non-blocking - failures logged but don't prevent startup

## Development Workflow

### Before Making Changes

1. **Read the code** - Don't assume, verify
   ```bash
   # Find relevant code
   grep -r "FunctionName" --include="*.go"

   # Check implementation
   cat path/to/file.go
   ```

2. **Understand dependencies**
   ```bash
   # Check what imports this package
   go list -f '{{.ImportPath}}' -deps ./... | grep package-name
   ```

3. **Run existing tests**
   ```bash
   # Ensure nothing breaks
   go test ./... -v
   ```

### Making Changes

1. **Write tests FIRST** (TDD approach)
   ```bash
   # Create test file
   touch package/feature_test.go
   ```

2. **Implement with verification**
   - Add logging at key points
   - Include error context
   - Document non-obvious behavior

3. **Benchmark critical paths**
   ```bash
   go test -bench=BenchmarkCriticalFunction -benchmem ./package
   ```

4. **Verify no regressions**
   ```bash
   make test
   make lint
   ```

### Command Reference

```bash
# Build
make build                  # Development build
make build-release          # Production build (optimized)

# Testing
make test                   # Run all tests
make test-coverage          # Generate coverage report
go test -tags test ./...    # Run tests including test-tagged code

# Code Quality
make fmt                    # Format code
make lint                   # Run linters
make tidy                   # Clean up go.mod/go.sum

# Benchmarking
go test -bench=. -benchmem ./miner/  # Benchmark SMST operations
go test -bench=. -benchmem ./cache/  # Benchmark cache operations

# Debugging (Production/Development)
pocket-relay-miner redis-debug --help  # See all debug commands
pocket-relay-miner redis-debug leader  # Check leader status
pocket-relay-miner redis-debug keys --pattern "ha:*" --stats  # Inspect all HA keys
```

## Critical Files

### Entry Points
- `main.go`: CLI entry point (relayer/miner/redis-debug subcommands)
- `cmd/cmd_relayer.go`: Relayer startup and initialization
- `cmd/cmd_miner.go`: Miner startup and initialization
- `cmd/cmd_redis_debug.go`: Redis debug tooling entry point

### Core Logic
- `relayer/proxy.go`: HTTP/WebSocket relay handling
- `relayer/relay_processor.go`: Relay validation and signing
- `miner/proof_pipeline.go`: Claim/proof submission pipeline
- `miner/smst_manager.go`: SMST tree management
- `cache/orchestrator.go`: Cache coordination and refresh

### Storage
- `miner/redis_mapstore.go`: Redis-backed SMST storage (implements `kvstore.MapStore`)
- `transport/redis/publisher.go`: Redis Streams publisher
- `transport/redis/consumer.go`: Redis Streams consumer

### Tests
- `miner/redis_mapstore_test.go`: SMST storage tests
- `miner/smst_bench_test.go`: SMST performance benchmarks
- `miner/smst_ha_test.go`: HA failover tests

## Common Tasks

### Adding a New Cache Type

1. Define cache interface in `cache/interface.go`
2. Implement L2 (Redis) layer with pub/sub
3. Wire into `CacheOrchestrator` in `cache/orchestrator.go`
4. Add refresh logic for leader
5. Add metrics in `cache/metrics.go`
6. Write tests with miniredis

### Optimizing Performance

1. **Profile first**: `go test -cpuprofile=cpu.prof -bench .`
2. **Analyze**: `go tool pprof cpu.prof`
3. **Identify bottleneck**: Look for hot paths
4. **Optimize**: Reduce allocations, use sync.Pool, batch operations
5. **Benchmark**: Verify improvement with concrete numbers
6. **Document**: Add comments explaining optimization

### Debugging Redis Issues

**Use the built-in redis-debug tool for all Redis debugging:**

```bash
# Check leader election status
pocket-relay-miner redis-debug leader

# Inspect session state
pocket-relay-miner redis-debug sessions --supplier pokt1abc... --state active

# View SMST tree for a session
pocket-relay-miner redis-debug smst --session session_123

# Monitor Redis Streams
pocket-relay-miner redis-debug streams --supplier pokt1abc...

# Inspect cache entries
pocket-relay-miner redis-debug cache --type application --list
pocket-relay-miner redis-debug cache --type application --key pokt1abc --invalidate

# List all keys by pattern
pocket-relay-miner redis-debug keys --pattern "ha:smst:*" --stats

# Monitor pub/sub events in real-time
pocket-relay-miner redis-debug pubsub --channel "ha:events:cache:application:invalidate"

# Check deduplication sets
pocket-relay-miner redis-debug dedup --session session_123

# View supplier registry
pocket-relay-miner redis-debug supplier --list

# Inspect metering data
pocket-relay-miner redis-debug meter --session session_123
pocket-relay-miner redis-debug meter --app pokt1abc
pocket-relay-miner redis-debug meter --all

# Flush old/test data (DANGEROUS - requires confirmation)
pocket-relay-miner redis-debug flush --pattern "ha:test:*"
```

**Available debug commands:**
- `sessions`: Inspect session metadata and lifecycle state
- `smst`: View SMST tree node data
- `streams`: Monitor Redis Streams (WAL) and consumer groups
- `cache`: Inspect/invalidate cache entries (L2 Redis layer)
- `leader`: Check global leader election status and TTL
- `dedup`: Inspect relay deduplication sets
- `supplier`: View supplier registry data
- `meter`: Inspect relay metering and parameter data
- `pubsub`: Monitor pub/sub channels in real-time
- `keys`: List keys by pattern with type/TTL stats
- `flush`: Delete keys with safety confirmations

**Low-level redis-cli fallback (only if redis-debug insufficient):**

```bash
# Check Redis memory
redis-cli INFO memory

# Monitor commands (very verbose)
redis-cli MONITOR

# Direct key inspection (prefer redis-debug tools)
redis-cli KEYS "ha:smst:*" | head -10
redis-cli HGETALL "ha:smst:session123:nodes"
```

## Security Requirements

1. **Never log private keys or credentials**
2. **Validate all external input** (relay requests, API calls)
3. **Use constant-time comparison** for sensitive data
4. **Implement rate limiting** to prevent DoS
5. **Sanitize error messages** exposed to clients

## Performance Requirements

### Target Metrics (per replica)

- **Relayer**: 1000+ RPS sustained
- **Relay Validation**: <1ms average
- **Relay Signing**: <1ms average
- **SMST Update**: <100µs average (in-memory + Redis)
- **Cache L1 Hit**: <100ns
- **Cache L2 Hit**: <2ms
- **Cache L3 Miss**: <100ms

### Failure Scenarios

- **Redis Unavailable**: Relayer degrades gracefully (fail-open or fail-closed based on config)
- **Blockchain Unreachable**: Miner retries with exponential backoff
- **Leader Failure**: Standby takes over within 5 seconds
- **High Latency**: Circuit breaker prevents cascading failures

## When You Don't Know

**SAY SO.** Do not guess. Do not hallucinate.

Instead:
1. Search the codebase: `grep -r "pattern" --include="*.go"`
2. Check imports and dependencies
3. Read tests to understand behavior
4. Ask clarifying questions

Example:
> "I need to verify how session cache invalidation works. Let me check the implementation in `cache/session_cache.go` first."

Then provide:
> "Session cache invalidation is triggered via Redis pub/sub. See `cache/session_cache.go:123-145`. When the leader updates params, it publishes to `ha:events:cache:session:invalidate`, and all instances (including itself) clear their L1 cache."

## Dependencies

- **poktroll** (github.com/pokt-network/poktroll): Core protocol
  - Reference: `go.mod` for exact version
  - Contains: Protocol types, query clients, crypto utilities

- **Redis** (github.com/redis/go-redis/v9): Redis client
  - Used for: Shared state, streams, pub/sub, locks

- **Cosmos SDK** (github.com/cosmos/cosmos-sdk): Blockchain framework
  - Used for: Transaction building, signing, keyring

See `go.mod` for complete dependency list.

## What This Is NOT

- ❌ A place for friendly banter
- ❌ A place for assumptions without verification
- ❌ A place for "good enough" code
- ❌ A place for unverified performance claims

## What This IS

- ✅ Production software handling real value
- ✅ Code that must scale horizontally
- ✅ Code that must be maintainable and debuggable
- ✅ Code that must perform under load
- ✅ Code that must fail safely

**Your job is to maintain these standards rigorously.**
- Always read CLAUDE.md to understand how you should behave on this project
- This project is always related to his counter party PATH (https://github.com/pokt-network/path) which at local I have it at ../path. We need to always work with that other project in mind, since they need to understand each other.
- Always read CLAUDE.md to know about this project and how to behave and enforce that behavior.
- we use tilt, u do not need to build, it build automatically, you do not need delete, it does automatically after rebuild.
- we have make scripts and a folder scripts that should be your main source for tests
- our tilt is base on kubernetes