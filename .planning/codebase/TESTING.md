# Testing Patterns

**Analysis Date:** 2026-02-02

## Test Framework

**Runner:**
- Go standard testing: `go test`
- Suite framework: `github.com/stretchr/testify/suite` for structured test suites
- Assertions: `github.com/stretchr/testify/require` for assertions

**Build Constraints:**
- Test-only code: Marked with `//go:build test` at the top of file (required in `miner/` package)
- Used to exclude test utilities from production builds
- Example: `miner/redis_smst_utils_test.go`, `miner/redis_mapstore_test.go`

**Run Commands:**
```bash
make test              # Run all tests sequentially with -p 4 -parallel 4
make test PKG=cache    # Run cache tests sequentially (143 tests with shared miniredis)
make test PKG=miner    # Run miner tests in parallel
make test_miner        # Run miner tests with race detection (Rule #1: no flakes/races)
make test-coverage     # Generate coverage report (coverage.html)
go test -race ./...    # Run with race detector enabled
```

**Cache Tests Special Handling:**
- Cache package runs sequentially due to shared miniredis instance across tests
- Makefile rule: `go test -p 1 -parallel 1 ./cache/...` (avoid concurrent miniredis creation)
- Other packages: `go test -p 4 -parallel 4 ./...` (safe for parallel execution)

## Test File Organization

**Location:**
- Co-located with source: `{name}_test.go` in same directory as implementation
- Benchmarks: `{name}_bench_test.go` for performance tests
- Test utilities: `{name}_utils_test.go` or `setup_test.go` for shared test infrastructure

**Naming:**
- Test functions: `Test{ComponentName}_{Feature}` (e.g., `TestRedisMapStore_GetEmpty`, `TestRedisSMSTManager_UpdateBasic`)
- Benchmark functions: `Benchmark{Operation}` (e.g., `BenchmarkRedisMapStore_Get`, `BenchmarkRedisMapStore_Set`)
- Helper functions: `{action}Test{Variant}` (e.g., `createTestRedisStore()`, `setupProofPipelineTest()`)

**Test Suites:**
- Use testify suite pattern: Define `{Name}Suite struct` embedding `suite.Suite`
- Methods: `SetupSuite()` (once before all), `SetupTest()` (before each), `TearDownSuite()` (once after all)
- Example file structure in `miner/redis_smst_utils_test.go`:
  - Define `RedisSMSTTestSuite struct`
  - Implement `SetupSuite()`, `SetupTest()`, `TearDownSuite()`
  - Add helper methods: `createTestRedisStore()`, `createTestRedisSMSTManager()`
  - All test methods use receiver: `func (s *RedisSMSTTestSuite) TestName()`

## Test Structure

**Suite Organization:**
```go
// miner/redis_smst_utils_test.go (test utilities)
type RedisSMSTTestSuite struct {
	suite.Suite
	miniRedis   *miniredis.Miniredis
	redisClient *redisutil.Client
	ctx         context.Context
}

// SetupSuite runs ONCE before all tests
func (s *RedisSMSTTestSuite) SetupSuite() {
	// Create shared miniredis instance
	mr, err := miniredis.Run()
	s.Require().NoError(err)
	s.miniRedis = mr
	// ...
}

// SetupTest runs BEFORE each test
func (s *RedisSMSTTestSuite) SetupTest() {
	// Flush all data to ensure test isolation
	s.miniRedis.FlushAll()
}

// TearDownSuite runs ONCE after all tests
func (s *RedisSMSTTestSuite) TearDownSuite() {
	if s.miniRedis != nil {
		s.miniRedis.Close()
	}
	if s.redisClient != nil {
		s.redisClient.Close()
	}
}

// Helper methods
func (s *RedisSMSTTestSuite) createTestRedisStore(sessionID string) *RedisMapStore {
	store := NewRedisMapStore(s.ctx, s.redisClient, sessionID)
	redisStore, ok := store.(*RedisMapStore)
	s.Require().True(ok, "should return *RedisMapStore")
	return redisStore
}
```

**Patterns:**
- Assertions: `s.Require().NoError(err)`, `s.Require().Equal(expected, actual)`, `s.Require().True(condition)`
- Test messages: Always include message as last parameter: `s.Require().Equal(expected, actual, "should have 1 relay")`
- Setup helpers: Use `s.Helper()` in helper functions to improve error reporting
- Context: Use `s.ctx` (set in `SetupSuite()`) for all context-requiring calls

