# Codebase Concerns

**Analysis Date:** 2026-02-02

## Tech Debt

**Panic in JSON marshaling:**
- Issue: `mustMarshalJSON` in `miner/params_refresher.go:399` panics on error instead of returning error
- Files: `miner/params_refresher.go:395-402`
- Impact: If JSON marshaling fails (should be rare for simple structs), the entire miner service crashes
- Fix approach: Either add proper error handling returning error, or use a guard that validates the struct is marshalable before calling

**WebSocket handshake signature verification unimplemented:**
- Issue: PATH v2 WebSocket handshake signature validation is not implemented - currently accepts all handshakes permissively
- Files: `relayer/websocket.go:1107-1117`
- Impact: PATH v2 gateways can send invalid or spoofed handshakes without detection. When PATH v2 spec is finalized, this MUST be implemented
- Fix approach:
  1. Reconstruct signed message structure matching PATH's signing logic
  2. Verify signature using application's public key from session or blockchain
  3. Log WARN if verification fails but allow connection for backward compatibility (gradual rollout)

**WebSocket compute units hardcoded to 1:**
- Issue: WebSocket relays use hardcoded compute units value of 1 instead of querying service config
- Files: `relayer/websocket.go:1010-1012`
- Impact: Metering for WebSocket relays is inaccurate until fixed. Suppliers may be undercompensated if services have higher compute unit costs
- Fix approach: Extract actual compute units from `p.config.GetServiceTimeoutProfile(serviceID)` or implement lazy loading from relay processor

**Compute units not extracted for gRPC relays:**
- Issue: gRPC relay service does not fetch actual compute units from service config
- Files: `relayer/relay_grpc_service.go:220`
- Impact: Same as WebSocket - gRPC relays hardcoded with incorrect compute units (affects metering accuracy)
- Fix approach: Load from service config during initialization or first relay request

**Ring params queried on every relay validation:**
- Issue: `rings/client.go:316-320` re-queries shared params for every `GetRingAddressesAtBlock` call instead of caching
- Files: `rings/client.go:310-326`
- Impact: Adds unnecessary network latency to validation path; should be cached or use replay observable pattern
- Fix approach: Use `ModuleParamsClient` replay observable's `#Last` method once implemented in poktroll; additionally use historical params values (params at session block height, not current)

**Cache entity eviction not implemented:**
- Issue: Applications/services/suppliers that are NotFound on chain accumulate in cache forever
- Files: `cache/orchestrator.go:491`, `cache/orchestrator.go:563`, `cache/orchestrator.go:620`
- Impact: Over time, memory usage grows as cache stores references to unstaked entities. No auto-cleanup of entities with repeated NotFound errors
- Fix approach: Track consecutive NotFound count per entity; remove from `knownApps`, `knownServices`, `knownSuppliers` after N consecutive failures (e.g., 3-5)

**Unstaking grace period not enforced:**
- Issue: Supplier unstaking status checked at current session, not at session end height when unstaking takes effect
- Files: `cache/supplier_cache.go:63-67`, `cache/supplier_cache.go:88-91`
- Impact: Suppliers may be marked inactive before their current session ends, potentially breaking active sessions
- Fix approach: Compare supplier unstake session end height against current session end height (not just check if unstaking is pending)

## Known Bugs

**Session determinism logging:**
- Symptoms: Session hashes logged at cache layer suggest non-deterministic session serialization detected during testing
- Files: `cache/session_cache.go:205-214`
- Trigger: Unclear - appears to be intermittent serialization differences between JSON and proto marshaling
- Workaround: Currently only logged with debug information. If sessions are actually non-deterministic, claims/proofs may fail verification on-chain
- Note: This debug logging block should be removed once root cause is identified and fixed

**Terminal session cleanup required:**
- Symptoms: Terminal sessions (proved, proof_window_closed, etc.) remain in activeSessions in-memory map even after Redis updates
- Files: `miner/session_lifecycle.go:612-627` (bug fix comment at line 613)
- Trigger: When session state callback returns error after updating Redis but before cleanup
- Impact: Causes worker pool exhaustion as each block processes stale terminal sessions. Fixed in place but indicates a deeper callback error handling issue
- Workaround: Cleanup loop now explicitly removes terminal sessions from in-memory tracking (existing fix is in place)

## Security Considerations

**Hardcoded compute units in load testing:**
- Risk: Load test HTTP client (`cmd/relay/http.go`) hardcodes compute units instead of validating against real service config
- Files: `cmd/relay/http.go` (load test client)
- Current mitigation: Load tests are development/staging only; not exposed in production
- Recommendations: When production-testing actual relayer, ensure compute units are validated against live service config

