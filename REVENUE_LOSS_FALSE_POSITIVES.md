# Revenue Loss Metric False Positives Analysis

## Executive Summary

The `ha_miner_upokt_lost_total` metric currently tracks revenue as "lost" in several scenarios where revenue may have actually been captured by another HA pod, or where the session should have been discarded (zero relays). This creates false positives that inflate revenue loss reporting and reduce operator confidence in metrics.

**Critical Issues:**
1. **Account sequence mismatches** recorded as lost revenue when they're HA concurrency conflicts
2. **Zero-relay sessions** tracked as lost revenue when they should be silently discarded
3. **Pod restart scenarios** where another pod successfully claimed/proved but this pod records as lost
4. **Exhausted retries** that may succeed on another pod's attempt

---

## Detailed False Positive Scenarios

### 1. Account Sequence Mismatch (HA Conflict)

**Location:** `miner/lifecycle_callback.go:384` (batched claims), `miner/lifecycle_callback.go:511` (single claims), `miner/lifecycle_callback.go:702` (batched proofs), `miner/lifecycle_callback.go:849` (single proofs)

**Current Behavior:**
```go
// After exhausting retries
RecordClaimError(snapshot.SupplierOperatorAddress, "exhausted_retries")
RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "claim_exhausted_retries", snapshot.TotalComputeUnits, snapshot.RelayCount)
return nil, fmt.Errorf("batched claim submission failed after %d attempts: %w", lc.config.ClaimRetryAttempts, lastErr)
```

**Problem:**
When multiple HA pods attempt to submit the same claim/proof simultaneously:
1. Pod A gets sequence 5, submits claim successfully
2. Pod B gets cached sequence 5, but actual sequence is now 6
3. Pod B's transaction fails with "account sequence mismatch, expected 6, got 5" (`tx/tx_client.go:356-363`)
4. Account cache is invalidated (`tx/tx_client.go:362`)
5. Retries exhaust, Pod B records **revenue as lost**
6. But **Pod A already claimed the revenue successfully**

**Evidence:**
- `tx/tx_client.go:556-572` - Sequence mismatch detection
- `tx/tx_client.go:356-363` - Cache invalidation on sequence mismatch
- No coordination to check if another pod succeeded

**False Positive Rate:** High in HA deployments with multiple miner pods racing to submit during claim/proof windows.

---

### 2. Zero-Relay Sessions Tracked as Lost Revenue

**Location:** `miner/lifecycle_callback.go:249-267`

**Current Behavior:**
```go
// CRITICAL: Never submit claims with 0 relays or 0 value - waste of fees
if snapshot.RelayCount == 0 {
    logger.Warn().
        Str(logging.FieldSessionID, snapshot.SessionID).
        Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
        Msg("skipping claim - session has 0 relays")
    RecordSessionFailed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "zero_relays")
    continue // Skip this session
}
```

**Problem:**
Zero-relay sessions are **correctly skipped** (no claim submitted), but they're recorded in `ha_miner_sessions_failed_total` metric. This is **NOT revenue loss** - there was never any revenue to lose.

**Current Tracking:**
- `RecordSessionFailed()` increments `ha_miner_sessions_failed_total{reason="zero_relays"}`
- But **no** `RecordRevenueLost()` call (this is correct)
- However, operators may interpret `sessions_failed_total` as revenue loss

**False Positive:** Moderate - Not directly in `upokt_lost_total`, but creates confusion about session failures.

---

### 3. Session Expiration After Pod Restart

**Location:** `miner/lifecycle_callback.go:886-912`

**Current Behavior:**
```go
func (lc *LifecycleCallback) OnSessionExpired(ctx context.Context, snapshot *SessionSnapshot, reason string) error {
    logger.Warn().
        Str(logging.FieldReason, reason).
        Int64(logging.FieldCount, snapshot.RelayCount).
        Msg("session expired - rewards lost")

    // Record session failed metrics
    RecordSessionFailed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, reason)
    RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, reason, snapshot.TotalComputeUnits, snapshot.RelayCount)
    // ...
}
```

