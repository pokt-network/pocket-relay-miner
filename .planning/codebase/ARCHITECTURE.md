# Architecture

**Analysis Date:** 2026-02-02

## Pattern Overview

**Overall:** Distributed Event-Driven Microservices with Leader Election and Redis-Based State Sharing

**Key Characteristics:**
- Horizontally scalable multi-instance deployment (Relayers + Miners)
- Asynchronous relay processing via Redis Streams (WAL pattern)
- Distributed cache with three-tier hierarchy (L1 in-memory, L2 Redis, L3 network)
- Global leader election for singleton operations (cache refresh, claim/proof coordination)
- Supplier-scoped session state management with SMST (Sparse Merkle Sum Tree) storage
- Stateless relayers + stateful miners for claim/proof submission

## Layers

**Relayer Layer (Stateless):**
- Purpose: Accept relay requests from applications, validate signatures, forward to backends, collect responses, sign responses, publish to Redis Streams for mining
- Location: `relayer/`
- Contains: HTTP/WebSocket/gRPC proxy, request validation, response signing, relay metering, health checking
- Depends on: Cache (L1/L2), Redis Streams (publisher), Ring signatures (validation), Backend services
- Used by: Applications sending relay requests

**Miner Layer (Stateful):**
- Purpose: Consume mined relays from Redis Streams, build SMST trees, batch and submit claims/proofs to blockchain, manage session lifecycle
- Location: `miner/`
- Contains: Redis consumer, session lifecycle tracking, SMST manager, claim/proof pipelines, session state store, supplier management
- Depends on: Redis Streams (consumer), Blockchain RPC, SMST library, Session/Proof/Shared params
- Used by: Blockchain (submits claims/proofs), other miner instances (via Redis shared state)

**Cache Layer (Three-Tier):**
- Purpose: Reduce load on blockchain by caching frequently accessed data with automatic invalidation and leader-driven refresh
- Location: `cache/`
- Contains: Singleton caches (shared/session/proof params), keyed caches (applications, services, suppliers), orchestrator for parallel refresh, pub/sub invalidation
- Depends on: Blockchain RPC/gRPC, Redis (L2 storage), Leader election
- Used by: Relayer (metering), Miner (session validation), other services

**Leader Election (Global):**
- Purpose: Elect single leader instance to perform singleton operations (cache refresh, leader-specific coordination)
- Location: `leader/`
- Contains: Redis-backed distributed lock with TTL, leader status querying
- Depends on: Redis
- Used by: Cache orchestrator, miner leader controller

**Transport Layer (Redis Streams):**
- Purpose: Provide durable message queue for relay processing with consumer groups and auto-acknowledgment
- Location: `transport/redis/`
- Contains: Publisher (relayer publishes to streams), Consumer (miner consumes from streams), namespace management, reconnection logic
- Depends on: Redis
- Used by: Relayer (publish), Miner (consume)

**Configuration & Keys:**
- Purpose: Load and validate configuration, manage supplier private keys
- Location: `config/`, `keys/`
- Contains: YAML/JSON config parsing, key file loading, Cosmos keyring integration
- Depends on: Filesystem, Cosmos SDK
- Used by: All components for startup

**Query & RPC Client:**
- Purpose: Provide blockchain query clients and utilities
- Location: `query/`, `client/`
- Contains: Query client wrappers, session/block/service/application queries
- Depends on: Blockchain gRPC/RPC endpoints
- Used by: Relayer (service difficulty), Miner (session/claim validation), Cache (data refresh)

**Transaction Submission:**
- Purpose: Build, sign, and submit transactions to blockchain
- Location: `tx/`
- Contains: Transaction building, signing, submission with retries
- Depends on: Cosmos SDK, Blockchain RPC
- Used by: Miner (claim/proof submission)

## Data Flow

**Relay Processing Flow:**

1. **Request Reception** (Relayer HTTP/WebSocket/gRPC server)
   - Receive relay request from application
   - Extract service ID, app address, request body

2. **Validation** (relay_validator.go)
   - Verify application ring signature
   - Check session validity (via cache)
   - Verify supplier is active and has sufficient stake

3. **Backend Forwarding** (proxy.go)
   - Check backend health (healthcheck.go)
   - Forward to backend service (configured based on service type)
   - Handle timeouts and retries
   - Support streaming responses (SSE, NDJSON)

4. **Response Signing** (relay_processor.go)
   - Deserialize relay request/response
   - Calculate relay hash
   - Check mining difficulty (compare hash to service target)
   - Sign response with supplier's private key

5. **Mined Relay Publishing** (relay_processor.go → transport/redis/publisher.go)
   - Publish to Redis Stream: `ha:relays:{supplierAddr}`
   - Message contains: session ID, app address, request/response hash, relay data
   - Fire-and-forget with acknowledgment

6. **Metering** (relay_meter.go, async after signature verification)
   - Load app stake from Redis cache (or blockchain)
   - Load service compute units
   - Track session meter data in Redis
   - Update active session counter
   - Support both eager (before response) and optimistic (after response) modes

