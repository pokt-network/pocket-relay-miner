---
phase: 05-fast-fail-and-resilience
plan: 02
subsystem: relayer
tags: [retry, circuit-breaker, http, resilience, pool]

# Dependency graph
requires:
  - phase: 05-fast-fail-and-resilience plan 01
    provides: HasHealthy, NextExcluding, RecordResult, MaxRetries config, fast-fail pre-check
provides:
  - IsRetryable exported function in pool package
  - HTTP retry loop with alternate backend selection via NextExcluding
  - Backend-Retries response header on successful retries
  - getMaxRetries helper method on ProxyServer
affects: [phase-06, phase-10]

# Tech tracking
tech-stack:
  added: []
  patterns: [retry-on-alternate-backend, pre-selected-endpoint-forwarding]

key-files:
  created: []
  modified:
    - pool/circuit_breaker.go
    - pool/pool_test.go
    - relayer/proxy.go

key-decisions:
  - "IsRetryable wraps isFailure directly -- same classification for retry and circuit breaker"
  - "forwardToBackendWithStreaming accepts optional pre-selected endpoint+pool for retry path"
  - "Retry loop uses outer err variable to avoid redeclaration in handleRelay scope"

patterns-established:
  - "Pre-selected endpoint pattern: pass endpoint+pool to forwardToBackendWithStreaming to control backend selection externally"
  - "Backend-Retries header: integer value set only on successful retry (attempt > 0)"

requirements-completed: [RECV-03]

# Metrics
duration: 4min
completed: 2026-03-14
---

# Phase 05 Plan 02: HTTP Retry-on-Alternate Backend Summary

**IsRetryable function + retry loop using NextExcluding for transparent single-backend failure recovery with Backend-Retries header**

## Performance

- **Duration:** 4 min
- **Started:** 2026-03-14T00:09:14Z
- **Completed:** 2026-03-14T00:13:30Z
- **Tasks:** 1 (TDD: RED + GREEN)
- **Files modified:** 3

## Accomplishments
- Added exported `IsRetryable(statusCode, err) bool` to pool package wrapping `isFailure`
- Implemented retry loop in HTTP relay handler: on retryable failure, selects alternate backend via `NextExcluding`, shares original context timeout, feeds both attempts to circuit breaker
- Added `Backend-Retries` response header (integer) on successful retry responses
- Added `getMaxRetries` helper using `BackendConfig.MaxRetries` pointer (default 1, 0 disables, max 3)
- Refactored `forwardToBackendWithStreaming` to accept optional pre-selected endpoint+pool parameters

## Task Commits

Each task was committed atomically:

1. **Task 1 RED: IsRetryable tests** - `66c879b` (test)
2. **Task 1 GREEN: IsRetryable + retry loop + Backend-Retries** - `0f0f9b3` (feat)

## Files Created/Modified
- `pool/circuit_breaker.go` - Added exported IsRetryable wrapping isFailure
- `pool/pool_test.go` - Added 6 IsRetryable test cases (connection refused, 5xx, timeout, 4xx, success, DNS error)
- `relayer/proxy.go` - Retry loop in HTTP handler, getMaxRetries helper, forwardToBackendWithStreaming accepts pre-selected endpoint

## Decisions Made
- IsRetryable wraps isFailure directly: same classification for retry eligibility and circuit breaker counting (connection errors + 5xx = retryable, timeouts + 4xx = not retryable)
- forwardToBackendWithStreaming signature extended with preSelectedEndpoint + preSelectedPool (nil = normal internal selection, non-nil = use provided)
- Retry loop breaks on streaming responses (cannot retry after partial response sent to client)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- `err` variable redeclaration conflict in handleRelay scope: resolved by reusing the outer `err` from line 558 instead of declaring a new one in the retry var block.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Phase 05 complete: fast-fail pre-check + retry-on-alternate backend provide full resilience foundation
- Ready for Phase 06 (health check recovery) and Phase 10 (advanced load balancing)

---
*Phase: 05-fast-fail-and-resilience*
*Completed: 2026-03-14*