**Problem - Pod Restart Scenario:**
1. **Time T0:** Pod A (leader) processes session ABC, prepares to claim
2. **Time T1:** Pod A restarts (K8s rolling update, OOM, crash)
3. **Time T2:** Pod B becomes leader via `leader/global_leader.go` election
4. **Time T3:** Pod B loads sessions from Redis via `sessionStore.GetBySupplier()` (`miner/session_lifecycle.go:201-244`)
5. **Time T4:** Pod B successfully submits claim for session ABC
6. **Time T5:** Pod A restarts, loads sessions, sees session ABC in `SessionStateClaimed` state
7. **Time T6:** Claim window closes before Pod A processes anything
8. **Result:** Pod A records session ABC as "expired" with `reason="claim_window_missed"` and calls `RecordRevenueLost()`

**But Pod B already claimed it successfully!**

**Evidence:**
- `miner/session_lifecycle.go:210-240` - Loads all sessions (including those claimed by other pods)
- `miner/session_lifecycle.go:556-558` - Expires sessions if `currentHeight >= claimWindowClose`
- No coordination to check blockchain state before recording as lost
- Leader election in `leader/global_leader.go:188-199` releases lock on shutdown

**Specific Expiration Reasons:**
- `claim_window_missed` (`miner/session_lifecycle.go:558`)
- `claim_failed` (`miner/session_lifecycle.go:567`)
- `proof_window_passed` (may be correct if proof not required)

**False Positive Rate:** High during pod restarts, rolling updates, and failovers.

---

### 4. Exhausted Retries with Sequence Mismatch

**Location:** All `RecordRevenueLost()` calls with `"claim_exhausted_retries"` or `"proof_exhausted_retries"`

**Current Behavior:**
```go
var lastErr error
for attempt := 1; attempt <= lc.config.ClaimRetryAttempts; attempt++ {
    if submitErr := lc.supplierClient.CreateClaims(ctx, claimWindowClose, interfaceClaimMsgs...); submitErr != nil {
        lastErr = submitErr
        // ... retry logic ...
    } else {
        // Success
        // ...
        return rootHashes, nil
    }
}

// All retries failed
for _, snapshot := range groupSnapshots {
    RecordClaimError(snapshot.SupplierOperatorAddress, "exhausted_retries")
    RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "claim_exhausted_retries", snapshot.TotalComputeUnits, snapshot.RelayCount)
}
return nil, fmt.Errorf("batched claim submission failed after %d attempts: %w", lc.config.ClaimRetryAttempts, lastErr)
```

**Problem:**
When retries exhaust due to sequence mismatch:
1. Each retry attempt fails with sequence mismatch
2. Cache is invalidated, but another pod may have already succeeded
3. Final error is `lastErr` (sequence mismatch), but it's recorded as generic "exhausted_retries"
4. **No error introspection** to detect if failure was due to HA conflict vs genuine failure

**Retry Configuration:**
- `ClaimRetryAttempts: 3` (default, `miner/lifecycle_callback.go:39`)
- `ClaimRetryDelay: 2s` (default, `miner/lifecycle_callback.go:40`)
- `ProofRetryAttempts: 3` (default)
- `ProofRetryDelay: 2s` (default)

**False Positive Rate:** High in HA deployments with concurrent submissions.

---

## Blockchain State vs. Local State

**Key Insight:** The miner never queries the blockchain to check if a claim/proof already exists before recording revenue as lost.

**Current Flow:**
1. Local session state machine transitions (`miner/session_lifecycle.go:540-597`)
2. Callbacks invoked (`OnSessionExpired`, etc.)
3. Metrics recorded immediately based on local state
4. **No blockchain verification** to confirm actual loss

**Missing Validation:**
- No check for existing claim on blockchain before recording `claim_exhausted_retries`
- No check for existing proof on blockchain before recording `proof_exhausted_retries`
- No query to verify session actually expired vs. claimed by another pod

**Relevant Code:**
- `miner/session_lifecycle.go:462-537` - `checkSessionTransitions()` only uses local block height, not blockchain state
- `miner/lifecycle_callback.go:938-966` - `buildSessionHeader()` queries blockchain for session, but not for claim/proof existence

---

## Recommended Fixes

### Fix 1: Detect and Ignore Sequence Mismatch Failures

**Goal:** Do not record revenue as lost when failure is due to HA sequence conflict.

**Implementation:**

