# Pocket RelayMiner Quality Hardening

## What This Is

A comprehensive quality hardening milestone for pocket-relay-miner — addressing tech debt accumulated during the 1-month rebuild, improving test coverage to enable fearless refactoring, and ensuring production stability for a system handling real money at 1000+ RPS.

## Core Value

Test confidence — comprehensive coverage that enables safe refactoring and prevents regressions.

## Requirements

### Validated

<!-- Existing capabilities from the codebase rebuild -->

- ✓ Multi-transport relay proxy (HTTP, WebSocket, gRPC, Streaming) — existing
- ✓ Ring signature validation for relay requests — existing
- ✓ Response signing with supplier private keys — existing
- ✓ Redis Streams for relay publishing (relayer) and consumption (miner) — existing
- ✓ SMST tree management for cryptographic proofs — existing
- ✓ Three-tier caching (L1 in-memory, L2 Redis, L3 blockchain) — existing
- ✓ Leader election for singleton operations — existing
- ✓ Session lifecycle state machine (ACTIVE → CLAIMABLE → PROOFABLE → SETTLED) — existing
- ✓ Claim/proof submission pipelines with batching — existing
- ✓ Supplier registry with hot-reload support — existing
- ✓ Prometheus metrics for observability — existing
- ✓ Structured logging with zerolog — existing
- ✓ Graceful shutdown with session draining — existing

### Active

<!-- Work to be done in this milestone -->

**Tech Debt Fixes:**
- [ ] Fix `mustMarshalJSON` panic in `miner/params_refresher.go` — add proper error handling
- [ ] Extract compute units from service config for WebSocket relays (currently hardcoded to 1)
- [ ] Extract compute units from service config for gRPC relays (currently hardcoded)
- [ ] Implement cache entity eviction for NotFound entities (memory leak prevention)
- [ ] Fix unstaking grace period enforcement (compare against session end height, not current)

**Bug Fixes:**
- [ ] Investigate session determinism logging in `cache/session_cache.go` — fix root cause or remove debug code
- [ ] Harden terminal session cleanup to prevent worker pool exhaustion on callback errors

**Test Coverage — Race Conditions:**
- [ ] Add race detection tests for `lifecycle_callback.go` state machine
- [ ] Add race detection tests for `session_lifecycle.go` state machine
- [ ] Add concurrent session state transition tests (Redis + in-memory map races)

**Test Coverage — Integration:**
- [ ] Add transport integration tests for `proxy.go` (all protocols under concurrent load)
- [ ] Add distributed cache invalidation tests (pub/sub across multiple instances)
- [ ] Add block subscriber reconnection chaos tests (Redis unavailability scenarios)
- [ ] Restore e2e tests downgraded to unit tests (claim_pipeline, submission_timing)

**Code Quality:**
- [ ] Review and potentially split large files (lifecycle_callback: 1898 lines, session_lifecycle: 1207 lines, proxy: 1842 lines)
- [ ] Ensure all tests pass `go test -race` without warnings
- [ ] Document fragile areas with modification guidelines

### Out of Scope

- WebSocket handshake signature verification — waiting on PATH team to finalize spec
- Historical params at block height — waiting on protocol fix for per-session params
- Ring params caching optimization — depends on historical params
- Session params per-relay caching — depends on historical params
- Performance-only optimizations — focus is correctness first

## Context

**Background:**
- Rebuilt from older pocket-relay-miner project in approximately 1 month
- Goal was scalability and horizontal scaling with Redis-backed state
- Rapid timeline led to some shortcuts and accumulated tech debt
- System is production-grade, handling real money on Pocket Network
- Target performance: 1000+ RPS per relayer replica

**Codebase Analysis:**
- Full codebase map available in `.planning/codebase/`
- CONCERNS.md documents 30+ items across tech debt, bugs, security, performance, fragility
- Large state machine files identified as high-risk for modification
- Some e2e tests downgraded to unit tests with miniredis

**External Dependencies:**
- PATH gateway team working on WebSocket handshake spec
- Pocket protocol team releasing historical params fix
- Both are out of our control and excluded from this milestone

## Constraints

- **Backwards Compatibility**: Changes must not break existing deployments
- **Test Quality**: No flaky tests, no race conditions, no mock/fake tests (Rule #1 from CLAUDE.md)
- **Real Implementations**: Use miniredis for Redis tests, not mocks
- **Race Safety**: All code must pass `go test -race` without warnings
- **Determinism**: Tests must be deterministic (no `time.Sleep()` dependencies)

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| Interleaved approach | Fix+test+refactor each area together for cohesion | — Pending |
| Full cleanup scope | Address all CONCERNS.md items (except external deps) | — Pending |
| Exclude external deps | PATH/protocol items are blocked, defer to future milestone | — Pending |
| Test confidence as goal | Enables safe future refactoring | — Pending |

---
*Last updated: 2026-02-02 after initialization*