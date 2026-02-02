# Requirements: Pocket RelayMiner Quality Hardening

**Defined:** 2026-02-02
**Core Value:** Test confidence — comprehensive coverage that enables safe refactoring

## v1 Requirements

Requirements for this quality hardening milestone. Each maps to roadmap phases.

### Test Infrastructure

- [ ] **INFRA-01**: Race detection enabled for all packages (not just miner) in CI and Makefile
- [ ] **INFRA-02**: golangci.yml configuration file with recommended linters (staticcheck, errcheck, govet, etc.)
- [ ] **INFRA-03**: govulncheck integrated into CI pipeline for vulnerability scanning
- [ ] **INFRA-04**: Coverage tracking with 80%+ enforcement on critical paths (miner/, relayer/, cache/)

### Test Quality

- [ ] **QUAL-01**: All time.Sleep() violations in tests replaced with proper synchronization (channels, contexts, Eventually assertions)
- [ ] **QUAL-02**: Flaky test audit completed — all tests pass 100/100 runs, flaky tests fixed or removed
- [ ] **QUAL-03**: Deterministic test data generation using seed-based generators (no random without seed)

### Characterization Tests

- [ ] **CHAR-01**: Characterization tests for `miner/lifecycle_callback.go` covering all state transitions
- [ ] **CHAR-02**: Characterization tests for `miner/session_lifecycle.go` covering session state machine
- [ ] **CHAR-03**: Characterization tests for `relayer/proxy.go` covering all transport protocols

### Code Structure

- [ ] **STRUCT-01**: Interface extraction for testability (ClaimBuilder, ProofBuilder, StateTransitionDeterminer, RequestProcessor)
- [ ] **STRUCT-02**: State handler packages extracted (miner/statehandler/, relayer/transport/) with handler registry pattern

### Tech Debt

- [ ] **DEBT-01**: Fix `mustMarshalJSON` panic in `miner/params_refresher.go` — add proper error handling
- [ ] **DEBT-02**: Extract compute units from service config for WebSocket relays (remove hardcoded value of 1)
- [ ] **DEBT-03**: Extract compute units from service config for gRPC relays (remove hardcoded value)
- [ ] **DEBT-04**: Implement cache entity eviction for NotFound entities (prevent memory growth)
- [ ] **DEBT-05**: Fix unstaking grace period enforcement (compare against session end height, not current)

### Bug Fixes

- [ ] **BUG-01**: Investigate and fix session determinism logging in `cache/session_cache.go` (or remove debug code if resolved)
- [ ] **BUG-02**: Harden terminal session cleanup to prevent worker pool exhaustion on callback errors

## v2 Requirements

Deferred to future milestone. Tracked but not in current roadmap.

### Advanced Testing

- **ADV-01**: Fuzzing for relay request parsing and input validation
- **ADV-02**: Property-based testing (Rapid) for SMST invariants
- **ADV-03**: Chaos testing with Toxiproxy for network failure scenarios
- **ADV-04**: Mutation testing audit with Gremlins for test quality validation
- **ADV-05**: Testcontainers for integration tests requiring 100% Redis compatibility

### Performance Optimization

- **PERF-01**: Ring params caching (blocked on historical params protocol fix)
- **PERF-02**: Session params per-relay caching (blocked on protocol fix)

## Out of Scope

Explicitly excluded. Documented to prevent scope creep.

| Feature | Reason |
|---------|--------|
| WebSocket handshake signature verification | Waiting on PATH team to finalize spec |
| Historical params at block height | Waiting on protocol fix |
| Ring signature caching | Depends on historical params |
| HTTP/2 specific testing | Low priority — most clients use HTTP/1.1 |
| Large file splitting | Focus on testability, not file size reduction |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

| Requirement | Phase | Status |
|-------------|-------|--------|
| INFRA-01 | Phase 1 - Test Foundation | Pending |
| INFRA-02 | Phase 1 - Test Foundation | Pending |
| INFRA-03 | Phase 1 - Test Foundation | Pending |
| INFRA-04 | Phase 2 - Characterization Tests | Pending |
| QUAL-01 | Phase 3 - Test Quality Cleanup | Pending |
| QUAL-02 | Phase 1 - Test Foundation | Pending |
| QUAL-03 | Phase 1 - Test Foundation | Pending |
| CHAR-01 | Phase 2 - Characterization Tests | Pending |
| CHAR-02 | Phase 2 - Characterization Tests | Pending |
| CHAR-03 | Phase 2 - Characterization Tests | Pending |
| STRUCT-01 | Phase 4 - Code Structure Refactoring | Pending |
| STRUCT-02 | Phase 4 - Code Structure Refactoring | Pending |
| DEBT-01 | Phase 5 - Tech Debt and Bug Fixes | Pending |
| DEBT-02 | Phase 5 - Tech Debt and Bug Fixes | Pending |
| DEBT-03 | Phase 5 - Tech Debt and Bug Fixes | Pending |
| DEBT-04 | Phase 5 - Tech Debt and Bug Fixes | Pending |
| DEBT-05 | Phase 5 - Tech Debt and Bug Fixes | Pending |
| BUG-01 | Phase 5 - Tech Debt and Bug Fixes | Pending |
| BUG-02 | Phase 5 - Tech Debt and Bug Fixes | Pending |

**Coverage:**
- v1 requirements: 19 total
- Mapped to phases: 19/19 (100%)
- Unmapped: 0

---
*Requirements defined: 2026-02-02*
*Last updated: 2026-02-02 after roadmap creation*
