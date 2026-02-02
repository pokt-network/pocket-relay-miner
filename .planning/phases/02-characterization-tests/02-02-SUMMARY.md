---
phase: 02-characterization-tests
plan: 02
subsystem: miner-lifecycle
tags: [tests, characterization, lifecycle, concurrent]
dependency_graph:
  requires: ["02-01"]
  provides: ["lifecycle-callback-coverage", "concurrent-test-patterns"]
  affects: ["02-03", "02-04"]
tech_stack:
  added: []
  patterns: ["testify/suite", "miniredis-per-suite", "thread-safe-mocks"]
key_files:
  created:
    - miner/lifecycle_callback_states_test.go
    - miner/lifecycle_callback_concurrent_test.go
  modified: []
decisions:
  - id: local-mocks-over-testutil
    rationale: "Import cycle prevented using testutil (testutil imports miner)"
  - id: fixed-session-heights
    rationale: "Deterministic window calculations require fixed heights"
  - id: characterize-actual-behavior
    rationale: "Document CreateClaims called even with 0 valid sessions"
metrics:
  duration: "~5 min"
  completed: "2026-02-02"
---

# Phase 2 Plan 2: Lifecycle Callback Characterization Tests Summary

Comprehensive characterization tests for miner/lifecycle_callback.go covering state transitions and concurrent operations.

## One-liner

State transition and concurrency tests for LifecycleCallback claim/proof paths using miniredis (31 tests, 100+ goroutines).

## Execution Summary

| Task | Description | Commit | Files |
|------|-------------|--------|-------|
| 1 | State transition tests | 16785ac | miner/lifecycle_callback_states_test.go (941 lines) |
| 2 | Concurrent tests | 35bd356 | miner/lifecycle_callback_concurrent_test.go (883 lines) |

## What Was Built

### Task 1: State Transition Tests (lifecycle_callback_states_test.go)

**Claim Path Tests (7 tests):**
- `TestOnSessionsNeedClaim_Success` - Happy path with SMST flush and TX submission
- `TestOnSessionsNeedClaim_BatchMultipleSessions` - Multiple sessions in single TX
- `TestOnSessionsNeedClaim_FlushTreeError` - SMST flush failure handling
- `TestOnSessionsNeedClaim_TxSubmissionError` - TX broadcast failure handling
- `TestOnSessionsNeedClaim_WindowTimeout` - Claim window close detection
- `TestOnSessionsNeedClaim_ZeroRelaysSkipped` - Sessions with 0 relays filtered
- `TestOnSessionsNeedClaim_AlreadyClaimedSkipped` - Deduplication by ClaimTxHash

**Proof Path Tests (6 tests):**
- `TestOnSessionsNeedProof_Success` - Happy path with proof generation
- `TestOnSessionsNeedProof_BatchMultipleSessions` - Multiple proofs in single TX
- `TestOnSessionsNeedProof_ProveClosestError` - SMST proof generation failure
- `TestOnSessionsNeedProof_TxSubmissionError` - Proof TX failure handling
- `TestOnSessionsNeedProof_WindowTimeout` - Proof window close detection
- `TestOnSessionsNeedProof_AlreadyProvedSkipped` - Deduplication by ProofTxHash

**Terminal State Tests (7 tests):**
- `TestOnSessionProved_CleansResources` - SMST deletion + dedup cleanup
- `TestOnProbabilisticProved_CleansResources` - Same cleanup path
- `TestOnClaimWindowClosed_CleansResources` - Failure path cleanup
- `TestOnClaimTxError_CleansResources` - TX error cleanup
- `TestOnProofWindowClosed_CleansResources` - Proof window failure cleanup
- `TestOnProofTxError_CleansResources` - Proof TX error cleanup
- `TestOnSessionActive_Informational` - No-op callback

### Task 2: Concurrent Tests (lifecycle_callback_concurrent_test.go)

**Concurrent Claim Tests (3 tests):**
- `TestConcurrentClaimSubmissions` - 100+ goroutines submitting claims
- `TestClaimDuringProof` - Interleaved claim and proof operations
- `TestConcurrentSameSession` - Lock contention on same session

**Concurrent Proof Tests (2 tests):**
- `TestConcurrentProofSubmissions` - 100+ goroutines submitting proofs
- `TestProofDuringCleanup` - Proof generation during terminal callbacks

**Race Condition Tests (3 tests):**
- `TestSessionLockContention` - Session coordinator lock testing
- `TestConcurrentCallbacksNoRace` - All callback types concurrently
- `TestCleanupDuringActiveOperation` - Race between active op and cleanup

## Technical Decisions

### 1. Local Mocks Instead of testutil

**Decision:** Created `lc*` and `conc*` prefixed mock types locally instead of using testutil.

