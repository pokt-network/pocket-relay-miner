# Technology Stack

**Analysis Date:** 2026-02-02

## Languages

**Primary:**
- Go 1.24.3 - Core language for all services (relayer, miner, utilities)

## Runtime

**Environment:**
- Linux (primary), Darwin (development)
- Docker containers orchestrated by Kubernetes
- Tilt for local development environment

**Package Manager:**
- Go Modules (go.mod/go.sum)
- Lockfile: Present (go.sum - 178KB with 234 total dependencies)

## Frameworks

**Core:**
- Cobra 1.9.1 - CLI framework (main.go, cmd/)
- Protocol Buffers 1.36.6 - Message serialization (transport/mined_relay.pb.go)

**Networking & Transport:**
- net/http (stdlib) - HTTP/1.1 and HTTP/2 server
- golang.org/x/net/http2 1.45.0 - HTTP/2 support
- google.golang.org/grpc 1.72.0 - gRPC framework with interceptors
- github.com/gorilla/websocket 1.5.3 - WebSocket protocol
- improbable-eng/grpc-web 0.15.0 - gRPC-Web bridge for browser clients

**State Management & Storage:**
- github.com/redis/go-redis/v9 9.17.2 - Redis client (async pub/sub, streams, hashes)
- github.com/alicebob/miniredis/v2 2.35.0 - In-process Redis mock for testing
- github.com/puzpuzpuz/xsync/v4 4.2.0 - Lock-free concurrent maps (L1 cache)

**Cryptography & Blockchain:**
- github.com/pokt-network/poktroll v0.1.31-rc1 - Pocket Network protocol
- github.com/pokt-network/shannon-sdk 0.0.0-20250926214315 - Shannon SDK for crypto
- github.com/pokt-network/ring-go v0.1.1 - Ring signature verification
- github.com/pokt-network/go-dleq 0.0.0-20250925202155 - DLEQ proofs
- github.com/pokt-network/smt v0.14.1 - Sparse Merkle Trees (SMST)
- github.com/cometbft/cometbft v1.0.0-rc1 - Consensus engine (block events, RPC)
- github.com/cosmos/cosmos-sdk v0.53.0 - Blockchain framework (signing, transactions, keyring)

**Concurrency:**
- github.com/alitto/pond/v2 2.6.0 - Worker pool for bounded concurrency
- github.com/sourcegraph/conc v0.3.0 - Concurrency utilities (referenced in CLAUDE.md)
- context (stdlib) - Context for cancellation and timeouts

**Observability:**
- github.com/prometheus/client_golang v1.22.0 - Prometheus metrics
- github.com/prometheus/client_model v0.6.1 - Prometheus data model
- github.com/rs/zerolog v1.34.0 - Structured logging with JSON output

**Configuration:**
- gopkg.in/yaml.v2 2.4.0 - YAML parsing for config files
- gopkg.in/yaml.v3 3.0.1 - YAML v3 (secondary)
- github.com/spf13/viper 1.20.1 - Configuration management (transitive)
- github.com/fsnotify/fsnotify 1.9.0 - File watching for config reload

**Testing:**
- github.com/stretchr/testify v1.11.1 - Test assertions and mocking
- testing (stdlib) - Go standard testing package

**Performance & Utilities:**
- go.uber.org/automaxprocs v1.6.0 - Auto-set GOMAXPROCS for containerized environments
- github.com/hashicorp/go-version v1.7.0 - Version comparison
- golang.org/x/sync v0.17.0 - Sync utilities (transitive, used for coordination)

## Configuration

**Environment:**
- Configuration via YAML files (config.relayer.example.yaml, config.miner.example.yaml)
- Redis connection URLs support multiple modes:
  - Single: `redis://localhost:6379`
  - Sentinel: `redis-sentinel://sentinel1:26379,sentinel2:26379/mymaster`
  - Cluster: `redis://node1:6379,node2:6379,node3:6379`
- Pocket blockchain endpoints (RPC and gRPC)
- Supplier signing keys via YAML file, directory, or Cosmos keyring

**Key configs required:**
- `RELAYER_CONFIG_FILE` - Relayer configuration path
- `MINER_CONFIG_FILE` - Miner configuration path
- Redis URL (relayer and miner must match namespace prefixes)
- Pocket blockchain RPC/gRPC endpoints
- Supplier key locations (YAML file, keys directory, or keyring backend)

**Build:**
- `Makefile` with targets:
  - `make build` - Development build with versioning
  - `make build-release` - Production build (CGO_ENABLED=0, trimmed)
  - `make test` - Run tests
  - `make lint` - Run golangci-lint
  - `make fmt` - Format code
- Flags injected at build time:
  - `-X main.Version` - Git tag or "dev"
  - `-X main.Commit` - Git commit hash
  - `-X main.BuildDate` - Build timestamp (UTC)

## Platform Requirements

**Development:**
- Go 1.24.3 or later
- Linux/Darwin operating system
- Docker for containerization
- Kubernetes cluster (local with Tilt or remote)
- Redis instance (local or remote)
- Pocket blockchain node (RPC and gRPC endpoints)
- golangci-lint for linting
- GNU Make for build commands

**Production:**
- Deployment target: Kubernetes cluster
- Redis instance (single, sentinel, or cluster supported)
- Pocket blockchain node access (RPC on standard ports, gRPC on 9090)
- GOMAXPROCS auto-tuned via go.uber.org/automaxprocs
- HTTP ports: 8080 (relayer), 9090 (metrics), 6060 (pprof)
- Resource allocation: Per replica depends on throughput target
  - Target: 1000+ RPS per relayer replica
  - Memory: Pool connections depend on supplier count
  - CPU: Auto-tuned based on container limits

---

*Stack analysis: 2026-02-02*
