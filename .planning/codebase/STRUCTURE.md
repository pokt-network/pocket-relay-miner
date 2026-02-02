# Codebase Structure

**Analysis Date:** 2026-02-02

## Directory Layout

```
pocket-relay-miner/
├── main.go                  # CLI entry point with root command and subcommands
├── version.go               # Version constant
├── go.mod / go.sum          # Go module and dependencies
├── Makefile                 # Build, test, lint targets
├── Tiltfile                 # Kubernetes/Tilt development environment
├── .github/                 # GitHub Actions CI/CD
│
├── cmd/                     # CLI command implementations
│   ├── cmd.go               # Command registration
│   ├── cmd_relayer.go       # `pocket-relay-miner relayer` subcommand
│   ├── cmd_miner.go         # `pocket-relay-miner miner` subcommand
│   ├── cmd_relay.go         # `pocket-relay-miner relay` load test harness
│   ├── cmd_redis.go         # `pocket-relay-miner redis` debug tools
│   ├── cmd_version.go       # `pocket-relay-miner version` subcommand
│   ├── relay/               # Load test implementations (HTTP, WebSocket, gRPC, Streaming)
│   └── redis/               # Redis debug subcommands (cache, sessions, SMST, streams, etc.)
│
├── relayer/                 # HTTP/WebSocket/gRPC proxy for relays (STATELESS)
│   ├── service.go           # Main Service orchestrator
│   ├── config.go            # Configuration loading and validation
│   ├── proxy.go             # HTTP/WebSocket/gRPC request routing and response handling
│   ├── relay_processor.go   # Relay deserialization, signing, difficulty checking
│   ├── relay_meter.go       # Application metering (stake checking, session validation)
│   ├── relay_pipeline.go    # Relay processing pipeline orchestration
│   ├── validator.go         # Relay request validation (ring signatures, sessions)
│   ├── signer.go            # Response signing with supplier keys
│   ├── healthcheck.go       # Backend health checking
│   ├── http_stream.go       # Streaming response handling (SSE, NDJSON)
│   ├── websocket.go         # WebSocket proxy and upgrade handling
│   ├── relay_grpc_service.go # gRPC service implementation
│   ├── grpc_web.go          # gRPC-Web bridge
│   ├── grpc_interceptor.go  # gRPC middleware
│   ├── session_monitor.go   # Session height monitoring
│   ├── service_factor_client.go # Service-specific configuration client
│   ├── middleware.go        # HTTP middleware
│   ├── metrics.go           # Prometheus metrics
│   ├── metric_recorder.go   # Async metric recording
│   ├── buffer_pool.go       # Byte buffer pooling for request/response bodies
│   └── (test files)
│
├── miner/                   # SMST builder and claim/proof submitter (STATEFUL)
│   ├── supplier_worker.go   # Worker for processing a single supplier (runs on all miners)
│   ├── leader_controller.go # Leader-only coordinator for claim/proof submission
│   ├── supplier_manager.go  # Manages multiple suppliers per miner
│   ├── session_lifecycle.go # Session state machine (ACTIVE → CLAIMABLE → PROOFABLE → SETTLED)
│   ├── session_store.go     # Redis-backed session metadata storage
│   ├── session_coordinator.go # Coordinates session processing across cluster
│   ├── smst_manager.go      # SMST tree lifecycle (create, update, seal, prove)
│   ├── redis_mapstore.go    # Redis-backed key-value store for SMST nodes
│   ├── claim_pipeline.go    # Accumulates and submits claims in batches
│   ├── proof_pipeline.go    # Accumulates and submits proofs in batches
│   ├── supplier_claimer.go  # Per-supplier claim submission orchestration
│   ├── proof_requirement.go # Determines if probabilistic proof required
│   ├── config.go            # Miner configuration (claims, proofs, Redis)
│   ├── metrics.go           # Prometheus metrics (sessions, claims, proofs, SMST ops)
│   ├── supplier_registry.go # Supplier discovery and tracking
│   ├── supplier_drain.go    # Graceful supplier shutdown
│   ├── deduplicator.go      # Prevents duplicate relay submissions
│   ├── service_factor_registry.go # Service-level configuration registry
│   ├── balance_monitor.go   # Monitors supplier account balance
│   ├── block_health_monitor.go # Monitors blockchain RPC health
│   ├── params_refresher.go  # Refreshes blockchain parameters (fallback)
│   ├── settlement_monitor.go # Monitors session settlement on blockchain
│   ├── submission_timing.go # Tracks claim/proof submission timing
│   ├── submission_tracker.go # Tracks historical submissions for debugging
│   ├── lifecycle_callback.go # Callback handler for session lifecycle events
│   └── (test files and benches)
│
├── cache/                   # Three-tier cache (L1: in-memory, L2: Redis, L3: blockchain)
│   ├── orchestrator.go      # Cache coordinator and leader-driven refresh
│   ├── warmer.go            # Cache initialization on startup
│   ├── interface.go          # Cache type definitions and interfaces
│   ├── metrics.go           # Prometheus metrics for cache operations
│   ├── pubsub.go            # Pub/sub for cross-instance cache invalidation
│   ├── block_subscriber.go  # Block event subscription for cache refresh
│   ├── block_subscriber_adapter.go # Adapter for block subscriber integration
│   ├── redis_block_client_adapter.go # Redis-based block height client
│   ├── block_publisher.go   # Redis pub/sub publisher for block events
│   │
│   ├── shared_params_singleton.go # Shared protocol parameters (singleton, leader-refreshed)
│   ├── shared_params.go     # Shared params with fallback (relayer version)
│   ├── session_params_singleton.go # Session parameters (singleton, leader-refreshed)
│   ├── session_params.go    # Session params wrapper (miner version)
│   ├── proof_params.go      # Proof parameters (singleton, leader-refreshed)
│   │
│   ├── application_cache.go # Application stake and delegation cache (keyed by address)
│   ├── service_cache.go     # Service metadata cache (keyed by service ID)
│   ├── account_cache.go     # Account/supplier account balance cache (keyed by address)
│   ├── supplier_cache.go    # Supplier availability and state (keyed by address)
│   ├── supplier_params.go   # Supplier module parameters (singleton)
│   │
│   ├── client_adapters.go   # Query client wrappers (thin layer)
│   └── (test files)
│
├── transport/               # Relay message transport (Redis Streams)
│   ├── interface.go         # Publisher/Consumer interfaces
│   ├── types.go             # Message types
│   ├── mined_relay.pb.go    # Protocol buffer for serialized relays
│   ├── relay_utils.go       # Relay serialization helpers
│   └── redis/               # Redis-backed implementation
│       ├── client.go        # Redis client wrapper
│       ├── publisher.go     # Publishes mined relays to streams
│       ├── consumer.go      # Consumes mined relays from streams (with consumer groups)
│       ├── namespace.go     # Stream naming and key management
│       ├── metrics.go       # Throughput and latency metrics
│       ├── reconnect.go     # Reconnection logic for Redis failures
│       └── (test/bench files)
│
├── leader/                  # Distributed leader election
│   ├── global_leader.go     # Redis-backed leader election with TTL
│   └── metrics.go           # Leader election metrics
│
├── tx/                      # Blockchain transaction building and submission
│   └── tx_client.go         # Builds, signs, and submits claims/proofs
│
├── query/                   # Blockchain query client wrappers
│   └── clients.go           # Query client initialization and pooling
│
├── client/                  # Helper clients for local/remote queries
│   └── (various client wrappers)
│
├── keys/                    # Supplier key management
│   └── (key loading and validation)
│
├── rings/                   # Ring signature verification (copied from poktroll)
│   └── (ring verification logic)
│
├── config/                  # Shared configuration types
│   ├── redis.go             # Redis connection config
│   ├── keys.go              # Key source configuration
│   ├── observability.go     # Logging/metrics config
│   └── pocket_node.go       # Blockchain node config
│
├── logging/                 # Structured logging utilities
│   └── (logger wrappers and context helpers)
│
├── observability/           # Observability setup
│   └── (Prometheus registry and metric registration)
│
├── tilt/                    # Kubernetes/Tilt dev environment
│   ├── Tiltfile             # Kubernetes manifests and Tilt config
│   ├── config/              # YAML configs for services
│   └── (Kubernetes deployment templates)
│
├── scripts/                 # Helper scripts
│   ├── test-simple-relay.sh # Integration test script
│   └── (other test/dev scripts)
│
├── docs/                    # Documentation
│   └── (architecture docs, API specs)
│
├── .planning/codebase/      # AI planning documents (generated)
│   ├── ARCHITECTURE.md
│   ├── STRUCTURE.md
│   ├── CONVENTIONS.md
│   ├── TESTING.md
│   ├── STACK.md
│   ├── INTEGRATIONS.md
│   └── CONCERNS.md
│
└── reports/                 # Load test and benchmark reports
    └── (CSV/JSON reports from load testing)
```

