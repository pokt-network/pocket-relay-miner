# SMST HA Bug Fix - Verification Plan

## Critical Changes to Verify

1. **SMST persists to Redis** (Commit() after UpdateTree)
2. **SMST warmup from Redis** on startup/failover
3. **Late relays dropped gracefully** with metrics
4. **Zero relay claims skipped** with metrics
5. **Unprofitable claims skipped** with economic validation
6. **SMST cleanup** after settlement

---

## Verification Checklist

### 1. ✅ SMST Writes to Redis (Most Critical)

**What**: Verify `Commit()` is writing SMST nodes to Redis after each relay.

**How**:
```bash
# After Tilt restart, wait for relays to flow, then check Redis
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli KEYS "ha:smst:*" | wc -l

# Expected: > 0 (should see SMST keys for active sessions)
```

**Check node count in a specific session**:
```bash
# Pick a session ID from active sessions
SESSION_ID=$(kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli KEYS "ha:smst:*:nodes" | head -1 | sed 's/ha:smst://;s/:nodes//')

# Check how many nodes in the tree
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli HLEN "ha:smst:${SESSION_ID}:nodes"

# Expected: > 0 (number of SMST nodes)
```

**Check SMST metrics**:
```bash
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | grep smst_redis_operations

# Expected output (counters should be > 0):
# smst_redis_operations_total{operation="set",result="success"} 1234
# smst_redis_operations_total{operation="get",result="success"} 567
```

**Success Criteria**:
- ✅ At least 1 `ha:smst:*:nodes` key exists in Redis
- ✅ HLEN shows > 0 nodes in active session trees
- ✅ `smst_redis_operations_total{operation="set"}` counter is incrementing

---

### 2. ✅ SMST Warmup on Startup

**What**: Verify miners load existing SMST trees from Redis on startup.

**How**:
```bash
# Check startup logs for warmup messages
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --tail=500 | \
  grep -i "warming up SMST\|SMST warmup complete"

# Expected output:
# {"level":"info","message":"warming up SMST trees from Redis"}
# {"level":"info","loaded_trees":5,"message":"SMST warmup complete"}
```

**Manual test** (if needed):
```bash
# 1. Note current SMST keys in Redis
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli KEYS "ha:smst:*:nodes" > /tmp/smst_keys_before.txt

# 2. Restart a miner pod
kubectl --context=nodes -n mainnet delete pod relay-miner-XXXX

# 3. Check new pod logs for warmup
kubectl --context=nodes -n mainnet logs relay-miner-YYYY --tail=100 | \
  grep "SMST warmup complete"

# Expected: loaded_trees count matches number of keys from step 1
```

**Success Criteria**:
- ✅ Logs show "warming up SMST trees from Redis"
- ✅ Logs show "SMST warmup complete" with loaded_trees count
- ✅ Loaded trees count matches SMST keys in Redis

---

### 3. ✅ Late Relay Handling

**What**: Verify late relays (for already-claimed sessions) are dropped gracefully.

**Check metrics**:
```bash
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | \
  grep 'ha_miner_relays_rejected_total{.*reason="session_sealed"}'

# Expected: Counter > 0 if late relays occurred
# ha_miner_relays_rejected_total{supplier="pokt1abc...",reason="session_sealed"} 42
```

**Check logs**:
```bash
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --tail=1000 | \
  grep "dropping late relay - session already claimed"

# Expected (if late relays occurred):
# {"level":"debug","session_id":"abc123","supplier":"pokt1...","message":"dropping late relay - session already claimed"}
```

**Success Criteria**:
- ✅ Metric `relays_rejected_total{reason="session_sealed"}` exists and increments
- ✅ Debug logs show late relays being dropped (not errors!)
- ✅ No "failed to process relay" warnings for "already claimed" errors

---

### 4. ✅ Zero Relay Claim Validation

**What**: Verify claims with 0 relays are never submitted.

