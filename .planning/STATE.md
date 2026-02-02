# State: Pocket RelayMiner Quality Hardening

**Last Updated:** 2026-02-02

## Project Reference

**Core Value:** Test confidence — comprehensive coverage that enables safe refactoring and prevents regressions

**Current Focus:** Test Foundation (Phase 1)

**Context:** Quality hardening milestone for pocket-relay-miner addressing tech debt from 1-month rebuild. System is production-grade (1000+ RPS), handling real money on Pocket Network. Goal: Enable fearless refactoring via comprehensive test coverage (80%+ on critical paths).

## Current Position

**Phase:** 1 of 6 (Test Foundation)

**Plan:** 01 of 04 in Phase 1

**Status:** In progress

**Last activity:** 2026-02-02 - Completed 01-01-PLAN.md (Linting configuration)

**Progress:**
```
[Phase 1: Test Foundation ██████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░] 12.5%
```

**Next Steps:**
1. Execute 01-02-PLAN.md (CI/CD integration)
2. Execute 01-03-PLAN.md (Vulnerability scanning)
3. Execute 01-04-PLAN.md (Test quality audit)

## Performance Metrics

**Velocity:** Not yet tracked (milestone just started)

**Quality:**
- Tests passing: All existing tests (baseline)
- Linting: ✅ golangci-lint configured (262 violations inventoried for Phase 2/3)
- Race detection: Not yet enabled (01-02)
- Coverage: Unknown (needs baseline measurement in 01-03)

**Blockers:** None

## Accumulated Context

### Decisions

| Decision | Rationale | Date |
|----------|-----------|------|
| 6-phase approach | Aligns with research guidance for incremental refactoring | 2026-02-02 |
| Test foundation first | Cannot safely refactor without understanding current behavior | 2026-02-02 |
| Standard depth (6 phases) | Balances thoroughness with manageable scope | 2026-02-02 |
| Lenient complexity thresholds in Phase 1 | funlen:600/220, gocognit:250, gocyclo:80 calibrated to existing code; tighten in Phase 4 | 2026-02-02 |
| golangci-lint v2 format | Uses linters.settings (nested) not linters-settings (top-level) | 2026-02-02 |
| Defer 262 violations to Phase 2/3 | Automatic fixes only (41); manual fixes (262) require non-trivial changes beyond Phase 1 scope | 2026-02-02 |

### Key Findings

- **64+ time.Sleep() violations** in tests identified by research — causes flaky tests
- **Three large files** need refactoring: lifecycle_callback.go (1898 lines), session_lifecycle.go (1207 lines), proxy.go (1842 lines)
- **Current test coverage** unknown for relayer/ and cache/ packages (only miner/ has comprehensive tests)
- **Research confidence: HIGH** — Go stdlib testing patterns proven, miniredis actively maintained

### TODOs

- [x] Create golangci-lint configuration - ✅ .golangci.yml created
- [ ] Integrate linter into CI (01-02)
- [ ] Fix 262 remaining lint violations (Phase 2/3: 220 errcheck, 42 gosec)
- [ ] Add vulnerability scanning (01-03)
- [ ] Measure baseline test coverage (01-04)

### Blockers

None currently. External dependencies (WebSocket handshake spec, historical params protocol fix) are explicitly out of scope.

## Session Continuity

**Last session:** 2026-02-02 20:35:53 UTC

**Stopped at:** Completed 01-01-PLAN.md

**Resume file:** None (phase in progress, continue with 01-02-PLAN.md)

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