7. **Miner Consumption** (miner → transport/redis/consumer.go)
   - Consume from Redis Stream consumer group
   - Auto-acknowledge message
   - Extract session ID

8. **Session Discovery** (session_lifecycle.go)
   - On new session ID, create SessionSnapshot
   - Query blockchain for session params
   - Cache session data in Redis
   - Transition to ACTIVE state

9. **Relay Accumulation** (smst_manager.go)
   - Add relay hash to SMST tree for session
   - Store in Redis (persistent across HA instances)
   - Increment relay count and compute units sum

10. **Claim Submission** (claim_pipeline.go, lifecycle callback)
    - When session ends, trigger claim submission
    - Build claim proof (SMST root)
    - Batch up to N claims in single transaction
    - Submit to blockchain with retry
    - Track submission in Redis

11. **Proof Submission** (proof_pipeline.go, lifecycle callback)
    - When claim window closes, prepare proof
    - Calculate merkle proof for random sample
    - Batch up to N proofs in single transaction
    - Submit to blockchain with retry

12. **Session Cleanup** (session_lifecycle.go)
    - On settlement, clean up SMST tree from Redis
    - Delete stream (optional, with retention for recovery)
    - Publish meter cleanup signal (via pub/sub) to relayers

**Cache Refresh Flow (Leader-Only):**

1. **Block Event** (cache/block_subscriber.go)
   - Leader receives block height from CometBFT WebSocket or Redis pub/sub
   - Triggers cache refresh cycle

2. **Parallel Refresh** (cache/orchestrator.go)
   - Refresh all cache types in parallel using worker pool:
     - Singleton caches: shared params, session params, proof params
     - Keyed caches: applications, services
     - Supplier cache (if not using distributed claiming)

3. **Cache Update** (cache/{type}.go)
   - Query blockchain for latest data
   - Cache in Redis (L2) with TTL
   - Publish invalidation signal via pub/sub

4. **All-Instance Invalidation** (cache/pubsub.go)
   - All instances (including leader) receive invalidation
   - Clear L1 in-memory cache
   - Next access will reload from Redis L2
   - If L2 miss, query blockchain L3

**State Management:**

**In-Memory (L1 - Relayer/All Miners):**
- L1 cache: `xsync.MapOf` for lock-free concurrent reads
- Session meters: Per-session metering data for billing
- Service factors: Per-service configuration for stake calculations
- Active session counter: Number of sessions in ACTIVE state

**Redis (L2 - Shared Across All Instances):**
- Cache entries: `ha:cache:{type}:{id}` with TTL
- SMST nodes: `ha:smst:{sessionID}:nodes` (Hash)
- Session metadata: `ha:miner:sessions:{supplier}:{sessionID}`
- Session index by state: `ha:miner:sessions:{supplier}:state:{state}`
- Relay streams: `ha:relays:{supplierAddr}` (Redis Streams)
- Leader lock: `ha:miner:global_leader` (TTL 30s)
- Meter data: `ha:meter:{sessionID}` (Hash)
- Supplier registry: `ha:suppliers:{address}` (Hash)

**Blockchain (L3 - Authoritative):**
- Session parameters
- Shared/Service/Proof parameters
- Application stakes and delegations
- Claim/Proof settlement
- Supplier registrations

## Key Abstractions

**Session (miner/session_lifecycle.go):**
- Purpose: Represents a relay mining session with state machine (ACTIVE → CLAIMABLE → PROOFABLE → SETTLED/FAILED)
- Examples: `miner/session_lifecycle.go`, `miner/session_store.go`, `miner/session_coordinator.go`
- Pattern: Event-driven state machine with callbacks for transitions; state stored in Redis; parallel processing via worker pools

**Relay Batch (transport/interface.go):**
- Purpose: Collection of mined relays ready for submission as claims/proofs
- Examples: `miner/claim_pipeline.go`, `miner/proof_pipeline.go`
- Pattern: Batching for efficiency; accumulated until timeout or max size; submitted atomically

**SMST Tree (miner/smst_manager.go):**
- Purpose: Accumulate relay hashes in tree structure for cryptographic proof
- Examples: `miner/redis_mapstore.go` (Redis storage), `miner/smst_manager.go` (tree lifecycle)
- Pattern: Tree operations stored in Redis Hashes; accessed via xsync-based in-memory cache; sealed on session end

**Distributed Cache (cache/orchestrator.go):**
- Purpose: Three-tier cache coordinating L1 (in-memory), L2 (Redis), L3 (blockchain)
- Examples: `cache/application_cache.go`, `cache/shared_params_singleton.go`, `cache/supplier_cache.go`
- Pattern: Singleton pattern for per-instance caches; pub/sub for cross-instance invalidation; leader-driven refresh

**Supplier Registry (miner/supplier_registry.go, miner/supplier_manager.go):**
- Purpose: Track active suppliers and route their relays for processing
- Examples: `miner/supplier_worker.go` (per-supplier processing), `miner/supplier_drain.go` (graceful shutdown)
- Pattern: Dynamic discovery via key changes; per-supplier Redis streams; multi-supplier processing in single miner