**Session signature validation timing:**
- Risk: Ring signatures validated on first relay request for WebSocket sessions. If attacker can send non-first requests to WebSocket, they bypass validation
- Files: `relayer/websocket.go` (handshake validation)
- Current mitigation: Relayer validates via RelayProcessor before publishing; WebSocket bridge is first touchpoint before RelayProcessor
- Recommendations: Ensure WebSocket relay handler always calls RelayProcessor with full validation before processing payload

**Supplier key hot reload panic concern:**
- Risk: If response signer keys need to be updated via hot reload, accessing `p.responseSigner` without sync.Map protection could cause panic
- Files: `relayer/proxy.go:1556` (TODO comment about sync map)
- Current mitigation: Response signer is set once during startup and not modified
- Recommendations: If key rotation is implemented, must use sync.Map or RWMutex to protect signer field access

## Performance Bottlenecks

**Ring signature verification on every relay:**
- Problem: Signature verification happens in hot path; no caching of ring validation results
- Files: `rings/client.go` (GetRingAddressesAtBlock calls on every relay)
- Cause: Params and ring membership queried per-relay instead of being cached
- Improvement path: Cache ring membership per session; verify signature only once at session creation time

**Session params queried on every relay meter calculation:**
- Problem: Session parameters fetched from chain/cache on each relay meter operation
- Files: `relayer/relay_meter.go` (meter lookups in hot path)
- Cause: No fast-path cache for session params during relay processing
- Improvement path: Pre-load session params when session first seen; cache invalidation via pub/sub

**Synchronous block subscriber reconnection:**
- Problem: Redis block subscriber uses exponential backoff reconnection (1s → 30s max) during which relayers don't see block events
- Files: `cache/block_subscriber.go` (reconnection logic)
- Cause: Network hiccups cause temporary event stream loss until reconnect succeeds
- Improvement path: Implement multi-source block delivery (blockchain RPC fallback during Redis outage)

## Fragile Areas

**Lifecycle callback state machine:**
- Files: `miner/lifecycle_callback.go` (1898 lines)
- Why fragile: Massive file with complex session state transitions (claim window, proof window, timeouts, error states). Multiple DEBUG/TEST environment variables can alter flow
- Safe modification: Any window timeout logic changes must account for all terminal states listed in comments
- Test coverage: Requires integration tests with miniredis; unit test coverage of window edge cases is strong but state transition coverage incomplete

**Session state machine in miner:**
- Files: `miner/session_lifecycle.go` (1207 lines)
- Why fragile: Manages active session tracking with potential for race between Redis updates and in-memory map. Terminal sessions can leak
- Safe modification: Any state transition changes must ensure terminal sessions are explicitly removed from activeSessions map
- Test coverage: Session state transitions tested; but HA failover scenarios (session in Redis but not in active map) need stronger coverage

**Relay processor proxy integration:**
- Files: `relayer/proxy.go` (1842 lines)
- Why fragile: All transport protocols (HTTP, WebSocket, gRPC, Streaming) converge at relay pipeline. Missing dependencies at initialization cause silent degradation
- Safe modification: InitializeRelayPipeline explicitly checks all dependencies; always verify dependencies are set before calling this method
- Test coverage: Individual transport tests exist; but combined integration tests (all transports under load) are integration tests only

**Cache three-tier consistency:**
- Files: `cache/orchestrator.go` (893 lines), multiple cache implementations
- Why fragile: L1 (in-memory) → L2 (Redis) → L3 (chain) invalidation must be atomic across all instances via pub/sub
- Safe modification: Any cache update must publish invalidation event; missing pub/sub publish will cause stale L1 caches on replicas
- Test coverage: Individual cache tests with miniredis are strong; multi-instance invalidation race conditions need distributed test scenarios

**SMST manager tree operations during flush:**
- Files: `miner/redis_smst_manager_test.go:` (test shows concurrent flush concern)
- Why fragile: SMST operations (get, set, delete) must be sequentially consistent during flush. Concurrent updates during flush can cause tree inconsistency
- Safe modification: All SMST operations must acquire appropriate locks; flush operation must be exclusive
- Test coverage: Redis mapstore tests have benchmarks; but concurrent update-during-flush scenarios should be in race detector tests

## Scaling Limits

**Supplier registry pub/sub event queue:**
- Current capacity: 2000 events buffered in channel
- Files: `miner/supplier_registry.go:` (eventCh buffer size 2000)
- Limit: If supplier updates come faster than processing (>2000 events pending), channel fills up
- Scaling path: Either increase buffer size, or implement backpressure/dropping of old events

**Claim and proof request queues:**
- Current capacity: Claim queue 2000, Proof queue ProofQueueSize
- Files: `miner/claim_pipeline.go:2000`, `miner/proof_pipeline.go:ProofQueueSize`
- Limit: Queue filling indicates claim/proof submission is bottlenecked
- Scaling path: Monitor queue depth metrics; increase worker pool size or optimize submission latency