**Rationale:** Import cycle - testutil imports miner (for SessionSnapshot), and miner tests can't import testutil.

**Files affected:** Both test files duplicate mock implementations

### 2. Fixed Session Heights for Determinism

**Decision:** Use fixed session heights (SessionEndHeight=100) with known window calculations.

**Rationale:** Window calculations depend on sharedtypes.Params. Fixed heights ensure:
- Claim window opens at 102, closes at 106
- Proof window opens at 106, closes at 110
- Tests set block height explicitly for each scenario

### 3. Characterize Actual Behavior

**Decision:** Document that CreateClaims is called even when all sessions are filtered (0 valid messages).

**Rationale:** Characterization tests should capture actual behavior, not expected behavior. This documents a potential optimization opportunity.

## Coverage Analysis

| Function | Coverage |
|----------|----------|
| OnSessionsNeedClaim | 70.5% |
| OnSessionsNeedProof | 59.9% |
| OnSessionProved | 76.5% |
| OnProbabilisticProved | 72.2% |
| OnClaimWindowClosed | 75.0% |
| OnClaimTxError | 75.0% |
| OnProofWindowClosed | 75.0% |
| OnProofTxError | 75.0% |

**Note:** OnSessionsNeedProof at 59.9% is slightly below 70% target. Remaining coverage requires:
- Probabilistic proof skip path (needs proofChecker mock)
- Stream deletion paths
- Submission tracker paths

## Verification Results

```
# All tests pass with race detection
go test -race -v ./miner/... -run "TestLifecycleCallback.*"
--- PASS: TestLifecycleCallbackConcurrentSuite (0.25s)
--- PASS: TestLifecycleCallbackStatesSuite (0.03s)
ok      github.com/pokt-network/pocket-relay-miner/miner    1.415s

# 10/10 stability runs pass
for i in {1..10}; do go test -race -shuffle=on ./miner/... -run "TestLifecycleCallback.*"; done
# All 10 runs: ok
```

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Import cycle prevented testutil usage**
- **Found during:** Task 1 initial implementation
- **Issue:** testutil imports miner package, creating cycle
- **Fix:** Created local mock types with `lc*` and `conc*` prefixes
- **Files modified:** Both test files

**2. [Rule 1 - Bug] Race condition in TestConcurrentSameSession**
- **Found during:** Task 2 concurrent tests
- **Issue:** Shared int32 pointer accessed without synchronization
- **Fix:** Used shared sync.Mutex for counter access
- **Commit:** 35bd356

**3. [Rule 3 - Blocking] Window timing issues in concurrent tests**
- **Found during:** Task 2 concurrent tests
- **Issue:** Claim and proof goroutines sharing block client caused timing conflicts
- **Fix:** Used different session heights so both work at same block height (102)
- **Commit:** 35bd356

## Test Infrastructure Patterns

### Thread-Safe Mock Pattern

```go
type lcTestSMSTManager struct {
    mu          sync.Mutex
    flushCalls  int
    flushErr    error
}

func (m *lcTestSMSTManager) FlushTree(ctx context.Context, sessionID string) ([]byte, error) {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.flushCalls++
    if m.flushErr != nil {
        return nil, m.flushErr
    }
    return []byte("mock-root-hash-" + sessionID), nil
}
```

### Concurrent Test Pattern

```go
func (s *Suite) TestConcurrentOps() {
    numGoroutines := getConcurrency()
    var wg sync.WaitGroup
    wg.Add(numGoroutines)

    errors := make(chan error, numGoroutines)

    for i := 0; i < numGoroutines; i++ {
        go func(idx int) {
            defer wg.Done()
            // Create unique snapshot per goroutine
            snapshot := s.newTestSnapshot(idx)
            // Execute operation
            if err := s.callback.DoSomething(s.ctx, snapshot); err != nil {
                errors <- err
            }
        }(i)
    }

    wg.Wait()
    close(errors)

    var errList []error
    for err := range errors {
        errList = append(errList, err)
    }
    s.Require().Empty(errList)
}
```

## Next Phase Readiness

**Ready for 02-03 (SMST Operations):**
- Session and Redis patterns established
- Mock infrastructure patterns available for reference
- Window calculation patterns documented

**Prerequisites complete:**
- miniredis pattern for SMST testing
- Thread-safe mock patterns
- Deterministic snapshot generation

## Files Created

| File | Lines | Purpose |
|------|-------|---------|
| miner/lifecycle_callback_states_test.go | 941 | State transition tests |
| miner/lifecycle_callback_concurrent_test.go | 883 | Concurrent operation tests |

**Total lines added:** 1,824

## Open Questions

None - all technical decisions resolved during execution.