## Directory Purposes

**`cmd/`:**
- Purpose: CLI command implementations (entry points for each mode: relayer, miner, debug)
- Contains: Command registration, flag parsing, dependency injection, startup orchestration
- Key files: `cmd_relayer.go`, `cmd_miner.go`, `cmd_redis.go`

**`relayer/`:**
- Purpose: HTTP/WebSocket/gRPC proxy that validates relays, forwards to backends, publishes mined relays
- Contains: Request routing, signature validation, response signing, metering, health checking
- Key files: `proxy.go` (main handler), `relay_processor.go` (mining logic), `relay_meter.go` (stake checking)

**`miner/`:**
- Purpose: Stateful relay processing - consumes from Redis, builds SMST trees, submits claims/proofs
- Contains: Session state machine, SMST management, claim/proof submission pipelines, supplier coordination
- Key files: `session_lifecycle.go` (state machine), `smst_manager.go` (tree ops), `claim_pipeline.go`, `proof_pipeline.go`

**`cache/`:**
- Purpose: Three-tier cache coordinating in-memory (L1), Redis (L2), and blockchain (L3) data
- Contains: Entity caches (applications, services, suppliers), parameter caches, orchestrator, pub/sub invalidation
- Key files: `orchestrator.go` (coordinator), individual cache files per entity type