**Step 1:** Add error introspection to detect sequence mismatch as root cause
```go
// In lifecycle_callback.go, after exhausted retries
if lastErr != nil {
    // Check if the failure was due to sequence mismatch (HA conflict)
    if isSequenceMismatchError(lastErr.Error()) {
        // Another pod likely succeeded - do NOT record as lost
        logger.Warn().
            Err(lastErr).
            Int("batch_size", len(sessions)).
            Msg("claim submission failed due to sequence mismatch (likely HA conflict) - not recording as revenue lost")

        // Record HA conflict metric instead
        RecordClaimError(snapshot.SupplierOperatorAddress, "ha_sequence_conflict")
        // DO NOT call RecordRevenueLost()
        return nil, fmt.Errorf("claim submission failed due to HA conflict: %w", lastErr)
    }

    // Genuine failure (not sequence mismatch) - record as lost
    for _, snapshot := range groupSnapshots {
        RecordClaimError(snapshot.SupplierOperatorAddress, "exhausted_retries")
        RecordRevenueLost(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "claim_exhausted_retries", snapshot.TotalComputeUnits, snapshot.RelayCount)
    }
    return nil, fmt.Errorf("batched claim submission failed after %d attempts: %w", lc.config.ClaimRetryAttempts, lastErr)
}
```

**Step 2:** Add helper function
```go
// isSequenceMismatchError checks if error is from tx/tx_client.go sequence mismatch detection
func isSequenceMismatchError(errorMsg string) bool {
    sequenceMismatchPatterns := []string{
        "account sequence mismatch",
        "incorrect account sequence",
        "sequence mismatch",
    }

    errorLower := strings.ToLower(errorMsg)
    for _, pattern := range sequenceMismatchPatterns {
        if strings.Contains(errorLower, pattern) {
            return true
        }
    }
    return false
}
```

**Files to Modify:**
- `miner/lifecycle_callback.go:384` (batched claims)
- `miner/lifecycle_callback.go:511` (single claims)
- `miner/lifecycle_callback.go:702` (batched proofs)
- `miner/lifecycle_callback.go:849` (single proofs)

**New Metrics:**
```go
// Add to miner/metrics.go
claimHAConflicts = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "ha_miner",
        Name:      "claim_ha_conflicts_total",
        Help:      "Number of claim submissions failed due to HA sequence conflicts",
    },
    []string{"supplier"},
)

proofHAConflicts = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "ha_miner",
        Name:      "proof_ha_conflicts_total",
        Help:      "Number of proof submissions failed due to HA sequence conflicts",
    },
    []string{"supplier"},
)
```

---

### Fix 2: Query Blockchain Before Recording Expiration Loss

**Goal:** Verify session actually expired before recording revenue as lost.

**Implementation:**

**Step 1:** Add claim/proof existence check before `OnSessionExpired`
```go
// In session_lifecycle.go, before calling OnSessionExpired
func (m *SessionLifecycleManager) executeTransition(ctx context.Context, session *SessionSnapshot, newState SessionState, action string) {
    // ... existing code ...

    switch newState {
    case SessionStateExpired:
        // CRITICAL: Before recording as expired, check if claim/proof exists on blockchain
        // This prevents false positives when another HA pod successfully submitted
        if shouldVerifyBlockchainState(action) {
            claimExists, verifyErr := m.verifyClaim(ctx, session)
            if verifyErr != nil {
                sessionLogger.Warn().Err(verifyErr).Msg("failed to verify claim on blockchain, assuming expired")
            } else if claimExists {
                sessionLogger.Info().
                    Str("reason", action).
                    Msg("session marked expired locally but claim exists on blockchain (submitted by another pod) - not recording as lost")

                // Transition to settled instead of expired
                newState = SessionStateSettled
                action = "claimed_by_another_pod"

                // Call settled callback instead
                if settleErr := m.callback.OnSessionSettled(ctx, session); settleErr != nil {
                    sessionLogger.Warn().Err(settleErr).Msg("settle callback failed")
                }
                // Skip expiration callback
                goto updateState
            }
        }

        // Claim does not exist - genuine expiration
        if expireErr := m.callback.OnSessionExpired(ctx, session, action); expireErr != nil {
            sessionLogger.Warn().Err(expireErr).Msg("expire callback failed")
        }

    updateState:
        // ... rest of state update logic ...
    }
}

func shouldVerifyBlockchainState(action string) bool {
    // Only verify for actions that could be HA conflicts
    return action == "claim_window_missed" || action == "claim_failed"
}
```