**Check metrics**:
```bash
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | \
  grep 'ha_miner_sessions_failed_total{.*reason="zero_relays"}'

# Expected: Should be 0 in normal operation
# If > 0, sessions are being created without relays (investigate why)
```

**Check logs**:
```bash
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --since=1h | \
  grep "skipping claim - session has 0 relays"

# Expected: Should be rare/none in production
```

**Success Criteria**:
- ✅ Metric exists (even if value is 0)
- ✅ If metric > 0, logs explain which sessions had 0 relays
- ✅ No claim transactions submitted for those sessions

---

### 5. ✅ Economic Validation (Unprofitable Claims)

**What**: Verify claims where fee > reward are skipped.

**Check metrics**:
```bash
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | \
  grep 'ha_miner_sessions_failed_total{.*reason="unprofitable"}'

# Expected: May be > 0 for very small sessions
```

**Check logs with economics**:
```bash
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --since=1h | \
  grep "skipping claim.*unprofitable"

# Expected output (if any unprofitable sessions):
# {
#   "level":"warn",
#   "session_id":"abc123",
#   "supplier":"pokt1...",
#   "expected_reward_upokt":450,
#   "estimated_fee_upokt":500,
#   "relay_count":10,
#   "total_compute_units":450,
#   "message":"skipping claim - estimated fee exceeds expected reward (unprofitable)"
# }
```

**Verify calculation**:
```bash
# From log above:
# expected_reward = total_compute_units × compute_units_to_tokens_multiplier
# Example: 450 = 450 × 1

# Fee should be > reward for skip to happen
# 500 > 450 ✅ Correct skip decision
```

**Success Criteria**:
- ✅ Metric `sessions_failed_total{reason="unprofitable"}` exists
- ✅ Logs show economic calculations (reward vs fee)
- ✅ Math is correct: reward < fee for skipped sessions

---

### 6. ✅ SMST Cleanup After Settlement

**What**: Verify SMST data is deleted from Redis after successful settlement.

**How** (manual test):
```bash
# 1. Find a session that will settle soon
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | grep session_relay_count | head -5

# 2. Note its session_id and check SMST exists
SESSION_ID="<from_above>"
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli EXISTS "ha:smst:${SESSION_ID}:nodes"
# Expected: 1 (exists)

# 3. Wait for settlement (check logs)
kubectl --context=nodes -n mainnet logs relay-miner-XXXX -f | \
  grep "session_id\":\"${SESSION_ID}\".*settled"

# 4. Check SMST is deleted
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli EXISTS "ha:smst:${SESSION_ID}:nodes"
# Expected: 0 (deleted)
```

**Check deletion logs**:
```bash
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --since=1h | \
  grep "deleted SMST from memory and Redis"

# Expected:
# {"level":"debug","session_id":"abc123","message":"deleted SMST from memory and Redis"}
```

**Success Criteria**:
- ✅ Logs show "deleted SMST from memory and Redis"
- ✅ Redis key no longer exists after settlement
- ✅ No warnings about "failed to delete SMST from Redis"

---

### 7. ✅ Relay Count Consistency

**What**: Verify relay_count in session metadata matches SMST tree size.

**How**:
```bash
# Get session with relay count
SESSION_DATA=$(kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli GET "ha:miner:sessions:pokt1abc...:${SESSION_ID}")

echo "$SESSION_DATA" | jq .relay_count
# Example: 150

# Check SMST node count (should be similar - may differ by a few due to dedup)
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli HLEN "ha:smst:${SESSION_ID}:nodes"
# Example: 148 (close to 150, difference due to duplicates)
```

