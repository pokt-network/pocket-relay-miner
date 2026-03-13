---
phase: 01-backend-pool-foundation
plan: 01
subsystem: relayer
tags: [pool, config, yaml, atomic, backend-selection]

requires: []
provides:
  - "pool/ package with Selector interface, BackendEndpoint, Pool data structures"
  - "BackendConfig with multi-URL support (urls field, BackendEndpointConfig)"
  - "Custom UnmarshalYAML for mixed string/object YAML entries"
  - "BuildPools() and GetPool() with fallback chain on Config"
affects: [01-02, 02-round-robin, 03-circuit-breaker, 04-health-probes, 10-advanced-selectors]

tech-stack:
  added: []
  patterns: ["atomic health state on BackendEndpoint", "Selector interface for backend selection strategy", "immutable Pool with copy-on-read"]

key-files:
  created:
    - pool/interface.go
    - pool/endpoint.go
    - pool/pool.go
    - pool/pool_test.go
    - relayer/config_test.go
  modified:
    - relayer/config.go

key-decisions:
  - "Pool.PoolName() instead of Pool.Name() to avoid collision risk"
  - "URL duplicate detection normalizes host+path with trailing slash trimmed"
  - "GetPool fallback chain: exact -> default_backend -> jsonrpc -> rest -> any"

patterns-established:
  - "Selector interface: Select(endpoints []*BackendEndpoint) int returns index or -1"
  - "BackendEndpoint atomic fields: healthy (Bool), consecutiveFailures (Int32), lastCheckUnixNano (Int64)"
  - "Pool is immutable after creation; All() returns copy of slice"

requirements-completed: [POOL-01, POOL-02]

duration: 7min
completed: 2026-03-13
---

# Phase 1 Plan 1: Pool Package and Multi-URL Config Summary

**Immutable backend pool with atomic health state, multi-URL config parsing with mixed string/object YAML support, and GetPool() fallback chain**

## Performance

- **Duration:** 7 min
- **Started:** 2026-03-13T02:23:05Z
- **Completed:** 2026-03-13T02:30:51Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- Created pool/ package with Selector interface, BackendEndpoint (atomic health), Pool (immutable with Next/All/Len/Healthy)
- Extended BackendConfig with urls field, BackendEndpointConfig with custom UnmarshalYAML handling mixed string/object entries
- Added validation for mutual exclusivity (url vs urls), duplicate URL/name detection, trailing slash normalization
- Implemented BuildPools() and GetPool() with full fallback chain (exact -> default_backend -> jsonrpc -> rest -> any)
- All 29 tests pass with -race flag, zero lint issues, full test suite green

## Task Commits

Each task was committed atomically:

1. **Task 1: Create pool package with Selector, BackendEndpoint, and Pool** - `6b0c8c5` (feat)
2. **Task 2: Extend BackendConfig with multi-URL support and pool building** - `a022ca5` (feat)

## Files Created/Modified
- `pool/interface.go` - Selector interface and FirstHealthySelector implementation
- `pool/endpoint.go` - BackendEndpoint with atomic health state fields
- `pool/pool.go` - Immutable Pool with Next/All/Len/Healthy/PoolName methods
- `pool/pool_test.go` - 11 tests including 100-goroutine concurrency test
- `relayer/config.go` - Extended BackendConfig, BackendEndpointConfig, UnmarshalYAML, BuildPools, GetPool
- `relayer/config_test.go` - 18 tests covering parsing, validation, pool building, fallback chain

## Decisions Made
- Used `PoolName()` instead of `Name()` for the pool identifier getter to avoid potential collision with embedded types
- URL duplicate detection normalizes by comparing `host + path` after trimming trailing slashes (catches `node:8545` vs `node:8545/`)
- GetPool fallback chain matches existing proxy.go behavior: exact match -> default_backend -> jsonrpc -> rest -> any available

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- pool/ package provides foundation for Phase 1 Plan 2 (caller migration in proxy.go, websocket.go, streaming.go)
- Selector interface ready for Phase 2 (RoundRobinSelector)
- BackendEndpoint health state ready for Phase 3 (circuit breaker) and Phase 4 (health probes)
- GetBackend()/GetBackendURL() kept temporarily for Plan 02 migration

---
*Phase: 01-backend-pool-foundation*
*Completed: 2026-03-13*