## Entry Points

**Relayer (`cmd/cmd_relayer.go`):**
- Location: `cmd/cmd_relayer.go` (CLI) → `relayer/service.go` (Service)
- Triggers: `pocket-relay-miner relayer --config <yaml>`
- Responsibilities:
  - Initialize config from YAML
  - Create HTTP/WebSocket/gRPC servers
  - Initialize caches, Redis client, validator, signer
  - Start health checker for backends
  - Listen for relay requests, forward to backends, publish to Redis
  - Graceful shutdown on SIGINT/SIGTERM

**Miner (`cmd/cmd_miner.go`):**
- Location: `cmd/cmd_miner.go` (CLI) → `miner/supplier_worker.go` (per-supplier) or `miner/leader_controller.go` (leader)
- Triggers: `pocket-relay-miner miner --config <yaml>`
- Responsibilities:
  - Initialize config from YAML
  - Load supplier keys (hot-reload support)
  - Create Redis consumer for each supplier's stream
  - Manage session lifecycle (watch for transitions)
  - Build SMST trees and submit claims/proofs
  - Elect leader for singleton operations
  - Graceful shutdown: drain sessions, wait for pending operations

**Redis Debug (`cmd/cmd_redis.go`):**
- Location: `cmd/cmd_redis.go` → `cmd/redis/{command}.go`
- Triggers: `pocket-relay-miner redis <subcommand>`
- Responsibilities: Inspect Redis state for debugging (sessions, SMST, streams, cache, leader, submissions)

**Load Test Relay (`cmd/cmd_relay.go`):**
- Location: `cmd/cmd_relay.go` → `cmd/relay/{transport}.go`
- Triggers: `pocket-relay-miner relay <transport>`
- Responsibilities: Load test harness for relayer performance benchmarking

## Error Handling

**Strategy:** Structured error wrapping, typed error returns, early validation with user feedback

**Patterns:**

1. **Validation Errors** (early, user-facing)
   - File: `relayer/validator.go`, `relayer/relay_processor.go`
   - Logged at WARN level with context
   - Rejected with HTTP 400 or gRPC error
   - Example: `relay validation failed: ring signature invalid`

2. **Transient Failures** (retry-able)
   - File: `miner/claim_pipeline.go`, `miner/proof_pipeline.go`, `tx/tx_client.go`
   - Logged at WARN level
   - Retried with exponential backoff
   - Example: `claim submission failed: blockchain unavailable, retrying...`

3. **Fatal Errors** (unrecoverable)
   - File: All main loops with defer recovery
   - Logged at ERROR level with stack trace
   - Component shuts down gracefully
   - Example: `panic recovered: unexpected nil pointer in session manager`

4. **Redis Failures** (degradation)
   - File: `relayer/relay_meter.go` (FailBehavior: open/closed)
   - May reject relays (FailClosed) or accept blindly (FailOpen)
   - Logged at WARN/ERROR
   - Example: `redis unavailable, failing closed (rejecting relays)`

## Cross-Cutting Concerns

**Logging:** Structured logging via `github.com/rs/zerolog`
- Components create child loggers: `logging.ForComponent(parent, "component_name")`
- Supplier-scoped logging: `logging.ForSupplierComponent(parent, "component", supplier_addr)`
- Fields: timestamp, level, component, supplier, message, error context
- Usage: `logger.Info().Str("session_id", sid).Msg("session active")`

**Validation:** Multi-layer validation
- Request level: `relayer/validator.go` (ring signatures, session validity)
- Data level: `relayer/relay_processor.go` (relay hash mining)
- Transaction level: `tx/tx_client.go` (transaction format)
- Failure: Log and reject early to prevent wasted work

**Authentication:** None (application is supplier-facing, not user-facing)
- Relayers identify supplier via signed relay (ring signature)
- Supplier operator identified via private key loaded from file/keyring
- No API keys or bearer tokens

**Metrics:** Prometheus for observability
- Relayer: requests, responses, errors, latency by service
- Miner: sessions, claims, proofs, SMST operations
- Cache: hits, misses, refresh latency
- Redis: connection health, throughput
- All registered via `prometheus.DefaultRegisterer`
- Exposed at `/metrics` (HTTP) or gRPC metrics service

**Concurrency:** Bounded via worker pools
- All goroutine-spawning uses `github.com/alitto/pond/v2` (worker pool)
- Prevents unbounded goroutine growth
- Examples: `cache/orchestrator.go` (refresh pool), `miner/claim_pipeline.go` (batching pool)

**Resilience:**
- **Relayer**: Stateless - failed instance replaced immediately
- **Miner**: SMST state in Redis - failed instance picks up from last known state
- **Cache**: L1 lost on instance failure - reloaded from L2/L3
- **Leader**: Lost on failure - new leader elected within 30s (TTL)
- **Redis Streams**: Messages redelivered if consumer crashes (consumer group coordination)

---

*Architecture analysis: 2026-02-02*