**Example from `miner/redis_mapstore_test.go`:**
```go
func (s *RedisSMSTTestSuite) TestRedisMapStore_SetGet() {
	store := s.createTestRedisStore("test-session-set-get")

	key := []byte("test-key")
	value := []byte("test-value")

	// Set value
	err := store.Set(key, value)
	s.Require().NoError(err, "Set should not error")

	// Get value
	gotValue, err := store.Get(key)
	s.Require().NoError(err, "Get should not error")
	s.Require().Equal(value, gotValue, "Get should return the same value that was set")
}
```

## Mocking

**Framework:** `github.com/stretchr/testify/mock` available but NOT preferred

**Patterns:**
- Prefer real implementations: miniredis for Redis, real protobuf types
- Mock only when necessary: External APIs (blockchain RPC, gRPC), randomized sources
- Mock pattern: Define interface, create simple mock struct with function pointers

**Real Implementations Preferred:**
- Redis: Use `miniredis/v2` (in-process Redis for testing) instead of mocks
- Protocol types: Use real proto types from `github.com/pokt-network/poktroll`
- Context: Use real `context.Background()` or `context.WithCancel()`

**Mock Example from `miner/proof_pipeline_test.go`:**
```go
// mockSMSTProver implements SMSTProver for testing
type mockSMSTProver struct {
	mu               sync.Mutex
	proveClosestFunc func(ctx context.Context, sessionID string, path []byte) ([]byte, error)
	getClaimRootFunc func(ctx context.Context, sessionID string) ([]byte, error)
	proveCallCount   int
	getRootCallCount int
}

func (m *mockSMSTProver) ProveClosest(ctx context.Context, sessionID string, path []byte) ([]byte, error) {
	m.mu.Lock()
	m.proveCallCount++
	m.mu.Unlock()
	if m.proveClosestFunc != nil {
		return m.proveClosestFunc(ctx, sessionID, path)
	}
	return []byte("mock-proof-bytes"), nil
}

// Safe call to get count without race
func (m *mockSMSTProver) getProveCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proveCallCount
}
```

**Rule #1 Compliance:**
- NO flaky tests: No `time.Sleep()`, no timing dependencies, deterministic ordering
- NO race conditions: Use mutexes for shared state in mocks, pass `-race` flag
- NO mock abuse: Real implementations preferred (miniredis is real, not a mock)
- NO timeout issues: Tests should be fast (<1s each) and never depend on timeouts

## Fixtures and Factories

**Test Data:**
- Small constants: Defined in test file (e.g., `testPrivKeyHex`, `testExpectedAddress`)
- Builders: Use constructor functions: `setupProofPipelineTest()` returns initialized components

**Location:**
- In same test file: Small test data and setup functions
- In utils file: Shared setup across multiple test files (e.g., `redis_smst_utils_test.go`)
- Fixtures pattern: Create test-specific variants (e.g., `createTestRedisStore()`, `createTestRedisSMSTManager()`)

**Example from `client/relay_client/signer_test.go`:**
```go
const (
	// Valid test private key (32 bytes hex)
	testPrivKeyHex = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"
	// Expected address derived from testPrivKeyHex
	testExpectedAddress = "pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4"
)

func TestNewSignerFromHex_ValidKey(t *testing.T) {
	signer, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err, "NewSignerFromHex should succeed with valid key")
	require.NotNil(t, signer, "Signer should not be nil")
	require.Equal(t, testExpectedAddress, signer.address, "Address should match expected value")
}
```

## Coverage

**Requirements:**
- Enforced: `make test-coverage` generates report
- Target: No explicit minimum (developer responsibility to test critical paths)
- View coverage: `make test-coverage` creates `coverage.html`

**Coverage Goals:**
- Critical business logic: 100% (relay processing, SMST operations, claim/proof submission)
- Infrastructure: 70%+ (caching, Redis operations)
- Configuration: 50%+ (config parsing, validation)

**Check Coverage:**
```bash
make test-coverage
# Opens coverage.html in browser with highlighted coverage
```

## Test Types

**Unit Tests:**
- Scope: Single function or method
- Dependencies: Mocked (or use real in-process implementations like miniredis)
- Files: `{name}_test.go` co-located with source
- Duration: <100ms each
- Example: `TestRedisMapStore_Get`, `TestNewSignerFromHex_ValidKey`

**Integration Tests:**
- Scope: Multiple components working together
- Dependencies: Real implementations (miniredis for Redis, real proto types)
- Pattern: Combine multiple test helpers into end-to-end flow
- Example: `TestRedisSMSTManager_UpdateBasic` (updates tree, gets stats, flushes, verifies consistency)
- Duration: <500ms each