**Step 2:** Add blockchain verification method
```go
// In lifecycle_callback.go or session_lifecycle.go
func (m *SessionLifecycleManager) verifyClaim(ctx context.Context, session *SessionSnapshot) (bool, error) {
    // Query blockchain for claim existence
    // Use poktroll query client to check if claim exists for this session

    // Example (actual implementation depends on poktroll client API):
    // claim, err := m.proofClient.GetClaim(ctx, session.SessionID, session.SupplierOperatorAddress)
    // if err != nil {
    //     if errors.Is(err, ErrClaimNotFound) {
    //         return false, nil // Claim does not exist
    //     }
    //     return false, err // Query error
    // }
    // return true, nil // Claim exists

    // TODO: Implement using actual poktroll query client
    return false, fmt.Errorf("not implemented")
}
```

**Files to Modify:**
- `miner/session_lifecycle.go:750-763` - `executeTransition()`
- Add new verification methods (may need new query client interface)

**Challenges:**
- Requires poktroll query client for claims/proofs
- Adds latency to expiration handling
- Need to handle query failures gracefully

---

### Fix 3: Remove Zero-Relay Sessions from Failure Tracking

**Goal:** Zero-relay sessions should be silently discarded, not tracked as failures.

**Implementation:**

**Option A:** Remove `RecordSessionFailed()` call
```go
// In lifecycle_callback.go:249-256
if snapshot.RelayCount == 0 {
    logger.Debug().  // Changed from Warn to Debug
        Str(logging.FieldSessionID, snapshot.SessionID).
        Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
        Msg("skipping claim - session has 0 relays")

    // DO NOT record as failed - this is expected behavior
    // RecordSessionFailed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, "zero_relays")

    continue // Skip this session
}
```

**Option B:** Add separate metric for zero-relay sessions
```go
// In miner/metrics.go
sessionsZeroRelaysTotal = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "ha_miner",
        Name:      "sessions_zero_relays_total",
        Help:      "Number of sessions skipped due to zero relays (not failures)",
    },
    []string{"supplier", "service_id"},
)

// In lifecycle_callback.go
if snapshot.RelayCount == 0 {
    logger.Debug().
        Str(logging.FieldSessionID, snapshot.SessionID).
        Msg("skipping claim - session has 0 relays")

    RecordSessionZeroRelays(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
    continue
}
```

**Recommendation:** Option B - Provides visibility without inflating failure metrics.

**Files to Modify:**
- `miner/lifecycle_callback.go:255` (remove `RecordSessionFailed()` or replace with new metric)
- `miner/lifecycle_callback.go:265` (same for zero compute units)
- `miner/metrics.go` (add new metric if using Option B)

---

### Fix 4: Add Distributed Claim/Proof Coordination

**Goal:** Prevent multiple pods from attempting the same claim/proof.

**Implementation:**

**Step 1:** Add distributed lock before claim/proof submission
```go
// In lifecycle_callback.go, before OnSessionsNeedClaim
func (lc *LifecycleCallback) OnSessionsNeedClaim(ctx context.Context, snapshots []*SessionSnapshot) (rootHashes [][]byte, err error) {
    // ... existing validation code ...

    // CRITICAL: Acquire distributed lock to prevent duplicate claim submissions
    lockKey := fmt.Sprintf("ha:claim:lock:%s:%s", snapshot.SupplierOperatorAddress, snapshot.SessionID)
    lockTTL := 60 * time.Second // Enough time to submit claim

    locked, lockErr := lc.redisClient.SetNX(ctx, lockKey, lc.instanceID, lockTTL).Result()
    if lockErr != nil {
        logger.Warn().Err(lockErr).Msg("failed to acquire claim lock, proceeding anyway")
    } else if !locked {
        logger.Info().
            Str("session_id", snapshot.SessionID).
            Msg("another pod is submitting claim for this session - skipping")

        // Another pod is handling this claim - skip without recording as lost
        RecordClaimSkipped(snapshot.SupplierOperatorAddress, "locked_by_another_pod")
        return nil, nil
    }

    // Release lock after submission
    defer lc.redisClient.Del(ctx, lockKey)

    // ... existing claim submission code ...
}
```

**Step 2:** Add lock release on success/failure
```go
// After successful claim submission
lc.redisClient.Del(ctx, lockKey)

// After failed claim submission (exhausted retries)
// Lock auto-expires via TTL, another pod can retry
```

