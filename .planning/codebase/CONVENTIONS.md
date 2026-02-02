# Coding Conventions

**Analysis Date:** 2026-02-02

## Naming Patterns

**Files:**
- Lowercase with underscores: `redis_mapstore.go`, `relay_processor.go`, `proof_pipeline.go`
- Struct implementations: `{name}.go` (e.g., `relay_processor.go` contains `relayProcessor` struct)
- Test files: `{name}_test.go` (unit tests), `{name}_bench_test.go` (benchmarks)
- Test-tagged files: Add `//go:build test` at the top (used in `miner/` package)

**Functions:**
- PascalCase for exported functions: `NewRelayProcessor()`, `ProcessRelay()`, `GetServiceDifficulty()`
- camelCase for unexported receiver methods: `(rp *relayProcessor) validate()`, `(bp *BufferPool) getBuffer()`
- Constructor convention: `New{Type}()` (e.g., `NewRelayProcessor()`, `NewCacheOrchestrator()`)
- Helper functions: Use descriptive names with underscores in test code: `createTestRedisStore()`, `setupProofPipelineTest()`

**Variables:**
- Short receiver names: `rp *relayProcessor`, `bp *BufferPool`, `s *RedisSMSTTestSuite`
- Context parameters: Always named `ctx context.Context` (first parameter in most functions)
- Loop variables: `i`, `j` for simple counters; `entry`, `session`, `relay` for domain objects
- Error handling: `err` consistently used for error returns

**Types:**
- PascalCase for struct and interface names: `RelayProcessor`, `CacheOrchestrator`, `RedisMapStore`
- Config structs: `{Component}Config` (e.g., `ProofPipelineConfig`, `CacheOrchestratorConfig`)
- Interface names: `{Capability}Interface` or just `{Capability}` when conventional (e.g., `RelayProcessor`, `Deduplicator`)
- Type aliases: Used for clarity (e.g., `type Logger = zerolog.Logger`)

**Constants:**
- UPPERCASE with underscores for global constants: `MaxConcurrentStreams`, `DefaultIdleTimeout`, `GracefulShutdownTimeout`
- Rejection/drop reasons: lowercase with underscores (used as metric labels): `rejectReasonReadBodyError`, `dropReasonValidationFailed`
- Package-level constants grouped in `const ()` blocks with comments describing each group

## Code Style

**Formatting:**
- Tool: `gofmt -s` (simplified format mode, enforced via `make fmt`)
- Line length: No explicit limit, but keep functions readable (typically <30-50 lines)
- Indentation: Standard Go tab indentation (enforced by gofmt)
- Imports: Grouped in order: stdlib → external → internal packages (visible in `relayer/proxy.go`)

**Linting:**
- Tool: `golangci-lint` (configured via repository config, no `.golangci.yaml` file)
- Enforced checks: Pre-commit hooks automatically run linting before commits (`make install-hooks`)
- Coverage of checks: Standard Go linting rules (unused variables, unreachable code, etc.)

## Import Organization

**Order:**
1. Standard library packages (e.g., `"context"`, `"fmt"`, `"sync"`)
2. Third-party packages with major imports (e.g., `"google.golang.org/grpc"`)
3. External dependencies (e.g., `"github.com/alitto/pond/v2"`, `"github.com/redis/go-redis/v9"`)
4. Internal project packages (e.g., `"github.com/pokt-network/pocket-relay-miner/cache"`)

**Path Aliases:**
- Used for clarity: `sdktypes "github.com/pokt-network/shannon-sdk/types"`
- Used for proto types: `servicetypes "github.com/pokt-network/poktroll/x/service/types"`
- Internal packages: Rarely use aliases, imported directly: `"github.com/pokt-network/pocket-relay-miner/logging"`

**Example from `relayer/proxy.go`:**
```go
import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	...
	"github.com/alitto/pond/v2"
	sdktypes "github.com/pokt-network/shannon-sdk/types"
	"github.com/puzpuzpuz/xsync/v4"
	...
	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)
```

## Error Handling

**Patterns:**
- Always check errors explicitly: `if err != nil { ... }`
- Wrap errors with context using `fmt.Errorf()`: `fmt.Errorf("failed to build relay response: %w", err)`
- Log errors with context before returning: Use structured logging with `logger.Warn()` or `logger.Error()`
- Return errors up the stack: Never silently ignore errors or return blank errors
- Sentinel errors: Not used in this codebase; prefer explicit error checks

**Example from `relayer/relay_processor.go`:**
```go
result, err := processWithTimeout(ctx, relay)
if err != nil {
    return fmt.Errorf("processing failed: %w", err)
}

if err := relay.Validate(); err != nil {
    logger.Warn().
        Err(err).
        Str("session_id", relay.SessionID).
        Msg("relay validation failed")
    return fmt.Errorf("validation failed: %w", err)
}
```

**No Panic Policy:**
- Production code: Never use `panic()` in production paths
- Panic recovery: Used in gRPC interceptors to prevent crashes (`relayer/grpc_interceptor.go`)
- Test code: May use `panic()` or `t.Fatalf()` for setup failures

## Logging

**Framework:** Structured logging via `zerolog` (type alias `Logger = zerolog.Logger`)

**Initialization:**
- Use `logging.NewLoggerFromConfig()` with `logging.DefaultConfig()` or custom config
- Async logging enabled by default (non-blocking ring buffer for 1000+ RPS)
- Component loggers created via `logging.ForComponent(logger, componentName)`

