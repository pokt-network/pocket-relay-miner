---
phase: 04-health-check-probes
plan: 01
subsystem: relayer
tags: [health-check, http-probe, circuit-breaker, pool, metrics]

# Dependency graph
requires:
  - phase: 03-circuit-breaker
    provides: "BackendEndpoint atomics, RecordResult, pool-based failure tracking"
provides:
  - "Per-pool multi-endpoint health probing with custom HTTP method/body/response validation"
  - "RegisterPool API for pool-aware health check registration"
  - "buildProbeRequest/validateResponse helpers for custom probe configuration"
  - "Per-endpoint Prometheus metrics (service_id + endpoint labels)"
affects: [05-fast-fail, 06-prometheus-metrics]

# Tech tracking
tech-stack:
  added: []
  patterns: ["per-pool multi-endpoint probing", "custom HTTP probe with body/status/auth", "full reset on recovery transition"]

key-files:
  created: []
  modified:
    - "relayer/healthcheck.go"
    - "relayer/config.go"
    - "relayer/metrics.go"
    - "cmd/cmd_relayer.go"
    - "relayer/service.go"

key-decisions:
  - "Merged Task 1 and Task 2 into single commit since callers must update for compilation"
  - "Removed RegisterBackend/RegisterBackendWithEndpoint entirely (no backward compat needed)"
  - "GetAllHealth uses poolKey#N format for multi-endpoint pools to keep flat map structure"
  - "IsHealthy returns true if ANY endpoint in pool is healthy (pool-level health)"

patterns-established:
  - "RegisterPool pattern: callers use config.GetPool() + pool.All() to get endpoints for registration"
  - "buildProbeRequest: auto-detect JSON content-type from request body prefix"
  - "validateResponse: custom status code list or default 2xx range, optional body substring match"

requirements-completed: [FAIL-03, FAIL-04, FAIL-05]

# Metrics
duration: 4min
completed: 2026-03-13
---

# Phase 04 Plan 01: Health Check Probes Summary

**Per-pool multi-endpoint health probing with custom HTTP method/body/response validation and pool-level auth inheritance**

## Performance

- **Duration:** 4 min
- **Started:** 2026-03-13T23:04:08Z
- **Completed:** 2026-03-13T23:08:34Z
- **Tasks:** 2 (merged into 1 commit)
- **Files modified:** 5

## Accomplishments
- Refactored HealthChecker from per-service single-backend to per-pool multi-endpoint probing
- Added 5 new config fields (Method, RequestBody, ContentType, ExpectedBody, ExpectedStatus) for operator-customizable probes
- Added pool-level auth/header inheritance for probe requests (bearer token, basic auth, custom headers)
- Added per-endpoint Prometheus metrics labels for granular observability
- Full reset (consecutiveFailures=0, consecutiveSuccesses=0) on recovery transition

## Task Commits

Each task was committed atomically:

1. **Task 1+2: Extend config, refactor HealthChecker, update registration paths** - `d3e32c4` (feat)

## Files Created/Modified
- `relayer/config.go` - Added Method, RequestBody, ContentType, ExpectedBody, ExpectedStatus to BackendHealthCheckConfig
- `relayer/healthcheck.go` - Refactored to per-pool multi-endpoint probing with RegisterPool, checkPool, checkEndpoint, buildProbeRequest, validateResponse
- `relayer/metrics.go` - Added "endpoint" label to healthCheckSuccesses, healthCheckFailures, backendHealthStatus
- `cmd/cmd_relayer.go` - Updated to use RegisterPool with config.GetPool + pool.All()
- `relayer/service.go` - Updated to use RegisterPool with config.GetPool + pool.All(), added health_check enabled guard

## Decisions Made
- Merged Task 1 and Task 2 into a single commit because removing old RegisterBackend methods required updating callers simultaneously for compilation
- Removed old RegisterBackend/RegisterBackendWithEndpoint entirely (no other callers exist)
- GetAllHealth uses "poolKey#N" key format for multi-endpoint pools to maintain a flat map interface
- IsHealthy at pool level returns true if ANY endpoint is healthy (consistent with pool selection behavior)

## Deviations from Plan

None - plan executed exactly as written. Tasks 1 and 2 were merged into a single commit due to compilation dependency (cannot remove old methods without updating callers).

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Health probing infrastructure complete for all pool endpoints
- Ready for Phase 05 (fast-fail when all backends down) and Phase 06 (Prometheus per-backend metrics)
- Circuit breaker (Phase 3) and health probes share the same BackendEndpoint atomics for unified failure tracking

---
*Phase: 04-health-check-probes*
*Completed: 2026-03-13*
