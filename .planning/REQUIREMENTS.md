# Requirements: Multi-Backend Load Balancing & Health Checks

**Defined:** 2026-03-12
**Core Value:** When a backend node goes down, relays continue flowing to healthy backends — and when all are down, the relayer fast-fails so the gateway can route elsewhere.

## v1 Requirements

### Backend Pool & Config

- [x] **POOL-01**: Operator can configure multiple backend URLs per service per transport
- [x] **POOL-02**: Existing single-URL configs continue to work without changes (backward compatible)
- [x] **POOL-03**: When a backend is marked unhealthy, new requests are not routed to it (graceful removal)
- [ ] **POOL-04**: Health status API endpoint exposes backend pool state (health, failure counts, last check time)

### Load Balancing

- [x] **LB-01**: Round-robin load balancing across healthy backends (default strategy)
- [ ] **LB-02**: Least-connections load balancing (track active connections, select lowest)
- [ ] **LB-03**: IP/session-hash routing for sticky backend selection (consistent hashing)
- [ ] **LB-04**: Per-service load balancing strategy selection via config

### Failure Detection

- [x] **FAIL-01**: Circuit breaker marks backend unhealthy after configurable consecutive 5xx failures
- [x] **FAIL-02**: Passive failure detection from real relay traffic (5xx, timeouts, connection refused)
- [x] **FAIL-03**: Periodic health check probes of unhealthy backends for recovery detection
- [x] **FAIL-04**: Operator-defined custom health check probes (request body, expected status, response matching)
- [x] **FAIL-05**: Configurable unhealthy threshold (failures before marking unhealthy) and healthy threshold (successes before marking healthy)

### Recovery & Resilience

- [x] **RECV-01**: Fast-fail with dedicated 503 "service temporarily unavailable" error when all backends unhealthy
- [x] **RECV-02**: Health-aware pre-check before relay processing begins (no wasted compute)
- [x] **RECV-03**: On backend failure, retry on a different healthy backend before returning error (max 1-2 retries, respect timeout budget)

### Observability

- [ ] **OBS-01**: Per-backend Prometheus metrics (health status, failure count, request count, latency)
- [ ] **OBS-02**: Backend failover events logged with structured context (which backend failed, which selected instead)

### Transport Support

- [x] **XPORT-01**: Multi-backend support for JSON-RPC (HTTP) transport (per-request selection)
- [ ] **XPORT-02**: Multi-backend support for REST/Streaming (SSE) transport (per-connection selection)
- [ ] **XPORT-03**: Multi-backend support for WebSocket transport (per-connection sticky routing)
- [ ] **XPORT-04**: Multi-backend support for gRPC transport (per-call selection, integrate with gRPC connection lifecycle)

## v2 Requirements

### Advanced Recovery

- **RECV-10**: Backend warmup / slow start after recovery (gradually increase traffic)
- **RECV-11**: Connection draining on unhealthy transition (allow existing WS/SSE connections to finish)

### Advanced Observability

- **OBS-10**: Per-backend latency tracking with outlier detection (auto-eject slow backends)
- **OBS-11**: Health check history/audit log for debugging

### Advanced Health Checks

- **FAIL-10**: Transport-specific health probes (gRPC health protocol, WebSocket ping/pong, distinct from HTTP probe)

## Out of Scope

| Feature | Reason |
|---------|--------|
| Weighted load balancing | Operators rarely know right weights; least-connections adapts automatically |
| Dynamic backend discovery (DNS SRV, Consul, etcd) | Massive scope expansion; static config with hot-reload sufficient |
| Request mirroring / traffic shadowing | Relay signing makes duplicates create false claims |
| Adaptive/ML-based load balancing | Over-engineering for small pools (2-5 backends typical) |
| Cross-service backend sharing | Different services have different requirements; isolation is simpler |
| Per-backend connection limits | Already handled by HTTPTransportConfig.MaxConnsPerHost |
| Per-backend rate limiting | Relay meter already handles rate limiting |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| POOL-01 | Phase 1 | Complete |
| POOL-02 | Phase 1 | Complete |
| POOL-03 | Phase 5 | Complete |
| POOL-04 | Phase 6 | Pending |
| LB-01 | Phase 2 | Complete |
| LB-02 | Phase 10 | Pending |
| LB-03 | Phase 10 | Pending |
| LB-04 | Phase 10 | Pending |
| FAIL-01 | Phase 3 | Complete |
| FAIL-02 | Phase 3 | Complete |
| FAIL-03 | Phase 4 | Complete |
| FAIL-04 | Phase 4 | Complete |
| FAIL-05 | Phase 4 | Complete |
| RECV-01 | Phase 5 | Complete |
| RECV-02 | Phase 5 | Complete |
| RECV-03 | Phase 5 | Complete |
| OBS-01 | Phase 6 | Pending |
| OBS-02 | Phase 6 | Pending |
| XPORT-01 | Phase 2 | Complete |
| XPORT-02 | Phase 8 | Pending |
| XPORT-03 | Phase 7 | Pending |
| XPORT-04 | Phase 9 | Pending |

**Coverage:**
- v1 requirements: 22 total
- Mapped to phases: 22
- Unmapped: 0

---
*Requirements defined: 2026-03-12*
*Last updated: 2026-03-12 after roadmap creation*
