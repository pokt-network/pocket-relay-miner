# Fix: Leader Election OOM Resilience

## Problem

When a miner instance crashes from heavy traffic causing Redis OOM, the standby miner fails to take over leadership. This results in **no claims/proofs being submitted** and **lost rewards** for the entire duration of the OOM condition (potentially 2+ hours until TTL-bearing keys expire).

### Error Example

```
OOM command not allowed when used memory > 'maxmemory'
```

### Failure Chain

1. Heavy relay traffic fills Redis memory beyond `maxmemory` limit
2. Active leader crashes or loses its lock renewal (write ops fail during OOM)
3. Leader key expires (TTL 30s) — no leader exists
4. Standby miner attempts to acquire leadership via Lua script
5. Lua script calls `SET` (a write op) which Redis rejects during OOM
6. Acquisition error is logged at `DEBUG` level — **invisible in production** (`INFO` or higher)
7. No metric is emitted — **no alerting possible**
8. Standby retries every 10s, always failing silently
9. No leader is elected until OOM naturally clears (keys expire after TTL)
10. **Result**: 2+ hours of zero claim/proof submissions = lost rewards

### Root Cause

The asymmetry between renewal failure handling and acquisition failure handling:

| Scenario | Log Level | Metric | Impact |
|----------|-----------|--------|--------|
| Renewal failure (leader) | `WARN` | `leader_losses_total` | Operators can detect and alert |
| Acquisition failure (standby) | `DEBUG` | None | **Silent failure — no visibility** |

## Fix Summary

### 1. OOM Error Detection (`transport/redis/errors.go`)

New `IsOOMError(err) bool` helper that detects Redis OOM errors by checking for the `"OOM"` substring. Placed in `transport/redis/` to avoid circular dependencies (both `leader/` and `miner/` import it).

OOM is classified as a **retryable error** in `miner/errors.go` because it's transient — it clears when TTL-bearing keys expire or memory is freed.

**Files**:
- `transport/redis/errors.go` — `IsOOMError()` function
- `transport/redis/errors_test.go` — Unit tests
- `miner/errors.go` — Added OOM check to `IsRetryableError()`

### 2. Leader Election Error Visibility (`leader/global_leader.go`)

The primary bug fix. Leadership acquisition failures are now visible and measurable:

| Condition | Log Level | Metric Label |
|-----------|-----------|--------------|
| OOM error | `ERROR` | `redis_oom` |
| Other Redis error | `WARN` | `redis_error` |
| Another instance holds lock | `DEBUG` (normal) | No metric (not a failure) |

Added `consecutiveAcquireFailures` counter:
- Incremented on every acquisition error
- Reset on successful acquisition or when another instance holds the lock (Redis is healthy)
- Logged as a structured field for log-based alerting

**Files**:
- `leader/metrics.go` — Added `ha_miner_leader_acquisition_failures_total` counter with `[instance, reason]` labels
- `leader/global_leader.go` — Updated `attemptLeadership()` error handling

### 3. Redis Memory Health Monitoring (`leader/redis_health.go`)

New `RedisHealthMonitor` that periodically polls `INFO MEMORY` and exposes memory usage metrics. Runs on **ALL replicas** (not just leader) because standbys need visibility into Redis memory state during OOM.

- **Interval**: 30 seconds
- **Warning**: Logs `WARN` when memory usage exceeds 90% of `maxmemory`

**Files**:
- `leader/redis_health.go` — Monitor implementation
- `leader/redis_health_metrics.go` — Three gauges: `redis_used_memory_bytes`, `redis_max_memory_bytes`, `redis_memory_usage_ratio`
- `leader/redis_health_test.go` — Unit tests for INFO MEMORY parsing and lifecycle
- `cmd/cmd_miner.go` — Wiring (started after Redis client creation)

### 4. Enhanced `/ready` Endpoint (`observability/server.go`)

The `/ready` endpoint now supports a readiness check function. For the miner, this is a Redis `PING` — if Redis is unreachable or in OOM, `/ready` returns HTTP 503.

This allows Kubernetes liveness/readiness probes to detect Redis connectivity issues and restart pods if needed.

**Files**:
- `observability/server.go` — Added `SetReadinessCheck()` method and updated `/ready` handler
- `cmd/cmd_miner.go` — Set Redis PING as readiness check after client creation

## Prometheus Alerting

### Leadership Acquisition Failures (primary alert)

```promql
# Alert when standby cannot acquire leadership due to Redis errors
increase(ha_miner_leader_acquisition_failures_total{reason="redis_oom"}[5m]) > 0
```

### Redis Memory Usage (early warning)

```promql
# Alert before OOM occurs (threshold: 90%)
ha_miner_redis_memory_usage_ratio > 0.9
```

### No Leader Elected (consequence alert)

```promql
# Alert when no instance is leader for 2+ minutes
# If max across all instances is 0, no one is leader
max(ha_miner_leader_status) == 0
```

### Combined Dashboard Query

```promql
# Redis memory vs leadership status — correlate OOM with leadership gaps
ha_miner_redis_memory_usage_ratio
ha_miner_leader_status
rate(ha_miner_leader_acquisition_failures_total[5m])
```

## Verification

```bash
# Run all tests
make test

# Run leader package tests specifically
go test -race ./leader/...

# Run transport/redis tests
go test -race ./transport/redis/...

# After deployment, verify metrics exist:
# curl localhost:9090/metrics | grep ha_miner_leader_acquisition_failures
# curl localhost:9090/metrics | grep ha_miner_redis_memory
# curl localhost:9090/ready  # Should return 200 OK or 503 if Redis is down
```
