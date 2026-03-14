---
phase: 05-fast-fail-and-resilience
plan: 01
subsystem: relayer
tags: [circuit-breaker, fast-fail, pool, prometheus, grpc, websocket]

# Dependency graph
requires:
  - phase: 03-circuit-breaker
    provides: Pool with endpoint health tracking, RecordResult, circuit breaker wiring
  - phase: 04-health-check-probes
    provides: HealthChecker with per-pool multi-endpoint probing
provides:
  - Pool.HasHealthy() zero-allocation health scan
  - Pool.NextExcluding(ep) for retry to different backend
  - Fast-fail pre-check across HTTP, WebSocket, gRPC transports
  - ha_relayer_fast_fails_total Prometheus counter
  - BackendConfig.MaxRetries field (0-3 range, validated)
  - sendServiceUnavailable 503 JSON helper
affects: [05-fast-fail-and-resilience, 06-retry-logic]

# Tech tracking
tech-stack:
  added: []
  patterns: [fast-fail pre-check before expensive operations, pool-level health gating]

key-files:
  created:
    - relayer/proxy_test.go
  modified:
    - pool/pool.go
    - pool/pool_test.go
    - relayer/config.go
    - relayer/metrics.go
    - relayer/proxy.go
    - relayer/websocket.go
    - relayer/relay_grpc_service.go

key-decisions:
  - "Fast-fail pre-check placed after metering but before validation/forwarding in HTTP path"
  - "WebSocket pre-check placed BEFORE Upgrade() to return HTTP 503 (not WS close frame)"
  - "gRPC returns codes.Unavailable (14) for fast-fail, not HTTP 503"
  - "fastFailsTotal metric is SEPARATE from relaysRejected per user decision"
  - "Debug-level logging for fast-fails to avoid log flood at 1000+ RPS during sustained outage"
  - "MaxRetries is a pointer field to distinguish not-set (default 1) from explicitly 0 (disabled)"

patterns-established:
  - "Fast-fail pattern: pool.HasHealthy() check before expensive operations"
  - "sendServiceUnavailable: minimal JSON response with service ID, no backend details"

requirements-completed: [RECV-01, RECV-02, POOL-03]

# Metrics
duration: 6min
completed: 2026-03-14
---

# Phase 05 Plan 01: Fast-Fail Pre-Check Summary

**Pool.HasHealthy() zero-allocation health gate across HTTP/WebSocket/gRPC with ha_relayer_fast_fails_total metric and MaxRetries config**

## Performance

- **Duration:** 6 min
- **Started:** 2026-03-14T00:00:43Z
- **Completed:** 2026-03-14T00:06:14Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Pool.HasHealthy() and NextExcluding() methods with full test coverage (8 test cases)
- Fast-fail pre-check wired into all 3 transports: HTTP (after metering, before validation), WebSocket (before Upgrade), gRPC (before forwarding)
- ha_relayer_fast_fails_total Prometheus counter with service_id label
- BackendConfig.MaxRetries pointer field with 0-3 validation (ready for Plan 02)
- sendServiceUnavailable helper returning minimal 503 JSON

## Task Commits

Each task was committed atomically (TDD: test then feat):

1. **Task 1: Pool HasHealthy/NextExcluding + config + metric** - `979a7a0` (test), `1c2a752` (feat)
2. **Task 2: Wire pre-check into all transports** - `c220606` (test), `347b8b0` (feat), `3d1ca4e` (fix: lint)

## Files Created/Modified
- `pool/pool.go` - Added HasHealthy() and NextExcluding() methods
- `pool/pool_test.go` - 8 new test cases for HasHealthy and NextExcluding
- `relayer/config.go` - MaxRetries field on BackendConfig with 0-3 validation
- `relayer/metrics.go` - fastFailsTotal Prometheus CounterVec
- `relayer/proxy.go` - sendServiceUnavailable helper, HTTP fast-fail pre-check
- `relayer/proxy_test.go` - Transport-level fast-fail tests (HTTP + WebSocket)
- `relayer/websocket.go` - WebSocket fast-fail before Upgrade(), removed redundant pool lookup
- `relayer/relay_grpc_service.go` - gRPC fast-fail with codes.Unavailable

## Decisions Made
- Fast-fail pre-check placed after metering but before validation/forwarding in HTTP path
- WebSocket pre-check placed BEFORE Upgrade() to return HTTP 503 (not WS close frame)
- gRPC returns codes.Unavailable (14), not HTTP 503
- fastFailsTotal is SEPARATE from relaysRejected per user decision
- Debug-level logging for fast-fails to avoid log flood at 1000+ RPS during sustained outage
- MaxRetries is a pointer field to distinguish not-set from explicitly 0

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed errcheck lint violation in sendServiceUnavailable**
- **Found during:** Task 2 (make lint)
- **Issue:** fmt.Fprintf return value not checked, errcheck flagged it
- **Fix:** Added `_, _ =` prefix to suppress (matches existing sendError pattern)
- **Files modified:** relayer/proxy.go
- **Verification:** make lint passes with 0 issues
- **Committed in:** 3d1ca4e

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Trivial lint fix, no scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- HasHealthy() and NextExcluding() are ready for Plan 02 (retry with backoff)
- MaxRetries config field is ready for Plan 02 consumption
- fastFailsTotal metric will provide visibility into fast-fail frequency

---
*Phase: 05-fast-fail-and-resilience*
*Completed: 2026-03-14*
