---
phase: 02-characterization-tests
plan: 08
subsystem: relayer
completed: 2026-02-03
duration: 8 minutes

requires:
  - 02-06 # testutil import cycle fixed

provides:
  - gRPC transport characterization tests
  - Real gRPC client/server test infrastructure
  - Error mapping documentation (4xx wrapped, 5xx not wrapped)

affects:
  - Any future gRPC transport refactoring
  - gRPC error handling changes
  - gRPC interceptor modifications

tech-stack:
  added: []
  patterns:
    - gRPC in-process testing with net.Listen
    - gRPC stream manual creation for testing
    - Real gRPC server/client (no mocks)

key-files:
  created:
    - relayer/relay_grpc_service_test.go
  modified: []

decisions:
  - decision: Use nil relayPipeline for most tests
    rationale: Validation/metering not needed for transport layer tests
    timestamp: 2026-02-03
  - decision: Document actual timeout behavior (DeadlineExceeded)
    rationale: Characterization tests document real behavior, not expected
    timestamp: 2026-02-03
  - decision: Use real gRPC client/server infrastructure
    rationale: No mocks rule, tests actual gRPC transport layer
    timestamp: 2026-02-03

tags: [tests, characterization, grpc, transport, gap-closure]
---

# Phase 2 Plan 08: gRPC Transport Characterization Tests Summary

**One-liner:** Added 825 lines of characterization tests for gRPC relay transport covering unary relays, error mapping, concurrency, and gRPC-Web detection.

## Objective

Close CHAR-03 gap by adding comprehensive characterization tests for the gRPC transport layer (relay_grpc_service.go, grpc_interceptor.go, grpc_web.go).

## What Was Built

### gRPC Transport Characterization Tests (825 lines)

Created `relayer/relay_grpc_service_test.go` with 15 test cases covering:

**Core Relay Handling:**
1. `TestGRPCRelay_UnarySuccess` - Valid unary request returns signed response
2. `TestGRPCRelay_MissingServiceID` - Returns InvalidArgument gRPC error
3. `TestGRPCRelay_MissingSupplierAddress` - Returns InvalidArgument gRPC error
4. `TestGRPCRelay_MissingSessionHeader` - Returns InvalidArgument gRPC error
5. `TestGRPCRelay_InvalidPayload` - Malformed protobuf returns InvalidArgument
6. `TestGRPCRelay_NoSignerForSupplier` - Returns FailedPrecondition error
7. `TestGRPCRelay_UnknownService` - Returns NotFound error

**Backend Error Handling:**
8. `TestGRPCRelay_BackendTimeout` - Context deadline returns DeadlineExceeded
9. `TestGRPCRelay_BackendUnavailable` - Connection failure returns error response
10. `TestGRPCRelay_Backend5xxError` - 5xx errors NOT wrapped (returns Unavailable)
11. `TestGRPCRelay_Backend4xxError` - 4xx errors wrapped and signed (valid relay)

**Interceptor & Transport Features:**
12. `TestGRPCInterceptor_MetadataForwarding` - HTTP headers forwarded to backend
13. `TestGRPCInterceptor_ErrorMapping` - Tests 200/400/404/500/502/503 mapping
14. `TestGRPCConcurrent_MultipleRequests` - 50 concurrent requests handled independently
15. `TestGRPCWeb_ContentTypeDetection` - gRPC content-type triggers backend selection
16. `TestGRPCRelay_UnknownMethod` - Unknown methods return Unimplemented
17. `TestGRPCRelay_ContextCancellation` - Cancelled contexts handled properly
18. `TestGRPCRelay_GzipCompression` - gRPC gzip compression works correctly

### Test Infrastructure

- Uses real gRPC client/server (net.Listen + grpc.NewServer)
- Manual gRPC stream creation for SendRelay method testing
- httptest.Server for backend simulation
- testutil package for deterministic test data
- Simple mocks (grpcMockPublisher, grpcMockSigner)

## Key Findings

### 1. Error Handling Behavior (Characterized)

**4xx Backend Errors (Client Errors):**
- ARE wrapped in signed RelayResponse
- Treated as valid relays (supplier compensated)
- Examples: 400 Bad Request, 404 Not Found

**5xx Backend Errors (Server Errors):**
- NOT wrapped or signed
- Return gRPC Unavailable status
- Supplier NOT compensated for infrastructure failures
- Examples: 500 Internal Server Error, 502 Bad Gateway, 503 Service Unavailable

