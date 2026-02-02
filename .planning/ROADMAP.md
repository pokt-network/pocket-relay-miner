# Roadmap: Pocket RelayMiner Quality Hardening

**Milestone:** Quality Hardening and Test Coverage
**Depth:** Standard (6 phases)
**Coverage:** 19/19 requirements mapped

## Overview

Transform pocket-relay-miner from a fast rebuild to a maintainable, test-confident codebase. This roadmap addresses accumulated tech debt, adds comprehensive test coverage (80%+ on critical paths), and refactors large state machines for long-term sustainability. Each phase maintains working code with passing tests, enabling safe future changes.

## Phases

### Phase 1: Test Foundation

**Goal:** Establish baseline test coverage and infrastructure before any refactoring

**Dependencies:** None (foundational)

**Requirements:**
- INFRA-01: Race detection enabled for all packages in CI and Makefile
- INFRA-02: golangci.yml configuration with recommended linters
- INFRA-03: govulncheck integrated into CI pipeline
- QUAL-02: Flaky test audit completed — all tests pass 100/100 runs
- QUAL-03: Deterministic test data generation using seed-based generators

**Success Criteria:**
1. All tests pass `go test -race` across all packages without warnings
2. golangci-lint runs in CI with staticcheck, errcheck, govet, and 10+ additional linters
3. govulncheck runs in CI and fails on HIGH/CRITICAL vulnerabilities
4. Test audit document created identifying all time.Sleep() violations, global state dependencies, and non-deterministic data generation
5. Flaky test report shows 100/100 pass rate for all existing tests

**Plans:** 4 plans

Plans:
- [x] 01-01-PLAN.md — Create strict golangci-lint configuration
- [x] 01-02-PLAN.md — Enable race detection and create stability script
- [x] 01-03-PLAN.md — Update CI with govulncheck and nightly stability
- [x] 01-04-PLAN.md — Create test quality audit and run stability validation

---

### Phase 2: Characterization Tests

**Goal:** Add comprehensive test coverage for large state machines to enable safe refactoring

**Dependencies:** Phase 1 (needs test infrastructure)

**Requirements:**
- CHAR-01: Characterization tests for miner/lifecycle_callback.go covering all state transitions
- CHAR-02: Characterization tests for miner/session_lifecycle.go covering session state machine
- CHAR-03: Characterization tests for relayer/proxy.go covering all transport protocols
- INFRA-04: Coverage tracking with 80%+ enforcement on critical paths

**Success Criteria:**
1. lifecycle_callback.go has tests covering all state transitions (ACTIVE → CLAIMABLE → PROOFABLE → SETTLED/FAILED)
2. session_lifecycle.go has tests for session discovery, relay accumulation, claim triggers, and proof triggers
3. proxy.go has tests for HTTP, WebSocket, gRPC, and Streaming transports under concurrent load
4. Coverage reports show 80%+ for miner/, relayer/, and cache/ packages
5. All new tests use miniredis for Redis (no mocks), pass race detector, and run deterministically

---

### Phase 3: Test Quality Cleanup

**Goal:** Eliminate flaky test anti-patterns and improve test maintainability

**Dependencies:** Phase 2 (needs characterization tests to validate no regressions)

**Requirements:**
- QUAL-01: All time.Sleep() violations in tests replaced with proper synchronization

**Success Criteria:**
1. All 64+ time.Sleep() violations in tests replaced with channels, contexts, require.Eventually(), or testing/synctest
2. Test suite execution time reduced by eliminating arbitrary waits
3. All tests pass 1000/1000 runs (validated via CI script)
4. Table-driven test format adopted for repetitive test cases (30% test code reduction)
5. Test utilities extracted to shared files (redis_test_utils.go, session_test_helpers.go)

---

### Phase 4: Code Structure Refactoring

**Goal:** Extract interfaces and handlers to reduce file sizes and improve testability

**Dependencies:** Phase 3 (needs clean tests to validate refactoring)

**Requirements:**
- STRUCT-01: Interface extraction for testability (ClaimBuilder, ProofBuilder, StateTransitionDeterminer, RequestProcessor)
- STRUCT-02: State handler packages extracted with handler registry pattern

**Success Criteria:**
1. Interfaces defined: ClaimBuilder, ProofBuilder, StateTransitionDeterminer, RequestProcessor (2-5 methods each)
2. New packages created: miner/statehandler/, relayer/transport/ with handler files
3. State handlers extracted: active.go, claiming.go, claimed.go, proving.go, terminal.go (miner side)
4. Transport handlers extracted: jsonrpc.go, grpc.go, websocket.go, rest.go, streaming.go (relayer side)
5. Large files reduced by 50%+: lifecycle_callback.go (<950 lines), session_lifecycle.go (<600 lines), proxy.go (<920 lines)

---

### Phase 5: Tech Debt and Bug Fixes

**Goal:** Resolve technical debt items and harden critical error paths

**Dependencies:** Phase 4 (refactored code is easier to fix)

**Requirements:**
- DEBT-01: Fix mustMarshalJSON panic — add proper error handling
- DEBT-02: Extract compute units from service config for WebSocket relays
- DEBT-03: Extract compute units from service config for gRPC relays
- DEBT-04: Implement cache entity eviction for NotFound entities
- DEBT-05: Fix unstaking grace period enforcement
- BUG-01: Investigate and fix session determinism logging
- BUG-02: Harden terminal session cleanup to prevent worker pool exhaustion

**Success Criteria:**
1. mustMarshalJSON replaced with marshalJSON() returning error (no panics in params_refresher.go)
2. WebSocket relays compute units loaded from service config (hardcoded value of 1 removed)
3. gRPC relays compute units loaded from service config (hardcoded value removed)
4. Cache implements LRU eviction for NotFound entities with configurable max size
5. Unstaking grace period compares against session end height (not current block height)
6. Session determinism logging either fixed (root cause identified) or removed (if already resolved)
7. Terminal session cleanup wrapped in error recovery to prevent worker pool exhaustion

---

### Phase 6: Validation and Hardening

**Goal:** Validate all changes, enforce quality gates, and document improvements

**Dependencies:** Phase 5 (all changes complete)

**Requirements:** None (validation phase)

**Success Criteria:**
1. Benchmark comparison shows no >10% performance regression on critical paths (SMST ops, relay validation, cache hits)
2. Coverage enforcement added to CI: fail build if miner/relayer/cache drops below 80%
3. Pre-commit hooks created for race detection and linting (optional installation via make target)
4. Integration tests added using testcontainers for distributed cache invalidation and block subscriber reconnection
5. Documentation updated: CLAUDE.md reflects new structure, ADRs written for major decisions (state handlers, transport handlers, builder pattern)

---

## Progress

| Phase | Status | Requirements | Progress |
|-------|--------|--------------|----------|
| Phase 1: Test Foundation | ✓ Complete | INFRA-01, INFRA-02, INFRA-03, QUAL-02, QUAL-03 | 100% |
| Phase 2: Characterization Tests | Pending | CHAR-01, CHAR-02, CHAR-03, INFRA-04 | 0% |
| Phase 3: Test Quality Cleanup | Pending | QUAL-01 | 0% |
| Phase 4: Code Structure Refactoring | Pending | STRUCT-01, STRUCT-02 | 0% |
| Phase 5: Tech Debt and Bug Fixes | Pending | DEBT-01, DEBT-02, DEBT-03, DEBT-04, DEBT-05, BUG-01, BUG-02 | 0% |
| Phase 6: Validation and Hardening | Pending | None (validation) | 0% |

---

*Roadmap created: 2026-02-02*
*Last updated: 2026-02-02 (Phase 1 complete)*
