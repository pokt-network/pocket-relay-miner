# External Integrations

**Analysis Date:** 2026-02-02

## APIs & External Services

**Pocket Network Blockchain:**
- Service: Pocket blockchain node (Shannon testnet/localnet)
  - Query service via gRPC
  - Transaction submission via RPC
  - Block events via WebSocket RPC or Redis pub/sub
  - SDK: github.com/pokt-network/poktroll v0.1.31-rc1
  - RPC endpoint config: `pocket_node.query_node_rpc_url` (default: http://localhost:26657)
  - gRPC endpoint config: `pocket_node.query_node_grpc_url` (default: localhost:9090)

**Relay Backend Services:**
- Service: Customer backend APIs (HTTP/WebSocket/gRPC/Streaming)
- Connection pooling per service with configurable timeouts
- HTTP client pool: 500 max connections per host (5x default)
- Idle connection management: 5 minute timeout, 100 warm connections
- Router: HTTP headers determine backend type:
  - `Rpc-Type: 1` → gRPC
  - `Rpc-Type: 2` → WebSocket
  - `Rpc-Type: 3` → JSON-RPC (HTTP)
  - `Rpc-Type: 4` → REST/Streaming (SSE)
  - `Rpc-Type: 5` → CometBFT

## Data Storage

**Databases:**
- Redis (single node, sentinel, or cluster)
  - Connection: `redis.url` in config (supports redis://, redis-sentinel://, redis-cluster://)
  - Client: github.com/redis/go-redis/v9
  - Connection pool: 20 × GOMAXPROCS (auto-tuned)
  - Uses:
    - Redis Streams: Relay WAL (`ha:relays:{supplier}`)
    - Redis Hashes: SMST nodes (`ha:smst:{sessionID}:nodes`)
    - Redis Strings: Session metadata, cache entries
    - Redis Sets: Session indexes, supplier lists
    - Pub/Sub: Block events, cache invalidation

**File Storage:**
- Supplier signing keys from file system:
  - YAML file: `keys.keys_file` (one YAML with multiple keys)
  - Directory: `keys.keys_dir` (one file per key)
  - Cosmos keyring: `keys.keyring.backend` (file, os, test, memory)

**Caching:**
- In-memory (L1): xsync.MapOf for lock-free reads (<100ns)
- Redis (L2): Shared across instances (<2ms)
- Network (L3): Blockchain queries (<100ms)
- Cache types:
  - Applications (`ha:cache:application:{address}`)
  - Services (`ha:cache:service:{serviceID}`)
  - Sessions (`ha:cache:session:{sessionID}`)
  - Shared/session/proof params
  - Accounts and suppliers

## Authentication & Identity

**Auth Provider:**
- Custom: Supplier secp256k1 ECDSA signatures
  - Signature verification on relay requests (ring signature validation)
  - Response signing with supplier private keys
  - Key storage via Cosmos keyring or file system

**Session Management:**
- Pocket Network on-chain sessions
  - Session queries via blockchain gRPC
  - Session parameters cached in Redis with TTL
  - Deduplication tracking per session

## Monitoring & Observability

**Error Tracking:**
- None detected (errors logged via zerolog)

**Logs:**
- Structured logging via github.com/rs/zerolog
- Output: JSON format (default) or text format
- Async ring buffer: 100KB default, 100ms poll interval
- Log levels: Debug, Info, Warn, Error
- Fields: Structured context (session_id, supplier, service, etc.)
- Config: `logging.level`, `logging.format`, `logging.async`

**Metrics:**
- Prometheus HTTP endpoints: `:9090/metrics`
- Client: github.com/prometheus/client_golang v1.22.0
- Metrics exposed:
  - ha_observability_instruction_duration_seconds (histogram)
  - ha_observability_operation_duration_seconds (histogram)
  - ha_observability_redis_operation_duration_seconds (histogram)
  - ha_observability_redis_operations_total (counter)
  - ha_observability_onchain_query_duration_seconds (histogram)
  - Custom component metrics (relayer, miner, cache)
- Pprof endpoint: `:6060` (optional, disabled by default)
- Config: `observability.metrics_enabled`, `observability.metrics_addr`

## CI/CD & Deployment

**Hosting:**
- Kubernetes (primary) - Local via Tilt, remote via kubectl
- Docker container images
- Services exposed via LoadBalancer (Tilt proxy)

**CI Pipeline:**
- None detected in main codebase
- GitHub Actions configuration in .github/ folder
- Build: make build / make build-release
- Test: make test / make test_miner
- Lint: make lint
- Format: make fmt

## Environment Configuration

**Required env vars:**
- `POCKET_NODE_RPC_URL` (or config file: `pocket_node.query_node_rpc_url`)
- `POCKET_NODE_GRPC_URL` (or config file: `pocket_node.query_node_grpc_url`)
- `REDIS_URL` (or config file: `redis.url`)
- `RELAYER_CONFIG_FILE` (optional - path to config file)
- `MINER_CONFIG_FILE` (optional - path to config file)

**Test/Debug env vars:**
- `TEST_FORCE_CLAIM_TX_ERROR=true` - Force claim submission failure (testing)
- `TEST_FORCE_PROOF_TX_ERROR=true` - Force proof submission failure (testing)

**Secrets location:**
- Supplier keys: File system (YAML, directory) or Cosmos keyring
  - File: `keys.keys_file` path
  - Directory: `keys.keys_dir` path
  - Keyring: `keys.keyring.dir` with backend selection
- No .env file pattern detected - configuration via YAML and env vars

## Webhooks & Callbacks

**Incoming:**
- HTTP endpoints:
  - Relayer: `:8080` - JSON-RPC relay requests (POST)
  - WebSocket: `:8080/ws` - WebSocket relay subscriptions
  - gRPC: `:8080` - gRPC relay service
  - Streaming: `:8080/stream` - Server-Sent Events (SSE)
  - Health checks: `/status` endpoint (HTTP)
  - gRPC health: standard gRPC health check service

**Outgoing:**
- Relay responses sent to backends (HTTP POST, WebSocket, gRPC, REST)
- Claim/proof transactions submitted to Pocket blockchain via RPC
- Redis pub/sub events for:
  - Block events (`ha:events:block`)
  - Cache invalidation (`ha:events:cache:{type}:invalidate`)
  - Supplier registry updates (`ha:events:supplier_update`)
  - Meter cleanup (`ha:meter:cleanup`)

## gRPC Services

**Relayer gRPC:**
- Location: `relayer/relay_grpc_service.go`
- Service: Relay processing via gRPC
- Interceptors: Custom gRPC interceptors for validation and metrics

**Query gRPC:**
- Location: `query/query.go`
- Services: Application, Session, Supplier, Service, Proof, Shared, Account, Bank
- Codec: ProtoCodec with Cosmos SDK integration
- Connection: TLS optional (configurable for production)
- Pool size: 500 max connections (HTTP layer)

## Redis Pub/Sub Channels

**Core Events:**
- `ha:events:block` - Block height updates (published by miner, consumed by relayers)
- `ha:events:cache:{type}:invalidate` - Cache invalidation signals
  - Types: application, service, session, shared_params, session_params, proof_params, account, supplier
- `ha:events:supplier_update` - Supplier registry updates
- `ha:meter:cleanup` - Meter cleanup signals

**Consumer Groups:**
- `ha-miners` - Consumer group for relay streams (one per supplier)
  - Persists relay messages across miner restarts
  - Supports HA failover with message acknowledgment

## Transaction Submission

**Pocket Blockchain Transactions:**
- Client: `tx/tx_client.go` using Cosmos SDK
- Codec: ProtoCodec with Cosmos integration
- Signing: Secp256k1 ECDSA via Cosmos keyring or file keys
- Gas configuration:
  - Price: 0.000001upokt (default)
  - Adjustment: 1.7x (safety margin)
  - Timeout height: 100 blocks
  - Chain ID: "pocket" (default, configurable for testnet)
- Transaction types:
  - CreateClaimMsg: Submit relay claim proofs
  - CreateProofMsg: Submit SMST proofs
- Retry logic: Exponential backoff with configurable attempts

---

*Integration audit: 2026-02-02*
