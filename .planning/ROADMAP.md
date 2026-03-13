# Roadmap: Multi-Backend Load Balancing & Health Checks

## Overview

This roadmap delivers native multi-backend load balancing and health-check failover to the Pocket RelayMiner. The work progresses from foundational data structures through HTTP round-robin, failure detection, recovery, observability, then transport-by-transport multi-backend support (WebSocket, SSE, gRPC), and finally advanced load balancing strategies. Each phase produces a testable, verifiable increment. The structure follows the dependency chain identified in research: pool foundation -> selection -> failure detection -> recovery -> observability -> stateful transports -> advanced strategies.

## Phases

**Phase Numbering:**
- Integer phases (1, 2, 3): Planned milestone work
- Decimal phases (2.1, 2.2): Urgent insertions (marked with INSERTED)

Decimal phases appear between their surrounding integers in numeric order.

- [ ] **Phase 1: Backend Pool Foundation** - Data structures, config parsing, backward-compatible multi-URL support
- [ ] **Phase 2: Round-Robin HTTP Selection** - First working load balancer for JSON-RPC transport
- [ ] **Phase 3: Circuit Breaker** - Passive failure detection from live relay traffic
- [ ] **Phase 4: Health Check Probes** - Active probing of unhealthy backends for recovery detection
- [ ] **Phase 5: Fast-Fail and Resilience** - Graceful degradation when backends are down
- [ ] **Phase 6: Observability** - Per-backend metrics, failover logging, health status API
- [ ] **Phase 7: WebSocket Multi-Backend** - Connection-level sticky routing with close-frame failover
- [ ] **Phase 8: REST/Streaming Multi-Backend** - Per-connection SSE backend selection
- [ ] **Phase 9: gRPC Multi-Backend** - Per-call selection with gRPC connection lifecycle management
- [ ] **Phase 10: Advanced Strategies** - Least-connections, app-hash routing, per-service strategy config

## Phase Details

### Phase 1: Backend Pool Foundation
**Goal**: Operators can configure multiple backend URLs per service and the relayer parses them correctly alongside existing single-URL configs
**Depends on**: Nothing (first phase)
**Requirements**: POOL-01, POOL-02
**Success Criteria** (what must be TRUE):
  1. Operator can specify multiple backend URLs for a service in the YAML config and the relayer starts without errors
  2. Existing single-URL configs continue to work without any changes (backward compatible)
  3. Backend pool data structures are initialized at startup with all configured URLs in healthy state
  4. Unit tests pass with race detector for concurrent pool access (backend_pool_test.go exists)
**Plans**: 2 plans

Plans:
- [ ] 01-01-PLAN.md — Pool package (Selector, BackendEndpoint, Pool) + config extension (urls field, UnmarshalYAML, validation, BuildPools)
- [ ] 01-02-PLAN.md — Caller migration (proxy.go, websocket.go to GetPool), deprecated field cleanup, config/Tilt updates

### Phase 2: Round-Robin HTTP Selection
**Goal**: JSON-RPC relay requests are distributed across healthy backends using round-robin
**Depends on**: Phase 1
**Requirements**: LB-01, XPORT-01
**Success Criteria** (what must be TRUE):
  1. JSON-RPC relays are distributed across configured backends (observable via relay logs showing different backend URLs)
  2. Round-robin selection adds less than 0.1ms to relay latency (verified by benchmark)
  3. A load test against a multi-backend HTTP service shows requests spread across all backends
**Plans**: 2 plans

Plans:
- [ ] 02-01-PLAN.md — RoundRobinSelector implementation, auto-detect strategy wiring in BuildPools, unit tests + benchmark
- [ ] 02-02-PLAN.md — Backend BACKEND_ID in responses, Tilt env vars, test-round-robin.sh distribution script

### Phase 3: Circuit Breaker
**Goal**: Backends that return consecutive errors are automatically removed from rotation without operator intervention
**Depends on**: Phase 2
**Requirements**: FAIL-01, FAIL-02
**Success Criteria** (what must be TRUE):
  1. A backend returning consecutive 5xx responses is marked unhealthy after the configured threshold (default 5)
  2. Unhealthy backends receive zero new relay requests until recovery
  3. Passive failure detection works inline with real relay traffic (no separate polling needed for detection)
  4. Circuit breaker state transitions are visible in logs with structured context (backend URL, failure count, status code)
**Plans**: TBD

Plans:
- [ ] 03-01: TBD

### Phase 4: Health Check Probes
**Goal**: Unhealthy backends are automatically restored to rotation when operator-defined health probes succeed
**Depends on**: Phase 3
**Requirements**: FAIL-03, FAIL-04, FAIL-05
**Success Criteria** (what must be TRUE):
  1. Unhealthy backends are periodically probed using operator-defined health check configuration
  2. A backend that was circuit-broken returns to healthy status after consecutive successful probes (configurable healthy threshold)
  3. Operators can define custom probe request body, expected HTTP status, and response body matching per backend
  4. Health check probe intervals and thresholds are configurable per service
**Plans**: TBD

Plans:
- [ ] 04-01: TBD

