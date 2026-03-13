---
phase: 01-backend-pool-foundation
plan: 02
subsystem: relayer
tags: [pool, backend-resolution, proxy, websocket, tilt, multi-backend]

requires:
  - phase: 01-01
    provides: "pool/ package, BackendConfig with urls field, BuildPools(), GetPool()"
provides:
  - "Pool-based backend resolution in proxy.go and websocket.go"
  - "GetBackendConfig() with fallback chain for headers/auth access"
  - "Multi-backend example config and schema documentation"
  - "Tilt backend-2 pod for multi-backend testing"
affects: [02-round-robin, 03-circuit-breaker, 04-health-probes, 09-grpc-migration]

tech-stack:
  added: []
  patterns: ["GetPool().Next() for backend resolution", "GetBackendConfig() for shared pool-level config (headers, auth)"]

key-files:
  created: []
  modified:
    - relayer/proxy.go
    - relayer/websocket.go
    - relayer/config.go
    - config.relayer.example.yaml
    - config.relayer.schema.yaml
    - tilt/k8s/backend.Tiltfile
    - tilt/k8s/utils.Tiltfile

key-decisions:
  - "GetBackendConfig() mirrors GetPool() fallback chain for consistent headers/auth resolution"
  - "Fixed pre-existing URL mutation bug in fallback path by copying parsedBackendURL"
  - "backend-2 uses same Docker image as backend with separate k8s Service/Deployment"

patterns-established:
  - "GetPool().Next() for all backend endpoint resolution (except gRPC -- Phase 9)"
  - "GetBackendConfig() for pool-level shared config (headers, auth, health_check)"
  - "Pool summary logging at startup for operational visibility"

requirements-completed: [POOL-01, POOL-02]

duration: 6min
completed: 2026-03-13
---

# Phase 1 Plan 2: Caller Migration and Multi-Backend Config Summary

**Pool-based backend resolution in proxy.go and websocket.go, deprecated field cleanup, multi-backend example config with Tilt backend-2 deployment**

## Performance

- **Duration:** 6 min
- **Started:** 2026-03-13T02:32:58Z
- **Completed:** 2026-03-13T02:39:14Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Migrated proxy.go and websocket.go from direct backend URL access to GetPool().Next() pool-based resolution
- Removed deprecated backendURLs/parsedBackendURLs fields from ProxyServer and GetBackend()/GetBackendURL() from Config
- Added GetBackendConfig() with same fallback chain as GetPool() for headers/auth access
- Fixed pre-existing URL mutation bug in fallback path (was mutating cached URL)
- Added pool summary logging at startup (service, transport, backend count, strategy, endpoints)
- Updated example config to show multi-backend as primary format for develop-http
- Added urls, load_balancing, BackendEndpointConfig to config schema
- Deployed backend-2 pod in Tilt with separate k8s Service for multi-backend testing
- All tests pass with -race flag, zero lint issues, full test suite green

## Task Commits

Each task was committed atomically:

1. **Task 1: Migrate proxy.go and websocket.go to pool-based backend resolution** - `aae5b49` (feat)
2. **Task 2: Update configs and Tilt for multi-backend support** - `e59c573` (feat)

## Files Created/Modified
- `relayer/proxy.go` - Pool-based backend resolution, removed deprecated fields, pool summary logging
- `relayer/websocket.go` - Pool-based WebSocket backend resolution
- `relayer/config.go` - Replaced GetBackend()/GetBackendURL() with GetBackendConfig()
- `config.relayer.example.yaml` - Multi-backend urls array as primary format for develop-http
- `config.relayer.schema.yaml` - Added urls, load_balancing, BackendEndpointConfig with oneOf
- `tilt/k8s/backend.Tiltfile` - Added backend-2 Deployment and Service
- `tilt/k8s/utils.Tiltfile` - Updated relayer config overrides to use urls array for develop-http

## Decisions Made
- GetBackendConfig() implements the same fallback chain as GetPool() (exact -> default_backend -> jsonrpc -> rest -> any) to ensure consistent resolution for headers/auth
- Fixed URL mutation bug in fallback path (Rule 1 auto-fix): was mutating parsedBackendURL directly from the xsync cache; now copies before mutation
- backend-2 uses same Docker image (demo-backend) with separate k8s resources and different local port forwards (18545, 60051, 19095)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed URL mutation in fallback request path**
- **Found during:** Task 1 (proxy.go migration)
- **Issue:** The existing fallback path (non-POKT request) mutated `parsedBackendURL.Path` directly, which in the old code mutated the xsync cache entry and in the new code would mutate the shared pool endpoint URL
- **Fix:** Copy `parsedBackendURL` to a local `fallbackURL` variable before mutating Path
- **Files modified:** relayer/proxy.go
- **Verification:** Compilation succeeds, all tests pass
- **Committed in:** aae5b49 (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Bug fix was necessary for correctness with shared pool endpoints. No scope creep.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- All HTTP and WebSocket transport handlers now use pool-based backend resolution
- gRPC migration remains for Phase 9 (per locked decision)
- Pool infrastructure ready for Phase 2 (RoundRobinSelector) and Phase 3 (circuit breaker)
- Tilt environment supports multi-backend testing with backend and backend-2 pods
- Phase 1 complete -- all POOL-01 and POOL-02 requirements satisfied

---
*Phase: 01-backend-pool-foundation*
*Completed: 2026-03-13*