This is correct behavior: 4xx = client made mistake (valid relay), 5xx = supplier infrastructure failed (no compensation).

### 2. Timeout Behavior (Characterized)

**gRPC Context Timeout:**
- Returns gRPC `DeadlineExceeded` error
- Handled at transport layer (not wrapped)
- Context cancellation works correctly

**Backend Timeout:**
- If backend responds slow but context OK: error response (500)
- If context times out: DeadlineExceeded gRPC error

### 3. Metadata Forwarding (Verified)

- POKTHTTPRequest headers forwarded to backend
- Config headers (from `ServiceConfig.Backends[].Headers`) added
- Config headers override request headers (correct precedence)

### 4. Concurrency (Verified)

- 50 concurrent requests handled independently
- No race conditions (passes -race)
- No flaky failures (5 consecutive shuffled runs pass)

## Deviations from Plan

None - plan executed exactly as written.

## Test Quality Metrics

- **Total lines:** 825 (exceeds 300 line requirement)
- **Test count:** 15 test cases + 6 subtests (TestGRPCInterceptor_ErrorMapping)
- **Race detector:** All tests pass with -race flag
- **Shuffle stability:** 5/5 consecutive runs pass with -shuffle=on
- **No time.Sleep:** Uses real gRPC communication, no artificial delays
- **No mocks for gRPC:** Uses real grpc.Server and grpc.NewClient

## Impact

### Coverage Improvement

- gRPC transport now has characterization tests
- CHAR-03 gap partially closed (gRPC coverage added)
- Relayer package test coverage increased

### Future Refactoring Safety

Safe to refactor:
- relay_grpc_service.go error handling
- grpc_interceptor.go panic recovery
- grpc_web.go content-type detection
- Backend selection logic

Not safe to change without updating tests:
- 4xx/5xx error wrapping behavior (documented as intentional)
- gRPC status code mappings
- Metadata forwarding precedence

## Next Steps

### Immediate (Phase 2 Continuation)

1. Execute plan 02-09: Cache package characterization tests
2. Execute plan 02-10: Session manager integration tests
3. Execute plan 02-11: Transaction client characterization tests

### Phase 2 Completion

After completing plans 02-09 through 02-11, re-verify Phase 2:
- Run full characterization test suite
- Measure updated coverage baseline
- Document any new gaps discovered

### Phase 3 (Future)

- Add tests for gRPC streaming relays (currently not supported)
- Add gRPC-Web browser client tests (manual verification needed)
- Consider property-based testing for gRPC protocol compliance

## Verification

```bash
# Tests pass with race detector
go test -tags test -race -v -run TestGRPC ./relayer/...
# Output: ok (1.996s)

# Tests pass with shuffle (5 consecutive runs)
for i in $(seq 1 5); do go test -tags test -race -shuffle=on -run TestGRPC ./relayer/... || exit 1; done
# Output: 5/5 runs passed

# Test file line count
wc -l relayer/relay_grpc_service_test.go
# Output: 825 lines
```

## Lessons Learned

### What Worked Well

1. **Real gRPC infrastructure** - Using actual gRPC server/client found real behavior
2. **Simplified mocks** - Nil relayPipeline avoided complex mock dependencies
3. **testutil usage** - Deterministic addresses from testutil worked perfectly
4. **Characterization approach** - Documented actual timeout behavior (not expected)

### What Could Be Improved

1. **Test organization** - Could group tests by category (error handling, concurrency, etc.)
2. **Helper functions** - Some test setup is duplicated (backend creation patterns)
3. **Documentation** - Could add more comments explaining gRPC-specific behavior

### Surprises

1. **Context timeout vs backend timeout** - Different error paths (DeadlineExceeded vs wrapped 500)
2. **No validation needed** - Most tests work fine with nil relayPipeline
3. **Fast test execution** - Real gRPC tests run in <2 seconds

## Related Documentation

- **Plan:** `.planning/phases/02-characterization-tests/02-08-PLAN.md`
- **Source:** `relayer/relay_grpc_service.go` (577 lines)
- **Interceptors:** `relayer/grpc_interceptor.go` (97 lines)
- **gRPC-Web:** `relayer/grpc_web.go` (79 lines)
- **Tests:** `relayer/relay_grpc_service_test.go` (825 lines)

## Commit

```
ccd6751 test(02-08): add gRPC transport characterization tests
```

---

*Summary created: 2026-02-03*
*Duration: 8 minutes*
*Gap closure: CHAR-03 (gRPC transport)*