### Phase 5: Fast-Fail and Resilience
**Goal**: When all backends are unhealthy the relayer fast-fails immediately, and when some are healthy it retries failed requests on alternates
**Depends on**: Phase 3
**Requirements**: RECV-01, RECV-02, RECV-03, POOL-03
**Success Criteria** (what must be TRUE):
  1. When all backends for a service are unhealthy, the relayer returns a 503 "service temporarily unavailable" immediately (no timeout waiting)
  2. Relay validation and signing are skipped when no healthy backend is available (no wasted compute)
  3. On a single backend failure, the relay is retried on a different healthy backend before returning an error (max 1-2 retries within timeout budget)
  4. Unhealthy backends are gracefully excluded from selection without affecting healthy backend routing
**Plans**: TBD

Plans:
- [ ] 05-01: TBD

### Phase 6: Observability
**Goal**: Operators have full visibility into backend pool health, failover events, and per-backend performance
**Depends on**: Phase 3
**Requirements**: OBS-01, OBS-02, POOL-04
**Success Criteria** (what must be TRUE):
  1. Prometheus metrics expose per-backend health status, failure count, request count, and latency
  2. Backend failover events are logged with structured context (failed backend, selected replacement, failure reason)
  3. A health status API endpoint returns current backend pool state (health, failure counts, last check time) as JSON
  4. Grafana dashboards or PromQL queries can distinguish individual backend health within a service
**Plans**: TBD

Plans:
- [ ] 06-01: TBD

### Phase 7: WebSocket Multi-Backend
**Goal**: WebSocket connections are routed to healthy backends at connection time with proper failover signaling
**Depends on**: Phase 5
**Requirements**: XPORT-03
**Success Criteria** (what must be TRUE):
  1. New WebSocket connections are routed to a healthy backend selected via the configured strategy
  2. If a WebSocket backend becomes unhealthy mid-session, the connection is closed with RFC 6455 code 1012/1013 so the client reconnects to a healthy backend
  3. WebSocket connections are pinned to their selected backend for the session duration (sticky routing)
**Plans**: TBD

Plans:
- [ ] 07-01: TBD

### Phase 8: REST/Streaming Multi-Backend
**Goal**: SSE and REST streaming connections are routed to healthy backends at connection establishment
**Depends on**: Phase 5
**Requirements**: XPORT-02
**Success Criteria** (what must be TRUE):
  1. New SSE/streaming connections are routed to a healthy backend at connection time
  2. Long-lived streaming connections are pinned to their selected backend for the connection duration
  3. If the selected backend becomes unhealthy, the streaming connection is terminated cleanly so the client can reconnect
**Plans**: TBD

Plans:
- [ ] 08-01: TBD

### Phase 9: gRPC Multi-Backend
**Goal**: gRPC relay calls are distributed across healthy backends with proper connection lifecycle management
**Depends on**: Phase 5
**Requirements**: XPORT-04
**Success Criteria** (what must be TRUE):
  1. gRPC relay calls are routed to healthy backends on a per-call basis
  2. Each backend URL has its own gRPC ClientConn that is reused across calls (not created per-call)
  3. Unhealthy gRPC backends are excluded from selection without closing their ClientConn (gRPC handles reconnection internally)
  4. The existing grpcBackends sync.Map is migrated to xsync.MapOf as part of multi-backend integration
**Plans**: TBD

Plans:
- [ ] 09-01: TBD

### Phase 10: Advanced Strategies
**Goal**: Operators can choose least-connections or app-hash load balancing strategies per service
**Depends on**: Phase 6, Phase 7, Phase 8, Phase 9
**Requirements**: LB-02, LB-03, LB-04
**Success Criteria** (what must be TRUE):
  1. Operator can set load balancing strategy per service via config (round_robin, least_conn, app_hash)
  2. Least-connections strategy routes to the backend with fewest active connections (using power-of-two-choices)
  3. App-hash strategy routes relays from the same application address to the same backend (consistent hashing on ApplicationAddress, not source IP)
  4. Strategy selection is validated at config parse time with clear error for invalid values
**Plans**: TBD

Plans:
- [ ] 10-01: TBD

## Progress

**Execution Order:**
Phases execute in numeric order: 1 -> 2 -> 3 -> 4/5/6 (5 and 6 can parallel after 3) -> 7/8/9 (can parallel after 5) -> 10

| Phase | Plans Complete | Status | Completed |
|-------|----------------|--------|-----------|
| 1. Backend Pool Foundation | 0/2 | Not started | - |
| 2. Round-Robin HTTP Selection | 0/2 | Not started | - |
| 3. Circuit Breaker | 0/1 | Not started | - |
| 4. Health Check Probes | 0/1 | Not started | - |
| 5. Fast-Fail and Resilience | 0/1 | Not started | - |
| 6. Observability | 0/1 | Not started | - |
| 7. WebSocket Multi-Backend | 0/1 | Not started | - |
| 8. REST/Streaming Multi-Backend | 0/1 | Not started | - |
| 9. gRPC Multi-Backend | 0/1 | Not started | - |
| 10. Advanced Strategies | 0/1 | Not started | - |