**Benchmarks:**
- Marked: `//go:build test`, function name starts with `Benchmark`
- Purpose: Measure performance of critical paths (SMST operations, Redis calls, signing)
- Run: `go test -bench=. -benchmem ./package`
- Example: `BenchmarkRedisMapStore_Get`, `BenchmarkRedisMapStore_Set`
- Output: Operations per second, bytes allocated, allocs per operation

**Example from `miner/redis_smst_bench_test.go`:**
```go
// BenchmarkRedisMapStore_Get benchmarks HGET operation.
func BenchmarkRedisMapStore_Get(b *testing.B) {
	suite := setupBenchSuite(b)
	defer suite.Cleanup()

	store := suite.createTestRedisStore("bench-get")
	key := []byte("test-key")
	value := []byte("test-value")

	// Setup
	err := store.Set(key, value)
	if err != nil {
		b.Fatalf("failed to set value: %v", err)
	}

	// Benchmark
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.Get(key)
		if err != nil {
			b.Fatalf("Get failed: %v", err)
		}
	}
}
```

**E2E/Load Tests:**
- Not in standard test suite
- Located: `cmd/relay/http.go` has built-in load test mode
- Run: `./scripts/test-simple-relay.sh` or custom relay test scripts
- Purpose: Validate relay processing under realistic load (1000+ RPS)

## Common Patterns

**Async Testing:**
```go
// Wait for goroutine completion with context timeout
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

done := make(chan error, 1)
go func() {
	done <- component.Start(ctx)
}()

select {
case err := <-done:
	s.Require().NoError(err)
case <-ctx.Done():
	s.Fail("operation timed out")
}
```

**Error Testing:**
```go
func (s *RedisSMSTTestSuite) TestRedisMapStore_DeleteNonExistent() {
	store := s.createTestRedisStore("test-delete-nonexistent")

	// Delete non-existent key should not error (per MapStore contract)
	err := store.Delete([]byte("non-existent"))
	s.Require().NoError(err, "Delete of non-existent key should not error")
}

// Error from invalid input
func TestNewSignerFromHex_InvalidHex(t *testing.T) {
	signer, err := NewSignerFromHex("not-valid-hex")
	require.Error(t, err, "should fail with invalid hex")
	require.Nil(t, signer, "should return nil on error")
	require.Contains(t, err.Error(), "invalid private key hex")
}
```

**Race Detection:**
- Always run: `go test -race ./...` before committing (catches data races)
- Critical for: Concurrency testing, goroutine safety
- Command: `make test_miner` runs miner tests with `-race` flag
- This is Rule #1: No race conditions allowed

**Miniredis Usage:**
```go
// Create once per suite (avoid instance explosion)
func (s *RedisSMSTTestSuite) SetupSuite() {
	mr, err := miniredis.Run()
	s.Require().NoError(err)
	s.miniRedis = mr

	// Create client
	redisURL := fmt.Sprintf("redis://%s", mr.Addr())
	client, err := redisutil.NewClient(s.ctx, redisutil.ClientConfig{URL: redisURL})
	s.Require().NoError(err)
	s.redisClient = client
}

// Flush between tests for isolation
func (s *RedisSMSTTestSuite) SetupTest() {
	s.miniRedis.FlushAll()
}

// Clean up after all tests
func (s *RedisSMSTTestSuite) TearDownSuite() {
	if s.miniRedis != nil {
		s.miniRedis.Close()
	}
	if s.redisClient != nil {
		s.redisClient.Close()
	}
}
```

## Performance Benchmarking

**Critical Paths to Benchmark:**
- SMST operations: Get, Set, Delete on Redis
- Relay processing: Validation, signing, publishing
- Cache operations: L1 hits, L2 hits, L3 fetches
- Metrics recording: Histogram operations

**Run Benchmarks:**
```bash
# Single benchmark
go test -bench=BenchmarkRedisMapStore_Get -benchmem ./miner

# All benchmarks in package
go test -bench=. -benchmem ./miner

# With CPU profile
go test -bench=. -cpuprofile=cpu.prof -benchmem ./package
go tool pprof cpu.prof
```

**Interpreting Results:**
```
BenchmarkRedisMapStore_Get-8    36651    29734 ns/op    632 B/op    27 allocs/op
                    ↑           ↑        ↑              ↑          ↑
                    Name      Iterations  Nanosecs/op  Bytes/op  Allocs/op
```

**Performance Targets (from CLAUDE.md):**
- HSET (Set): ~29.7 µs/op
- HGET (Get): ~28.5 µs/op
- HDEL (Delete): ~29.2 µs/op
- HLEN (Len): ~27.7 µs/op
- L1 cache hit: <100ns
- L2 cache hit: <2ms
- L3 miss: <100ms

---

*Testing analysis: 2026-02-02*