**Patterns:**
- Chain methods for field addition: `logger.Info().Str("key", value).Int("count", n).Msg("message")`
- Always include `.Msg()` at the end of the chain
- Use appropriate log levels:
  - `Debug()`: Internal flow, variable values
  - `Info()`: State changes, significant events
  - `Warn()`: Recoverable errors, degraded operation
  - `Error()`: Failures, unrecoverable conditions
- Use error field: `.Err(err)` for error values
- Session context: Include session ID, application address in logs for tracing

**Example from `relayer/proxy.go`:**
```go
p.logger.Info().Str(logging.FieldListenAddr, p.config.ListenAddr).Msg("starting HTTP proxy server")

logging.WithSessionContext(p.logger.Error(), sessionCtx).
    Err(err).
    Str("service_id", serviceID).
    Msg("failed to process relay")

p.logger.Debug().
    Str("session_id", relay.SessionID).
    Int64("compute_units", result.ComputeUnits).
    Msg("relay processed successfully")
```

**Sensitive Data:**
- Never log private keys, credentials, or auth tokens
- Safe to log: Session IDs, application addresses, service IDs, block heights
- Sanitize error messages before logging to clients

## Comments

**When to Comment:**
- Complex algorithms or non-obvious logic (see `cache/orchestrator.go` or `miner/redis_mapstore.go`)
- Business logic rationale: Why a certain approach was chosen
- Performance notes: When optimization impacts readability
- Known limitations or TODOs: Use `// TODO: description` or `// FIXME: description`
- Exported functions/types: Brief description of purpose

**JSDoc/Godoc:**
- Exported functions: Start with function name (e.g., `// NewRelayProcessor creates a new relay processor.`)
- Exported structs: Document the struct and significant fields
- Exported methods: Brief description of what they do
- Style: One-line summary at start, followed by detailed comments if needed

**Example from `miner/redis_mapstore.go`:**
```go
// RedisMapStore implements kvstore.MapStore using Redis hashes with pipelining optimization.
// This enables shared storage across HA instances, avoiding local disk IOPS issues.
//
// The RedisMapStore uses a single Redis hash to store all key-value pairs for a session's SMST.
// This provides O(1) access for Get/Set/Delete operations and enables instant failover since
// all instances can access the same Redis data.
//
// Pipelining Optimization:
// During SMST Commit(), the library calls Set() 10-20 times for dirty nodes.
// Instead of 10-20 round trips (20-40ms), we buffer operations and flush in one HSET (2-3ms).
// This provides 8-10× speedup for relay processing.
//
// Redis Hash Structure:
//
//	Key: Built via KeyBuilder.SMSTNodesKey(sessionID)
//	Fields: hex-encoded SMST node keys
//	Values: raw SMST node data
type RedisMapStore struct {
	redisClient *redisutil.Client
	hashKey     string // Redis hash key built via KeyBuilder.SMSTNodesKey()
	ctx         context.Context
	// ...
}
```

## Function Design

**Size:**
- Typical range: 20-50 lines for most functions
- Large functions (100+ lines): Should be broken into helpers or packages
- Single responsibility: Each function does one thing well

**Parameters:**
- Context always first: `func (c *Component) DoWork(ctx context.Context, ...)`
- Related parameters grouped: Receiver, then context, then domain parameters
- Avoid parameter lists >5 parameters (use config structs instead)
- Use pointers for large structs, values for primitives

**Return Values:**
- Error always last: `func (c *Component) Action() (Result, error)`
- Single return type for success: `(result *Type, error)` not `(*Type, *OtherType, error)`
- Named return values: Not used (improves readability to omit them in most cases)

**Example from `miner/redis_mapstore.go`:**
```go
// Get retrieves a value from the Redis hash.
// Returns nil, nil if the key doesn't exist (per MapStore interface contract).
func (s *RedisMapStore) Get(key []byte) ([]byte, error) {
	start := time.Now()
	defer func() {
		observability.SMSTRedisOperationDuration.WithLabelValues("get").Observe(time.Since(start).Seconds())
	}()
	// ...
}
```

## Module Design

**Exports:**
- Interfaces exported for dependency injection (e.g., `RelayProcessor`, `Deduplicator`)
- Config structs exported so callers can configure (e.g., `ClaimPipelineConfig`)
- Factory functions exported (e.g., `NewRelayProcessor()`)
- Helper functions: Unexported if only used internally

**Barrel Files:**
- Not used in this codebase
- Each package stands alone with its exports clearly defined

**Package Structure:**
- One main type per file (e.g., `relay_processor.go` contains `RelayProcessor` interface and `relayProcessor` implementation)
- Related helpers in same file or grouped by purpose
- Test files co-located: `{name}_test.go` in same directory

**Concurrency Patterns:**
- Use `context.Context` for cancellation and timeouts (always first parameter)
- Use worker pools for bounded concurrency: `pond.Pool` or `alitto/pond/v2`
- Never spawn unbounded goroutines: Use `go func()` with caution (usually within worker pools)
- Protect shared state: `sync.RWMutex` or `xsync.MapOf` for lock-free reads
- Defer cleanup: Always defer `Close()` or cleanup functions

**Example from `cache/orchestrator.go`:**
```go
// Pond subpool for parallel cache refresh (I/O-bound network queries)
refreshSubpool pond.Pool

// Track entities - using xsync for lock-free performance
knownApps      *xsync.Map[string, struct{}]
knownServices  *xsync.Map[string, struct{}]
knownSuppliers *xsync.Map[string, struct{}]
```

---

*Convention analysis: 2026-02-02*