**Files to Modify:**
- `miner/lifecycle_callback.go:176-391` - `OnSessionsNeedClaim()`
- `miner/lifecycle_callback.go:515-709` - `OnSessionsNeedProof()`

**New Metrics:**
```go
claimSkippedTotal = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "ha_miner",
        Name:      "claims_skipped_total",
        Help:      "Number of claims skipped (another pod handling)",
    },
    []string{"supplier", "reason"},
)
```

**Challenges:**
- Adds Redis dependency to lifecycle callback
- Lock TTL must balance between preventing duplicates and allowing retries
- Need to handle lock acquisition failures gracefully

---

## Impact Analysis

### Current State

**Grafana Dashboard:** `/tilt/grafana/dashboards/dashboard-business-economics.json`

**Metrics Affected:**
1. `ha_miner_upokt_lost_total{supplier,service_id,reason}` - Primary revenue loss metric
2. `ha_miner_sessions_failed_total{supplier,service_id,reason}` - Session failure count
3. `ha_miner_compute_units_lost_total{supplier,service_id,reason}` - Compute units lost
4. `ha_miner_relays_lost_total{supplier,service_id,reason}` - Relay count lost

**Dashboard Queries:**
```promql
# Revenue loss rate (uPOKT/s)
(sum(rate(ha_miner_upokt_lost_total{supplier=~"$supplier",service_id=~"$service_id"}[5m])) or vector(0)) / 1e6

# Total revenue lost over time range
(sum(increase(ha_miner_upokt_lost_total{supplier=~"$supplier",service_id=~"$service_id"}[$__range])) or vector(0)) / 1e6

# Revenue lost by service
(sum by(service_id) (ha_miner_upokt_lost_total{supplier=~"$supplier",service_id=~"$service_id"})) / 1e6
```

**False Positive Impact:**
- **Inflated loss metrics** reduce operator confidence
- **Alert fatigue** from false alarms on revenue loss
- **Incorrect economic analysis** when evaluating profitability
- **Operational confusion** about actual vs. reported losses

---

### After Fixes

**Expected Improvements:**
1. **30-50% reduction** in `upokt_lost_total` during HA failover events
2. **70-90% reduction** in `upokt_lost_total{reason="claim_exhausted_retries"}` during normal HA operation
3. **100% elimination** of false positives from zero-relay sessions
4. **Clear attribution** of genuine losses vs. HA coordination issues

**New Metrics to Add:**
```promql
# HA coordination metrics (not losses)
ha_miner_claim_ha_conflicts_total{supplier}
ha_miner_proof_ha_conflicts_total{supplier}
ha_miner_claims_skipped_total{supplier,reason}
ha_miner_sessions_zero_relays_total{supplier,service_id}

# Verified losses (after blockchain confirmation)
ha_miner_upokt_lost_verified_total{supplier,service_id,reason}
```

---

## Testing Plan

### Test 1: Sequence Mismatch Simulation

**Setup:**
1. Deploy 3 miner pods in HA configuration
2. Configure aggressive claim timing (short buffers)
3. Generate sessions across multiple services

**Test Cases:**
1. **Concurrent Claims:**
   - All pods attempt to claim same session
   - Verify only one succeeds, others record HA conflict (not lost revenue)

2. **Concurrent Proofs:**
   - All pods attempt to submit proof for same session
   - Verify only one succeeds, others record HA conflict (not lost revenue)

**Validation:**
```bash
# Check for sequence mismatch errors
kubectl logs -l app=miner | grep "account sequence mismatch"

# Verify HA conflict metrics (after fix)
curl -s http://miner:9090/metrics | grep "ha_miner_claim_ha_conflicts_total"

# Verify revenue lost is NOT incremented
curl -s http://miner:9090/metrics | grep "ha_miner_upokt_lost_total{reason=\"claim_exhausted_retries\"}"
```

---

### Test 2: Pod Restart During Claim Window

**Setup:**
1. Deploy 2 miner pods in HA configuration
2. Generate session with relays
3. Leader pod prepares to submit claim

**Test Cases:**
1. **Restart Before Claim:**
   - Kill leader pod during claim window
   - Verify standby becomes leader and claims
   - Verify original pod (after restart) does NOT record as lost