**`transport/`:**
- Purpose: Durable relay transport via Redis Streams with consumer groups
- Contains: Message serialization, stream publishing, consumer group management
- Key files: `redis/publisher.go`, `redis/consumer.go`, `redis/namespace.go`

**`leader/`:**
- Purpose: Distributed leader election for singleton operations
- Contains: Redis-backed lock with TTL, leader status querying
- Key files: `global_leader.go`

**`tx/`:**
- Purpose: Blockchain transaction building, signing, and submission
- Contains: Cosmos SDK integration for claim/proof messages
- Key files: `tx_client.go`

**`query/`:**
- Purpose: Blockchain query client initialization and pooling
- Contains: Query client wrappers for gRPC/RPC endpoints
- Key files: `clients.go`

## Key File Locations

**Entry Points:**
- `main.go`: Root command with all subcommands
- `cmd/cmd_relayer.go`: Relayer startup
- `cmd/cmd_miner.go`: Miner startup
- `cmd/cmd_redis.go`: Redis debug tools

**Configuration:**
- `relayer/config.go`: Relayer configuration schema and validation
- `miner/config.go`: Miner configuration schema and validation
- `config/redis.go`: Redis connection configuration
- `config/keys.go`: Key source configuration
- `config/pocket_node.go`: Blockchain node configuration

**Core Logic:**
- `relayer/proxy.go`: HTTP/WebSocket/gRPC proxy main handler (~2000 LOC, handles all transport types)
- `relayer/relay_processor.go`: Relay signature verification and mining difficulty checking
- `relayer/relay_meter.go`: Application metering and stake validation
- `miner/session_lifecycle.go`: Session state machine (~1400 LOC, manages lifecycle transitions)
- `miner/smst_manager.go`: SMST tree creation, updates, sealing, and proof generation
- `miner/claim_pipeline.go`: Batched claim submission
- `miner/proof_pipeline.go`: Batched proof submission
- `cache/orchestrator.go`: Cache coordinator with parallel refresh logic
- `transport/redis/consumer.go`: Redis Streams consumer with auto-acknowledgment

**Testing:**
- `miner/redis_mapstore_test.go`: SMST storage benchmarks (~350 LOC, validates <100µs per operation)
- `miner/redis_smst_bench_test.go`: SMST tree operation benchmarks
- `miner/redis_smst_manager_test.go`: SMST manager integration tests (~750 LOC)
- `scripts/test-simple-relay.sh`: End-to-end integration test

## Naming Conventions

**Files:**
- Interface implementations: `{interface_name}_impl.go` or just `{package}.go`
- Example: `relayer/config.go` (implements Config struct and methods)
- Suffixes for variants: `_singleton.go` (singleton cache variant), `_adapter.go` (adapter pattern)

**Directories:**
- Package per concern: `relayer/`, `miner/`, `cache/`, `transport/`, `leader/`
- Sub-packages for transport backends: `transport/redis/`
- Sub-packages for debug commands: `cmd/relay/`, `cmd/redis/`

**Variables & Functions:**
- Unexported receiver methods (private to package): lowercase (e.g., `func (s *Service) start()`)
- Exported receiver methods (package API): PascalCase (e.g., `func (s *Service) Start()`)
- Interfaces: PascalCase (e.g., `RelayProcessor`, `MinedRelayPublisher`)
- Configuration structs: `{Component}Config` (e.g., `RelayerConfig`, `MinerConfig`)

**Types:**
- Proto-generated types: Package prefix from poktroll (e.g., `sessiontypes.SessionHeader`)
- Local types: No prefix (e.g., `RelayProcessor`, `SessionSnapshot`)
- Callbacks: Verb-based naming (e.g., `AppDiscoveryCallback`, `SessionLifecycleCallback`)

## Where to Add New Code

