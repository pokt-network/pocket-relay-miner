# State: Pocket RelayMiner Quality Hardening

**Last Updated:** 2026-02-02

## Project Reference

**Core Value:** Test confidence — comprehensive coverage that enables safe refactoring and prevents regressions

**Current Focus:** Test Foundation (Phase 1)

**Context:** Quality hardening milestone for pocket-relay-miner addressing tech debt from 1-month rebuild. System is production-grade (1000+ RPS), handling real money on Pocket Network. Goal: Enable fearless refactoring via comprehensive test coverage (80%+ on critical paths).

## Current Position

**Phase:** 1 - Test Foundation

**Plan:** Not yet planned

**Status:** Pending

**Progress:**
```
[Phase 1: Test Foundation                                    ] 0%
```

**Next Steps:**
1. Run `/gsd:plan-phase 1` to create execution plan for test infrastructure
2. Establish race detection, linting, and vulnerability scanning in CI
3. Audit existing tests for flaky patterns before adding new coverage

## Performance Metrics

**Velocity:** Not yet tracked (milestone just started)

**Quality:**
- Tests passing: All existing tests (baseline)
- Race detection: Currently enabled only for miner package (needs expansion)
- Coverage: Unknown (needs baseline measurement)

**Blockers:** None

## Accumulated Context

### Decisions

| Decision | Rationale | Date |
|----------|-----------|------|
| 6-phase approach | Aligns with research guidance for incremental refactoring | 2026-02-02 |
| Test foundation first | Cannot safely refactor without understanding current behavior | 2026-02-02 |
| Standard depth (6 phases) | Balances thoroughness with manageable scope | 2026-02-02 |

### Key Findings

- **64+ time.Sleep() violations** in tests identified by research — causes flaky tests
- **Three large files** need refactoring: lifecycle_callback.go (1898 lines), session_lifecycle.go (1207 lines), proxy.go (1842 lines)
- **Current test coverage** unknown for relayer/ and cache/ packages (only miner/ has comprehensive tests)
- **Research confidence: HIGH** — Go stdlib testing patterns proven, miniredis actively maintained

### TODOs

- [ ] Measure baseline coverage for all packages
- [ ] Create flaky test reproduction script (run 100x, detect failures)
- [ ] Document current test execution time (before optimization)
- [ ] Identify which tests currently use time.Sleep() (grep audit)

### Blockers

None currently. External dependencies (WebSocket handshake spec, historical params protocol fix) are explicitly out of scope.

## Session Continuity

**For Next Session:**

This is the first session for this milestone. Roadmap and state initialized.

**Context to Preserve:**

- **Rule #1 from CLAUDE.md:** No flaky tests, no race conditions, no mock/fake tests — MANDATORY
- **Test Quality Standards:** Use miniredis for Redis (not mocks), all tests pass `go test -race`, deterministic data only
- **Performance Target:** 1000+ RPS per relayer replica must be maintained through refactoring
- **Coverage Goal:** 80%+ enforcement on critical paths (miner/, relayer/, cache/)

**Open Questions:**

- Should pre-commit hooks be mandatory or optional? (Decision deferred to Phase 6)
- Does testcontainers add value for blockchain integration tests or are mocks sufficient? (Research gap noted, decision in Phase 6)
- Which SMST invariants should be tested with property-based testing (Rapid)? (Decision deferred to Phase 2)

---

*State tracking initialized: 2026-02-02*