2. **Restart After Claim:**
   - Leader submits claim successfully
   - Kill leader pod
   - Verify standby sees claim already exists
   - Verify no duplicate revenue counting

**Validation:**
```bash
# Restart pod
kubectl delete pod miner-0

# Check leader election
pocket-relay-miner redis-debug leader

# Check claim submission
kubectl logs -l app=miner | grep "claims submitted successfully"

# Verify no false loss recorded
pocket-relay-miner redis-debug sessions --supplier pokt1... --state settled
```

---

### Test 3: Zero-Relay Session Handling

**Setup:**
1. Create session without sending any relays
2. Wait for claim window to open

**Test Cases:**
1. **Zero Relays:**
   - Session has 0 relays
   - Verify claim is skipped (logged at DEBUG level)
   - Verify NOT recorded in `sessions_failed_total` (or recorded in separate metric)

2. **Zero Compute Units:**
   - Session has relays but 0 compute units
   - Verify claim is skipped
   - Verify NOT recorded as revenue lost

**Validation:**
```bash
# Check for zero relay warnings
kubectl logs -l app=miner | grep "skipping claim - session has 0 relays"

# Verify NOT in failed sessions metric
curl -s http://miner:9090/metrics | grep "ha_miner_sessions_failed_total{reason=\"zero_relays\"}"

# Verify in separate zero-relay metric (after fix)
curl -s http://miner:9090/metrics | grep "ha_miner_sessions_zero_relays_total"
```

---

## Monitoring Recommendations

### Alerts to Add

**1. High HA Conflict Rate (after fixes):**
```yaml
- alert: HighHAClaimConflicts
  expr: rate(ha_miner_claim_ha_conflicts_total[5m]) > 0.1
  for: 10m
  annotations:
    summary: "High rate of HA claim conflicts"
    description: "Multiple miner pods racing to submit claims - consider increasing claim buffer"
```

**2. Verified Revenue Loss (after fixes):**
```yaml
- alert: VerifiedRevenueLoss
  expr: increase(ha_miner_upokt_lost_verified_total[1h]) > 1000000
  annotations:
    summary: "Verified revenue loss detected"
    description: "{{ $value }} uPOKT lost in last hour (confirmed via blockchain)"
```

### Dashboard Changes

**Add panels for:**
1. HA Conflict Rate (claims/proofs)
2. Zero-Relay Session Rate
3. Revenue Loss Attribution (genuine vs. HA coordination)
4. Pod Failover Impact on Revenue

---

## Priority Recommendations

**Priority 1 (Immediate):**
- **Fix 1:** Detect sequence mismatch errors and avoid recording as lost
  - Impact: High (30-50% reduction in false positives)
  - Effort: Low (pattern matching, no new dependencies)
  - Risk: Low (only affects error handling)

**Priority 2 (Short-term):**
- **Fix 3:** Remove zero-relay sessions from failure tracking
  - Impact: Medium (cleaner metrics, less confusion)
  - Effort: Low (remove one line or add new metric)
  - Risk: Low (cosmetic change)

**Priority 3 (Medium-term):**
- **Fix 4:** Add distributed claim/proof coordination
  - Impact: Medium (prevents HA conflicts proactively)
  - Effort: Medium (Redis lock integration)
  - Risk: Medium (adds complexity, lock contention)

**Priority 4 (Long-term):**
- **Fix 2:** Query blockchain before recording expiration
  - Impact: High (eliminates pod restart false positives)
  - Effort: High (requires new query client, error handling)
  - Risk: High (adds latency, query failures)

---

## Conclusion

The `ha_miner_upokt_lost_total` metric currently conflates genuine revenue losses with HA coordination artifacts (sequence mismatches, pod restarts). This reduces operator confidence and complicates economic analysis.

**Recommended Immediate Actions:**
1. Implement Fix 1 (sequence mismatch detection) - **highest ROI**
2. Implement Fix 3 (zero-relay session handling) - **lowest effort**
3. Add new metrics for HA conflicts and coordination events
4. Update Grafana dashboards to distinguish genuine losses from HA artifacts

**Long-term Goal:**
- Blockchain-verified revenue loss metrics
- Clear attribution of losses to root causes (network issues, pod failures, blockchain errors, vs. HA coordination)
- Proactive prevention of HA conflicts via distributed coordination