**Success Criteria**:
- ✅ SMST node count is close to relay_count (within 5-10%)
- ✅ SMST node count ≤ relay_count (can't be more)

---

### 8. ✅ No Commit Errors

**What**: Verify no Commit() failures in logs.

**How**:
```bash
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --since=2h | \
  grep -i "failed to commit SMST"

# Expected: No output (no errors)
```

**If errors found**:
```bash
# Check full error context
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --since=2h | \
  grep -A 5 "failed to commit SMST"

# Common causes:
# - Redis connection lost
# - Redis out of memory
# - Network partition
```

**Success Criteria**:
- ✅ Zero "failed to commit SMST" errors
- ✅ Zero "failed to update SMST" errors (except "already claimed")

---

## Quick Health Check Commands

**Overall miner health**:
```bash
# Check all miners are running
kubectl --context=nodes -n mainnet get pods -l app.kubernetes.io/name=relay-miner

# Check for crashloops
kubectl --context=nodes -n mainnet get pods -l app.kubernetes.io/name=relay-miner | grep -v Running

# Check recent errors
kubectl --context=nodes -n mainnet logs -l app.kubernetes.io/name=relay-miner --tail=100 --since=10m | \
  grep -i "error\|fatal\|panic" | grep -v "already claimed"
```

**Redis health**:
```bash
# Check Redis memory
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli INFO memory | grep used_memory_human

# Count SMST keys
kubectl --context=nodes -n redis exec redis-standalone-0 -c redis-standalone -- \
  redis-cli KEYS "ha:smst:*:nodes" | wc -l
```

**Metrics snapshot**:
```bash
# Grab all relevant metrics
POD=$(kubectl --context=nodes -n mainnet get pods -l app.kubernetes.io/name=relay-miner -o jsonpath='{.items[0].metadata.name}')

kubectl --context=nodes -n mainnet exec $POD -- curl -s http://localhost:9090/metrics | \
  grep -E "smst_redis|relays_rejected|sessions_failed" > /tmp/metrics_snapshot.txt

cat /tmp/metrics_snapshot.txt
```

---

## Red Flags to Watch For

🚨 **Critical Issues**:
- No SMST keys in Redis after 5 minutes of relay traffic
- `smst_redis_operations_total` not incrementing
- "failed to commit SMST" errors in logs
- Miners crashlooping with SMST-related errors

⚠️ **Warnings**:
- High `relays_rejected_total{reason="session_sealed"}` - streams lagging
- High `sessions_failed_total{reason="unprofitable"}` - gas price too high or rewards too low
- SMST keys not being deleted after settlement - memory leak

✅ **Good Signs**:
- SMST keys exist in Redis
- Metrics incrementing steadily
- Warmup logs on startup
- Clean error logs (no SMST errors)

---

## Timeline for Verification

**Immediate (0-5 min after restart)**:
- ✅ Check pods are running
- ✅ Check startup logs for warmup
- ✅ Check no immediate errors

**Short term (5-30 min)**:
- ✅ Verify SMST keys appear in Redis
- ✅ Verify metrics start incrementing
- ✅ Verify relays are processing

**Medium term (30-120 min)**:
- ✅ Wait for first claim window
- ✅ Verify claim validations work
- ✅ Verify economic checks work

**Long term (2-24 hours)**:
- ✅ Verify SMST cleanup after settlement
- ✅ Monitor memory usage stability
- ✅ Check for any memory leaks

---

## Debugging Commands

**If SMST keys missing**:
```bash
# Check if UpdateTree is being called
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --tail=500 | \
  grep "failed to update SMST"

# Check if relays are being consumed
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | grep relays_consumed_total
```

**If warmup not working**:
```bash
# Check startup sequence
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --tail=200 | \
  grep -E "warming up|warmup complete|failed to warmup"

# Check Redis connectivity
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | grep smst_redis_operations_total
```

**If claims not being validated**:
```bash
# Check claim logs
kubectl --context=nodes -n mainnet logs relay-miner-XXXX --since=1h | \
  grep -E "skipping claim|submitting.*claims"

# Check if sessions have data
kubectl --context=nodes -n mainnet exec relay-miner-XXXX -- \
  curl -s http://localhost:9090/metrics | grep session_relay_count
```
