---
phase: 03-circuit-breaker
plan: 02
subsystem: relayer
tags: [circuit-breaker, transport-wiring, healthcheck-refactor, pool-integration]

# Dependency graph
requires:
  - phase: 03-circuit-breaker plan 01
    provides: Pool.RecordResult(), TransitionEvent, isFailure/isTimeoutError, DefaultUnhealthyThreshold
  - phase: 01-backend-pool
    provides: Pool, BackendEndpoint with atomic health primitives, GetPool, GetBackendConfig
provides:
  - RecordResult wired in HTTP/streaming transport (proxy.go)
  - RecordResult wired in WebSocket transport (websocket.go)
  - RecordResult wired in gRPC transport (relay_grpc_service.go)
  - healthcheck.go delegates health state to pool BackendEndpoint
  - getCircuitBreakerThreshold helper on ProxyServer and RelayGRPCService
  - logCircuitBreakerTransition helper on ProxyServer and RelayGRPCService
  - RegisterBackendWithEndpoint for pool-aware health check registration
affects: [04-active-probes, 05-fast-fail, 06-metrics]

# Tech tracking
tech-stack:
  added: []
  patterns: [transport-level RecordResult wiring after backend response, healthcheck delegation to pool endpoint]

key-files:
  created: []
  modified: [relayer/proxy.go, relayer/websocket.go, relayer/relay_grpc_service.go, relayer/healthcheck.go]

key-decisions:
  - "forwardToBackendWithStreaming returns endpoint and pool for caller-side RecordResult (avoids deep refactoring)"
  - "WebSocket records on connection success/failure (not per-message, since WS is long-lived)"
  - "gRPC uses pool-based selection via function refs (GetPool/GetBackendConfig) on RelayGRPCServiceConfig"
  - "healthcheck.go preserves legacy path when no pool endpoint linked (backwards compatible)"
  - "RecordResult called before error handling so both success and failure paths are covered"

patterns-established:
  - "Transport wiring: RecordResult called at boundary after backend response, before error handling logic"
  - "Health delegation: BackendHealth delegates IsHealthy/SetStatus/recordFailure to pool.BackendEndpoint when linked"
  - "Circuit breaker threshold resolution: GetBackendConfig -> HealthCheck.UnhealthyThreshold -> DefaultUnhealthyThreshold(5)"

requirements-completed: [FAIL-01, FAIL-02]

# Metrics
duration: 9min
completed: 2026-03-13
---

# Phase 03 Plan 02: Transport Wiring Summary

**RecordResult wired into all four transports (HTTP, streaming, WebSocket, gRPC) with healthcheck.go refactored to delegate health state to pool BackendEndpoint**

## Performance

- **Duration:** 9 min
- **Started:** 2026-03-13T21:57:34Z
- **Completed:** 2026-03-13T22:06:44Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- All four transports (HTTP, streaming, WebSocket, gRPC) call RecordResult after backend interactions
- State transition logging at correct levels (Error for circuit-break, Info for recovery, silent between)
- healthcheck.go refactored to use pool BackendEndpoint as single source of truth for health state
- gRPC transport migrated to pool-based selection (prerequisite for circuit breaker)

## Task Commits

Each task was committed atomically:

1. **Task 1: Wire RecordResult into HTTP/streaming and WebSocket transports** - `186c83b` (feat)
2. **Task 2: Wire gRPC transport and refactor healthcheck.go** - `1410799` (feat)

## Files Created/Modified
- `relayer/proxy.go` - Added pool import, endpoint/pool return from forwardToBackendWithStreaming, RecordResult wiring, getCircuitBreakerThreshold and logCircuitBreakerTransition helpers, GetPool/GetBackendConfig passed to gRPC service
- `relayer/websocket.go` - RecordResult on WebSocket backend connection success and failure
- `relayer/relay_grpc_service.go` - Pool-based selection, RecordResult wiring, GetPool/GetBackendConfig function refs, circuit breaker helpers
- `relayer/healthcheck.go` - BackendHealth delegates to pool.BackendEndpoint, RegisterBackendWithEndpoint, recordFailure/recordSuccess use endpoint atomics

## Decisions Made
- forwardToBackendWithStreaming returns (*pool.BackendEndpoint, *pool.Pool) as additional return values rather than restructuring to select endpoint outside (less refactoring, endpoint naturally scoped inside the function)
- WebSocket records at connection establishment level (success on connect, failure on connect error) since WS is long-lived and individual messages are not backend HTTP requests
- gRPC service receives GetPool/GetBackendConfig as function refs (same pattern as GetHTTPClient/GetServiceTimeout) to avoid direct config dependency
- healthcheck.go keeps legacy path (no endpoint) for backwards compatibility; new RegisterBackendWithEndpoint enables pool-linked health tracking
- RecordResult positioned before error handling conditionals so it captures both success and failure results

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- Lint caught a staticcheck suggestion to use tagged switch instead of if-else chain in healthcheck.go SetStatus -- fixed inline.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All transports wire into circuit breaker -- passive failure detection is fully active
- Phase 4 (active probes) can use the recovery path in RecordResult and the preserved health check loop
- Phase 5 (fast-fail) can use pool endpoint health state for all-backends-down detection
- Phase 6 (metrics) can add Prometheus counters at the transition logging call sites

---
*Phase: 03-circuit-breaker*
*Completed: 2026-03-13*