**New Feature (e.g., new relay transport type):**
- Primary code: `relayer/` (add handler in `proxy.go` or new file like `http2_handler.go`)
- Tests: `relayer/*_test.go` (co-located with implementation)
- Configuration: Add fields to `relayer/config.go` and validate
- Metrics: Add collectors to `relayer/metrics.go`

**New Component/Module:**
- Implementation: Create new package directory (e.g., `miner/new_component.go`)
- Interface: Define in `{package}/interface.go` if shared with other packages
- Tests: Co-located test file `{package}/{component}_test.go`
- Metrics: Register in `{package}/metrics.go` or central `observability/`
- Logging: Use `logging.ForComponent()` or `logging.ForSupplierComponent()`

**New Cache Type:**
- Interface: Add to `cache/interface.go` (define `{Entity}Cache` interface)
- Implementation: Create `cache/{entity}_cache.go` with L1/L2/L3 logic
- Singleton variant: Create `cache/{entity}_singleton.go` if single instance per miner
- Orchestrator integration: Wire into `cache/orchestrator.go` refresh cycle
- Pub/Sub: Add invalidation channel to `cache/pubsub.go`

**Utilities:**
- Shared helpers: `cache/client_adapters.go`, `relayer/buffer_pool.go`, `miner/redis_mapstore.go`
- Transport helpers: `transport/relay_utils.go`
- Configuration: `config/{concern}.go`

## Special Directories

**`tilt/`:**
- Purpose: Kubernetes/Tilt development environment
- Generated: No (checked in, developer-maintained)
- Committed: Yes (required for local development)
- Usage: `tilt up` starts all services (miner, relayers, Redis, blockchain)

**`scripts/`:**
- Purpose: Shell scripts for testing and development
- Generated: No (hand-written)
- Committed: Yes
- Usage: `./scripts/test-simple-relay.sh` for integration testing

**`.planning/codebase/`:**
- Purpose: AI planning documents (generated by Claude)
- Generated: Yes (created by `/gsd:map-codebase`)
- Committed: Yes (documented in repo)
- Usage: Referenced by `/gsd:plan-phase` and `/gsd:execute-phase`

**`reports/`:**
- Purpose: Load test results and benchmarks
- Generated: Yes (by load test scripts)
- Committed: No (benchmarks vary by hardware)
- Usage: Performance tracking and regression detection

**`.github/`:**
- Purpose: GitHub Actions CI/CD
- Generated: No (hand-written)
- Committed: Yes
- Contents: Test/lint workflows triggered on push

## File Organization Patterns

**Component Packages (e.g., `relayer/`, `miner/`):**
1. `service.go` - Main orchestrator (if applicable)
2. `config.go` - Configuration types and defaults
3. `*.go` - Feature implementations
4. `metrics.go` - Prometheus metrics
5. `*_test.go` - Tests (co-located)
6. `*_bench_test.go` - Benchmarks

**Cache Packages (`cache/`):**
1. `interface.go` - All cache interfaces and types
2. `orchestrator.go` - Coordinator (leader-driven refresh)
3. `warmer.go` - Initialization
4. `{entity}_cache.go` - Per-entity implementations
5. `{entity}_singleton.go` - Singleton variants (leader-refreshed)
6. `pubsub.go` - Invalidation channels
7. `metrics.go` - Cache metrics

**Transport Packages (`transport/redis/`):**
1. `client.go` - Redis client wrapper
2. `publisher.go` - Publish interface implementation
3. `consumer.go` - Consume interface implementation
4. `namespace.go` - Key naming and management
5. `metrics.go` - Transport metrics
6. `reconnect.go` - Error recovery

## Dependency Graph (High Level)

```
cmd/
  └── relayer/service.go
  └── miner/supplier_worker.go (or leader_controller.go)

relayer/service.go
  ├── relayer/proxy.go
  ├── relayer/relay_processor.go
  ├── relayer/relay_meter.go
  ├── relayer/validator.go
  ├── cache/ (L1/L2/L3)
  ├── transport/redis/publisher.go
  └── observability/

miner/supplier_worker.go (or leader_controller.go)
  ├── miner/session_lifecycle.go
  ├── miner/smst_manager.go
  ├── miner/claim_pipeline.go
  ├── miner/proof_pipeline.go
  ├── transport/redis/consumer.go
  ├── cache/ (L1/L2/L3)
  ├── tx/tx_client.go
  ├── leader/global_leader.go
  └── observability/

cache/orchestrator.go
  ├── cache/*_cache.go (all entity caches)
  ├── leader/global_leader.go (leader-only refresh)
  ├── cache/block_subscriber.go
  └── transport/redis/client.go

transport/redis/
  └── redis/go-redis/v9 (external)

leader/global_leader.go
  └── transport/redis/client.go
```

---

*Structure analysis: 2026-02-02*