**Worker pool sizing for cache orchestrator:**
- Current capacity: Auto-calculated based on CPU cores
- Files: `cache/orchestrator.go:` (masterPoolSize auto-calculated)
- Limit: If cache refresh time > block time, orchestrator falls behind and misses refreshes
- Scaling path: Monitor cache refresh latency; may need explicit poolSize config if auto-sizing insufficient

**HTTP connection pool limits for relayer:**
- Current capacity: 500 max connections per host (5x increase from v0.x)
- Files: `relayer/config.go:436-447`
- Limit: At 1000 RPS with 500ms backend latency, approaches the 500 connection limit
- Scaling path: If backend latency increases >500ms, must increase MaxConnsPerHost or implement request queuing

## Dependencies at Risk

**cometbft/cometbft v1.0.0-rc1:**
- Risk: Pre-release version pinned; likely to have breaking changes before v1.0.0 final release
- Impact: CometBFT API changes will break BlockSubscriber integration
- Migration plan: Upgrade to v1.0.0 final once available; test BlockSubscriber compatibility with final release version

**pokt-network/poktroll v0.1.31-rc1:**
- Risk: Protocol pre-release version; session/claim/proof structures may change
- Impact: Claim/proof submission could fail if structures change
- Migration plan: Coordinate with poktroll release cycle; test each major/minor version upgrade thoroughly

**github.com/DataDog/zstd v1.5.7 (removed in recent commit):**
- Risk: Was used for response compression but removed due to memory leak issues
- Impact: Now using gzip only; no zstd compression available (tradeoff for memory safety)
- Recommendation: If compression is needed, verify gzip pool implementation doesn't leak memory

## Missing Critical Features

**Governance param change monitoring:**
- Problem: If Pocket governance changes shared params (e.g., session duration) mid-session, relayers continue using cached values
- Blocks: Cache invalidation during active sessions; affects relay metering accuracy if params change
- Workaround: Governance changes typically scheduled for session boundaries; manual cache invalidation available via pub/sub

**Unstaking grace period enforcement:**
- Problem: Unstaking suppliers are marked inactive immediately instead of being allowed to finish current session
- Blocks: Proper session lifecycle for unstaking suppliers; may break active relays prematurely
- Workaround: Mark as inactive but continue serving until session end (not implemented, only TODO'd)

**Historical params at block height:**
- Problem: Ring client queries current params instead of params that existed at relay's session block height
- Blocks: Accurate ring signature verification for old sessions; affects replay scenarios
- Workaround: Currently accepts current params (works if params are stable)

## Test Coverage Gaps

**WebSocket transport with actual signatures:**
- What's not tested: WebSocket handshakes with real application signatures (PATH v2 format) - validation is unimplemented
- Files: `relayer/websocket.go:1107-1117` (TODO comment indicates this)
- Risk: If PATH v2 sends invalid signature, it will be accepted without detection until implementation complete
- Priority: HIGH - must be tested before PATH v2 integration

**Concurrent session state transitions:**
- What's not tested: Race conditions between session completion in Redis and lifecycle manager cleanup
- Files: `miner/session_lifecycle.go` (terminal session cleanup bug fix suggests race condition)
- Risk: Terminal sessions leaking into next block processing if callback error handling fails
- Priority: HIGH - use `go test -race` to detect these

**Integration tests converted to unit tests:**
- What's not tested: Several important scenarios downgraded from e2e to unit tests with miniredis
- Files: `miner/claim_pipeline_test.go:519`, `miner/submission_timing_test.go` (marked TODO(e2e))
- Risk: Tests may pass with miniredis but fail with production Redis due to timing/pub/sub behavior differences
- Priority: MEDIUM - should be re-implemented as e2e tests with testcontainers once infrastructure available

**Block subscriber reconnection scenarios:**
- What's not tested: BlockSubscriber behavior when Redis becomes unavailable (integration test exists but limited)
- Files: `client/block_subscriber_integration_test.go` (uses mock CometBFT, not actual Redis behavior)
- Risk: Real Redis disconnections may cause missed block events or delayed reconnection beyond acceptable threshold
- Priority: MEDIUM - implement chaos testing with actual Redis

**HTTP/2 server push and streaming:**
- What's not tested: HTTP/2 specific features like server push, multiplexing under high concurrency
- Files: `relayer/proxy.go` (HTTP/2 server configured)
- Risk: HTTP/2 protocol edge cases may cause connection pooling to fail under sustained load
- Priority: LOW - most clients use HTTP/1.1; HTTP/2 load testing recommended before production

---

*Concerns audit: 2026-02-02*
