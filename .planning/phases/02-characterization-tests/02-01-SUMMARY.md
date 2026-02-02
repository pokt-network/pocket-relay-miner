---
phase: 02-characterization-tests
plan: 01
subsystem: testing
tags: [testutil, miniredis, builders, deterministic, rule1]

# Dependency graph
requires:
  - phase: 01-test-foundation
    provides: CI infrastructure, race detection, stability testing
provides:
  - Shared testutil package for characterization tests
  - Deterministic test data generation (SessionBuilder, RelayBuilder)
  - RedisTestSuite base for miniredis integration tests
  - Hardcoded test cryptographic keys
affects: [02-02, 02-03, 02-04, miner-tests, relayer-tests, cache-tests]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Builder pattern for deterministic test data generation"
    - "Embedded test suite pattern (RedisTestSuite)"
    - "Seeded PRNG for reproducible tests (math/rand not crypto/rand)"

key-files:
  created:
    - testutil/testutil.go
    - testutil/keys.go
    - testutil/session_builder.go
    - testutil/relay_builder.go
    - testutil/redis_suite.go
    - testutil/testutil_test.go
  modified: []

key-decisions:
  - "Use math/rand with seed for test data, not crypto/rand"
  - "Hardcode test keys in source for reproducibility across CI runs"
  - "Single miniredis instance per test suite (SetupSuite) with FlushAll isolation (SetupTest)"

patterns-established:
  - "SessionBuilder: testutil.NewSessionBuilder(seed).WithState(x).Build()"
  - "RelayBuilder: testutil.NewRelayBuilder(seed).WithWeight(n).BuildN(count)"
  - "RedisTestSuite: embed in test suite for miniredis + helpers"

# Metrics
duration: 4min
completed: 2026-02-02
---

# Phase 2 Plan 01: Test Utilities Summary

**Shared testutil package with deterministic builders, hardcoded keys, and embeddable RedisTestSuite for Rule #1 compliant tests**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-02T22:30:34Z
- **Completed:** 2026-02-02T22:34:51Z
- **Tasks:** 3
- **Files created:** 6

## Accomplishments

- Created testutil package foundation with test mode detection and deterministic data generation
- Implemented SessionBuilder and RelayBuilder with fluent API for reproducible test data
- Added RedisTestSuite embeddable base suite with miniredis and helper methods
- Verified all tests pass with -race flag and 10 consecutive runs (100% pass rate)

## Task Commits

Each task was committed atomically:

1. **Task 1: Create testutil package foundation** - `3e16a5d` (feat)
2. **Task 2: Create builder patterns for test data** - `10bf6f6` (feat)
3. **Task 3: Create Redis test suite base** - `f60f49f` (test)

## Files Created

- `testutil/testutil.go` - Test mode helpers (GetTestConcurrency, IsNightlyMode, GenerateDeterministicBytes)
- `testutil/keys.go` - Hardcoded secp256k1 test keys (supplier, app, variants for multi-entity tests)
- `testutil/session_builder.go` - Fluent builder for miner.SessionSnapshot
- `testutil/relay_builder.go` - Fluent builder for TestRelay (key, value, weight)
- `testutil/redis_suite.go` - Embeddable test suite with miniredis and helpers
- `testutil/testutil_test.go` - Comprehensive verification tests

## Decisions Made

1. **Use math/rand with seed for determinism** - crypto/rand would make tests non-reproducible
2. **Hardcode test keys** - Ensures CI runs produce identical results
3. **Single miniredis per suite** - Prevents CPU exhaustion from creating thousands of instances
4. **FlushAll in SetupTest** - Guarantees test isolation without performance penalty

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None - all tasks completed without issues.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

**Ready for 02-02:** The testutil package provides all infrastructure needed for:
- Session lifecycle characterization tests (uses SessionBuilder, RedisTestSuite)
- SMST operations characterization tests (uses RelayBuilder, RedisTestSuite)
- Cache behavior characterization tests (uses RedisTestSuite)

**Artifacts available:**
- `testutil.GetTestConcurrency()` - 100 for CI, 1000 for nightly
- `testutil.NewSessionBuilder(seed).Build()` - Deterministic SessionSnapshot
- `testutil.NewRelayBuilder(seed).BuildN(n)` - Deterministic relay batches
- `testutil.RedisTestSuite` - Embed for miniredis-backed tests

---
*Phase: 02-characterization-tests*
*Completed: 2026-02-02*
