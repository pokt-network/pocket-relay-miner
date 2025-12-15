# Pocket RelayMiner (High Availability)

[![CI](https://github.com/pokt-network/pocket-relay-miner/actions/workflows/ci.yml/badge.svg)](https://github.com/pokt-network/pocket-relay-miner/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/pokt-network/pocket-relay-miner/branch/main/graph/badge.svg)](https://codecov.io/gh/pokt-network/pocket-relay-miner)
[![Go Report Card](https://goreportcard.com/badge/github.com/pokt-network/pocket-relay-miner)](https://goreportcard.com/report/github.com/pokt-network/pocket-relay-miner)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.24.3-blue.svg)](https://golang.org/dl/)

High-Availability RelayMiner implementation for Pocket Network - a production-grade, horizontally scalable relay mining service with full multi-transport support.

## Overview

The HA RelayMiner is a distributed relay mining system that enables horizontal scaling and automatic failover through Redis-based shared state. It separates concerns into two components:

- **Relayer**: Stateless multi-transport proxy (JSON-RPC, WebSocket, gRPC, Streaming) that validates and forwards relay requests (scales horizontally)
- **Miner**: Stateful claim/proof submission service with leader election (active-standby failover)

### Supported Transport Protocols

✅ **JSON-RPC (HTTP)** - Traditional HTTP POST with JSON-RPC payload
✅ **WebSocket** - Persistent bidirectional connections for real-time applications
✅ **gRPC** - High-performance binary protocol with streaming support
✅ **REST/Streaming** - Server-Sent Events (SSE) for streaming responses

All transport modes support:
- Ring signature validation for application authentication
- Supplier signature verification on responses
- Relay metering and rate limiting
- Session-based routing to backends

## Architecture

```
                     +----------------+
                     | Load Balancer  |
                     +----------------+
                            |
              +-------------+-------------+
              |             |             |
       +-----------+  +-----------+  +-----------+
       | Relayer#1 |  | Relayer#2 |  | Relayer#3 |
       +-----------+  +-----------+  +-----------+
              |             |             |
              +-------------+-------------+
                            |
                     +-------------+
                     | Redis (HA)  |
                     +-------------+
                            |
              +-------------+-------------+
              |                           |
       +-----------+               +-----------+
       | Miner     |               | Miner     |
       | (Leader)  |               | (Standby) |
       +-----------+               +-----------+
```

### Storage Architecture

**All session state is stored in Redis for cross-instance sharing:**

- **WAL (Write-Ahead Log)**: Redis Streams for durability and recovery
- **SMST (Sparse Merkle Sum Tree)**: Redis Hashes for merkle tree node storage
- **Session Metadata**: Redis Hashes for session state snapshots

**Key Benefits:**
- No local disk I/O bottlenecks
- Instant failover (standby can take over immediately)
- Horizontal scaling without state synchronization
- O(1) operations for Get/Set/Delete

## Performance Characteristics

### Measured Performance (v1.0)

**Local Development (Docker-in-Docker):**
- **Throughput**: 1182 RPS with full validation (signature + JSON-RPC error checking)
- **Latency**:
  - p50: 1.33ms
  - p95: 2.67ms
  - p99: 26.19ms
- **Success Rate**: 100% (1000/1000 relays validated)

**Production Expectations (Dedicated Hardware):**
- **Per Relayer**: 1500-2000 RPS sustained
- **3 Replicas**: ~4500-6000 RPS total
- **10 Replicas**: ~15K-20K RPS (linear horizontal scaling)

### Component Performance

- **SMST Operations**: ~30µs per operation (Redis Hashes)
- **Relay Validation**: <1ms (ring signature + session verification)
- **Cache L1 Hit**: <100ns (lock-free concurrent map)
- **Cache L2 Hit**: <2ms (Redis with connection pooling)
- **Failover Time**: <5 seconds (leader election + state recovery)

### Connection Pooling (5x Defaults)

The relayer uses aggressive connection pooling to support high throughput:
- **Max Connections per Backend**: 500 (handles 1000 RPS @ 500ms backend latency)
- **Idle Connections per Backend**: 100 (keeps connections warm after traffic bursts)
- **Total Idle Connections**: 500 (supports multiple backends and services)

This prevents TCP handshake overhead and ensures consistent sub-millisecond response times even under heavy load.

## Requirements

- **Go**: 1.24.3+
- **Redis**: 6.2+ (Standalone, Sentinel, or Cluster mode)
- **Network**: Access to Pocket Network Shannon RPC/gRPC endpoints
- **Poktroll**: v0.1.31+ (dependency)

## Installation

### Build from Source

```bash
# Clone the repository
git clone https://github.com/pokt-network/pocket-relay-miner.git
cd pocket-relay-miner

# Build the binary
make build

# Or build optimized release version
make build-release
```

### Binary Location

- Development build: `./pocket-relay-miner`
- Release build: `bin/pocket-relay-miner`

### Build Backend Test Server

The backend test server is a generic multi-transport server for testing the relay miner:

```bash
# Build backend server (includes protobuf generation)
make build-backend

# Or manually
cd tilt/backend-server
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pb/demo.proto
go build -o backend main.go
```

**Backend server location:** `tilt/backend-server/backend`

**Supported transports:**
- HTTP (JSON-RPC at `/`)
- WebSocket (at `/ws`)
- gRPC (port 50051)
- SSE streaming (at `/stream/sse`)
- NDJSON streaming (at `/stream/ndjson`)

**Test features:**
- Configurable error injection (error_rate, error_code)
- Configurable response delay (delay_ms)
- Subscription simulation (repeat_count, delay_ms in params)
- Health check endpoint (`/health`)

## Configuration

### Relayer Configuration

See `config.relayer.example.yaml` for complete example configuration.

**Key settings:**
- `listen_addr`: HTTP server bind address (e.g., `0.0.0.0:8080`)
- `redis.url`: Redis connection string (e.g., `redis://localhost:6379`)
- `pocket_node.query_node_grpc_url`: Pocket node gRPC endpoint (primary query interface)
- `pocket_node.query_node_rpc_url`: Pocket node RPC endpoint (health checks only)
- `services`: Backend service configurations per service ID and RPC type
- `default_validation_mode`: "eager" (validate first) or "optimistic" (serve first, validate async)

**HTTP Transport Settings (Connection Pooling):**

The relayer uses aggressive connection pooling to support 1000+ RPS sustained throughput:

```yaml
http_transport:
  max_idle_conns: 500                # Total idle connections across all backends
  max_idle_conns_per_host: 100       # Idle connections per backend (keeps warm after bursts)
  max_conns_per_host: 500            # Total concurrent connections per backend
  idle_conn_timeout_seconds: 90      # How long to keep idle connections alive
  dial_timeout_seconds: 5            # Timeout for establishing new connection
  tls_handshake_timeout_seconds: 10  # Timeout for TLS handshake
  response_header_timeout_seconds: 30  # Timeout waiting for response headers
  tcp_keep_alive_seconds: 30         # TCP keepalive period
  disable_compression: true          # Don't modify content encoding (required)
```

**Why 5x defaults?**
- Handles 1000 RPS with backend latency up to 500ms (`RPS × latency = 1000 × 0.5s = 500 connections`)
- Keeps connections warm after traffic bursts (prevents TCP handshake overhead)
- Supports multiple backends and services simultaneously
- Memory cost: ~2MB per relayer (500 × 4KB buffers) - negligible

**When to adjust:**
- **Fast backends (<50ms)**: Can reduce to 100/50/100 (saves memory)
- **Slow backends (>200ms)**: May need to increase for high RPS
- **Multiple services**: Increase `max_idle_conns` to `services × 100`

### Miner Configuration

See `config.miner.example.yaml` for complete example configuration.

**Key settings:**
- `redis.url`: Redis connection string (must match relayer)
- `redis.consumer_group`: Consumer group name (e.g., `ha-miners`)
- `redis.consumer_name`: Unique consumer identifier per replica
- `pocket_node.query_node_grpc_url`: Pocket node gRPC endpoint
- `pocket_node.tx_node_rpc_url`: Pocket node RPC endpoint for submitting transactions
- `keys.keys_file`: Path to supplier keys YAML file
- `known_applications`: Application addresses to pre-warm in cache
- `leader_election.leader_ttl_seconds`: Leader lock TTL in seconds (default: 30)
- `leader_election.heartbeat_rate_seconds`: Leadership acquisition/renewal rate in seconds (default: 10)

## Usage

### Start Relayer

```bash
pocket-relay-miner relayer --config /path/to/relayer-config.yaml
```

**Command flags:**
- `--config`: Path to relayer config file (required)
- `--redis-url`: Override Redis URL from config

### Start Miner

```bash
pocket-relay-miner miner --config /path/to/miner-config.yaml
```

**Command flags:**
- `--config`: Path to miner config file (required)
- `--redis-url`: Override Redis URL from config
- `--consumer-group`: Override consumer group name
- `--consumer-name`: Override consumer name (defaults to hostname)

### Leader Election

Only one miner instance will be active (leader) at any time. The leader:
- Refreshes shared caches (params, services, applications)
- Processes relays from Redis Streams
- Builds SMST trees and submits claims/proofs

Standby instances automatically take over if the leader fails.

**Configuration:**
```yaml
leader_election:
  # How long the leader lock lasts before expiring (seconds)
  # Default: 30
  leader_ttl_seconds: 30

  # How often to attempt acquire/renew leadership (seconds)
  # Should be less than leader_ttl_seconds to ensure renewal before expiration
  # Default: 10
  heartbeat_rate_seconds: 10
```

**Production recommendations:**
- `leader_ttl_seconds`: 30 (allows 3 heartbeat failures before expiration)
- `heartbeat_rate_seconds`: 10 (renews 3x before TTL expires)

**Testing/development:**
- Use faster values for quicker failover (e.g., TTL: 3s, Heartbeat: 1s)

### Session Lifecycle Configuration

The session lifecycle manager monitors sessions and triggers state transitions (active → claiming → claimed → proving → settled). Configure how often to check for transitions:

```yaml
session_lifecycle:
  # How often to check for session state transitions (seconds)
  # This controls the polling frequency for detecting when sessions need to transition
  # Default: 30
  check_interval_seconds: 30

  # Blocks before claim window close to start claiming
  # Provides buffer time for transaction confirmation
  # Default: 2
  claim_submission_buffer: 2

  # Blocks before proof window close to start proving
  # Default: 2
  proof_submission_buffer: 2

  # Maximum number of sessions transitioning concurrently
  # Default: 10
  max_concurrent_transitions: 10
```

**Production recommendations:**
- `check_interval_seconds`: 30 (balance between responsiveness and resource usage)
- `claim_submission_buffer`: 2 (ensures claims submitted before window closes)
- `proof_submission_buffer`: 2 (ensures proofs submitted before window closes)

**Testing/development:**
- Use faster check intervals for quicker state transitions (e.g., 1-5 seconds)

## CLI Commands

The `pocket-relay-miner` binary provides several commands for running services and debugging:

### Available Commands

```
pocket-relay-miner <command> [flags]

Commands:
  relayer       Start the relayer service (stateless proxy)
  miner         Start the miner service (claim/proof submitter)
  relay         Test relay requests (single or load test mode)
  redis-debug   Debug Redis state and HA components
  version       Display version information
  help          Show help for any command
```

### redis-debug: Debugging Tool

The `redis-debug` command provides powerful tools for inspecting and troubleshooting Redis-backed HA state:

**Check Leader Status:**
```bash
pocket-relay-miner redis-debug leader
# Output: Shows current leader instance ID, TTL, and election status
```

**Inspect Sessions:**
```bash
# List all sessions for a supplier
pocket-relay-miner redis-debug sessions --supplier pokt1abc...

# Filter by state
pocket-relay-miner redis-debug sessions --supplier pokt1abc... --state active

# View specific session details
pocket-relay-miner redis-debug sessions --supplier pokt1abc... --session session_123
```

**Inspect SMST Trees:**
```bash
# View SMST tree nodes for a session
pocket-relay-miner redis-debug smst --session session_123

# Check node count and structure
pocket-relay-miner redis-debug smst --session session_123 --stats
```

**Monitor Redis Streams (WAL):**
```bash
# View stream info and pending messages
pocket-relay-miner redis-debug streams --supplier pokt1abc...

# Monitor consumer group lag
pocket-relay-miner redis-debug streams --supplier pokt1abc... --group ha-miners
```

**Inspect Caches:**
```bash
# List all cached applications
pocket-relay-miner redis-debug cache --type application --list

# View specific cache entry
pocket-relay-miner redis-debug cache --type application --key pokt1abc...

# Invalidate cache entry (force refresh)
pocket-relay-miner redis-debug cache --type application --key pokt1abc... --invalidate
```

**Monitor Pub/Sub Events:**
```bash
# Watch cache invalidation events in real-time
pocket-relay-miner redis-debug pubsub --channel "ha:events:cache:application:invalidate"

# Monitor all HA events
pocket-relay-miner redis-debug pubsub --channel "ha:events:*"
```

**List Redis Keys:**
```bash
# List all HA keys with statistics
pocket-relay-miner redis-debug keys --pattern "ha:*" --stats

# Find specific key patterns
pocket-relay-miner redis-debug keys --pattern "ha:smst:*"
```

**Inspect Relay Meter Data:**
```bash
# View session meter data
pocket-relay-miner redis-debug meter --session session_123

# View app consumption data
pocket-relay-miner redis-debug meter --app pokt1abc...

# View all active meters
pocket-relay-miner redis-debug meter --all
```

**Inspect Deduplication:**
```bash
# View deduplication set for a session
pocket-relay-miner redis-debug dedup --session session_123
```

**Inspect Supplier Registry:**
```bash
# List all registered suppliers
pocket-relay-miner redis-debug supplier --list

# View specific supplier data
pocket-relay-miner redis-debug supplier --address pokt1abc...
```

**Cleanup Operations:**
```bash
# Delete keys matching pattern (requires confirmation)
pocket-relay-miner redis-debug flush --pattern "ha:test:*"

# Force delete without confirmation (DANGEROUS)
pocket-relay-miner redis-debug flush --pattern "ha:test:*" --force
```

### Relay Testing Tool

Test all supported transport protocols with signature verification:

```bash
# Test JSON-RPC (HTTP) relay
pocket-relay-miner relay jsonrpc \
  --app-priv-key <hex> \
  --service <serviceID> \
  --node <grpc_endpoint> \
  --chain-id <chainID> \
  --relayer-url <relayer_url> \
  --supplier <supplier_address>

# Test WebSocket relay
pocket-relay-miner relay websocket \
  --app-priv-key <hex> \
  --service <serviceID> \
  --node <grpc_endpoint> \
  --chain-id <chainID> \
  --relayer-url <relayer_url> \
  --supplier <supplier_address>

# Test gRPC relay
pocket-relay-miner relay grpc \
  --app-priv-key <hex> \
  --service <serviceID> \
  --node <grpc_endpoint> \
  --chain-id <chainID> \
  --relayer-url <relayer_url> \
  --supplier <supplier_address>

# Test streaming relay (SSE)
pocket-relay-miner relay stream \
  --app-priv-key <hex> \
  --service <serviceID> \
  --node <grpc_endpoint> \
  --chain-id <chainID> \
  --relayer-url <relayer_url> \
  --supplier <supplier_address>

# Load testing mode (concurrent requests)
pocket-relay-miner relay jsonrpc \
  --load-test \
  --count 1000 \
  --concurrency 50 \
  <...other flags>
```

**Features:**
- Signature verification for all relay responses
- Detailed timing breakdowns (build, network, verify)
- Load testing with configurable concurrency
- Custom JSON-RPC payload support
- Session-based relay construction

### Redis Debug Tools

Debug and inspect Redis data structures used by the RelayMiner:

```bash
# Check leader election status
pocket-relay-miner redis-debug leader --redis redis://localhost:6379

# Inspect session metadata for a supplier
pocket-relay-miner redis-debug sessions --supplier pokt1abc... --state active

# View SMST tree for a session
pocket-relay-miner redis-debug smst --session session_123

# Monitor Redis Streams
pocket-relay-miner redis-debug streams --supplier pokt1abc...

# Inspect cache entries
pocket-relay-miner redis-debug cache --type application --list

# List all keys matching pattern
pocket-relay-miner redis-debug keys --pattern "ha:smst:*" --stats

# Monitor pub/sub events
pocket-relay-miner redis-debug pubsub --channel "ha:events:cache:application:invalidate"

# Flush data with safety confirmations (DANGEROUS)
pocket-relay-miner redis-debug flush --pattern "ha:smst:old_session_*"
```

**Available debug commands:**
- `sessions`: Inspect session metadata and state
- `smst`: View SMST tree data for sessions
- `streams`: Monitor Redis Streams (WAL)
- `cache`: Inspect/invalidate cache entries
- `leader`: Check leader election status
- `dedup`: Inspect deduplication sets
- `supplier`: View supplier registry
- `meter`: Inspect relay metering data
- `pubsub`: Monitor pub/sub channels in real-time
- `keys`: List keys by pattern with stats
- `flush`: Delete keys with safety confirmations

All debug commands support `--redis` flag to specify Redis URL (default: `redis://localhost:6379`).

## Development

### Running Tests

```bash
# Run all tests
make test

# Run tests with coverage
make test-coverage

# Run HA-specific tests
go test -tags test ./miner/... -v
```

### Code Quality

```bash
# Format code
make fmt

# Run linters
make lint

# Tidy dependencies
make tidy
```

## Production Deployment

### Redis Capacity Planning

- **Memory per relay**: ~500 bytes (SMST node + metadata)
- **Memory per session**: ~500 bytes × relays_per_session
- **Example**: 1000 relays × 100 active sessions = ~50 MB
- **Recommendation**: Size Redis memory at 2x expected usage

### High Availability Setup

**Redis:**
- Use Redis Sentinel (3+ nodes) for automatic failover
- Or Redis Cluster (6+ nodes) for sharding
- Configure connection retry with exponential backoff
- Monitor replication lag

**Relayer:**
- Deploy 3+ instances behind a load balancer
- Use health checks at `/health` endpoint
- Configure resource limits (CPU/memory)

**Miner:**
- Deploy 2+ instances (leader + standbys)
- Ensure network connectivity to Redis and Pocket node
- Monitor leader election state

### Monitoring

**Metrics exposed at `:9092/metrics`:**
- `ha_relay_requests_total`: Total relay requests processed
- `ha_relay_validation_errors_total`: Relay validation failures
- `ha_session_trees_active`: Active SMST trees in memory
- `ha_cache_hits_total` / `ha_cache_misses_total`: Cache performance
- `ha_leader_election_state`: Leader election status (1=leader, 0=standby)

**Redis health checks:**
```bash
# Check memory usage
redis-cli INFO memory

# Monitor latency
redis-cli --latency

# Check connected clients
redis-cli CLIENT LIST
```

## Troubleshooting

### Common Issues

**1. Redis Out of Memory**
- Symptom: `OOM command not allowed` errors
- Solution: Increase `maxmemory` or enable eviction policy
- Prevention: Monitor memory usage, alert at 80% threshold
- Debug: `redis-debug keys --pattern "ha:*" --stats` to see what's consuming memory

**2. High Relay Latency**
- Symptom: Slow relay processing, timeouts
- Solution: Check Redis network latency, verify no disk swapping
- Check: `redis-cli --latency` should show <2ms
- Debug: `redis-debug streams --supplier <addr>` to check pending message backlog

**3. Leader Election Failures**
- Symptom: No active miner leader
- Solution: Check Redis connectivity, verify lock TTL settings
- Debug: `redis-debug leader` to check current leader status

**4. SMST Tree Corruption**
- Symptom: Proof generation failures
- Solution: WAL replay from last checkpoint
- Debug: `redis-debug smst --session <id>` to inspect tree node count
- Check: Redis keys `wal:session_{sessionID}` exist

**5. Stale Cache Data**
- Symptom: Validation failures, outdated session/params
- Solution: Invalidate cache entries to force refresh
- Debug: `redis-debug cache --type <type> --key <key> --invalidate`

**6. Orphaned Session Data**
- Symptom: High memory usage from old sessions
- Solution: Clean up completed/expired sessions
- Debug: `redis-debug sessions --supplier <addr> --state settled`
- Cleanup: `redis-debug flush --pattern "ha:miner:sessions:*:old_session_id"`

### Using Debug Tools for Troubleshooting

```bash
# 1. Check overall system state
redis-debug leader                    # Verify active leader
redis-debug keys --pattern "ha:*"     # See all HA keys

# 2. Investigate session issues
redis-debug sessions --supplier <addr> --state active
redis-debug smst --session <session_id>

# 3. Monitor real-time events
redis-debug pubsub --channel "ha:events:cache:session:invalidate"

# 4. Clean up after investigation
redis-debug flush --pattern "ha:test:*"  # Delete test data
```

## Architecture Details

### Component Responsibilities

**Relayer:**
- Validate relay requests (ring signatures, session validity)
- Sign relay responses with supplier keys
- Publish validated relays to Redis Streams
- Rate limit based on application stake (relay meter)

**Miner:**
- Consume relays from Redis Streams (per supplier)
- Build SMST trees in Redis (shared across instances)
- Submit claims at session grace period end
- Generate and submit proofs before proof window closes

**Cache Orchestrator:**
- Refresh shared caches on block updates (leader only)
- Coordinate L1 (local) / L2 (Redis) / L3 (network) cache layers
- Pub/sub invalidation across all relayer/miner instances
- Pre-warm caches with configured known entities

### Data Flow

1. Client sends relay request → Relayer
2. Relayer validates request → Publishes to Redis Stream
3. Miner (leader) consumes from stream → Updates SMST in Redis
4. WAL entry written to Redis Stream for durability
5. At session end → Miner generates claim → Submits to blockchain
6. During proof window → Miner generates proof from SMST → Submits to blockchain

## Dependencies

- **poktroll**: Core Pocket Network protocol (v0.1.31+)
- **Redis**: go-redis/v9 for Redis operations
- **Cosmos SDK**: Blockchain client libraries (v0.53.0)
- **CometBFT**: Consensus engine fork (pokt-network/cometbft)

## License

See LICENSE file in the repository.

## Contributing

This is production-grade software. All contributions must:
- Include comprehensive tests
- Pass all linters and static analysis
- Include performance benchmarks for critical paths
- Be reviewed for security vulnerabilities
- Maintain backward compatibility or provide migration path

## Support

For issues and questions:
- GitHub Issues: https://github.com/pokt-network/pocket-relay-miner/issues
- Discord: https://discord.gg/pokt (for community support)

### Load Testing

The `relay` command includes built-in load testing with full validation (signature verification + JSON-RPC error checking):

```bash
# Optimal concurrency for local testing (sweet spot: 3-4 workers)
./bin/pocket-relay-miner relay jsonrpc \
     --localnet \
     --relayer-url http://localhost:8180 \
     --service develop \
     --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj \
     --load-test --count 1000 --concurrency 4
```

**Expected Output:**
```
=== Load Test Results ===
Total Requests: 1000
Successful: 1000
Errors: 0
Success Rate: 100.00%

Duration: 845ms
Throughput: 1182.84 RPS

Latency Percentiles (ms):
  min: 0.78
  p50: 1.33
  p95: 2.67
  p99: 26.19
  max: 311.36
```

**Validation Performed:**
- ✅ HTTP status code (200 OK)
- ✅ Supplier signature verification (ECDSA)
- ✅ JSON-RPC error field inspection
- ✅ Relay protocol compliance

**Concurrency Guidelines:**
- **Local (Docker-in-Docker)**: Use concurrency 3-5 (avoids environment limits)
- **Production Load Test**: Use concurrency 10-50 (matches real traffic patterns)
- **Stress Test**: Use concurrency 100+ (identifies breaking points)
