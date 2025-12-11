# Pocket RelayMiner (High Availability)

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

- **Target**: 1000+ RPS per relayer replica
- **SMST Operations**: ~30µs per operation (Redis Hashes)
- **Relay Processing**: Sub-millisecond validation and signing
- **Failover Time**: <5 seconds (leader election + state recovery)

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

See `localnet/ha/relayer-config-1.yaml` for example configuration.

**Key settings:**
- `listen_addr`: HTTP server bind address (e.g., `0.0.0.0:3000`)
- `redis.url`: Redis connection string (e.g., `redis://localhost:6379`)
- `pocket_node.query_node_grpc_url`: Pocket node gRPC endpoint
- `pocket_node.query_node_rpc_url`: Pocket node RPC endpoint (WebSocket)
- `services`: Backend service configurations per service ID and RPC type

### Miner Configuration

See `localnet/ha/miner-config-1.yaml` for example configuration.

**Key settings:**
- `redis.url`: Redis connection string (must match relayer)
- `redis.consumer_group`: Consumer group name (e.g., `ha-miners`)
- `redis.consumer_name`: Unique consumer identifier per replica
- `pocket_node.query_node_grpc_url`: Pocket node gRPC endpoint
- `pocket_node.tx_node_rpc_url`: Pocket node RPC endpoint for submitting transactions
- `keys.keys_file`: Path to supplier keys YAML file
- `known_applications`: Application addresses to pre-warm in cache

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
