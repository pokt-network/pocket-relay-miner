# Phase 3: Test Quality Cleanup - Context

**Gathered:** 2026-02-03
**Status:** Ready for planning

<domain>
## Phase Boundary

Eliminate flaky test anti-patterns across the codebase: replace all 66 time.Sleep() violations with proper synchronization, fix all 4 known race conditions, and resolve all 262 lint violations (220 errcheck + 42 gosec). This phase cleans up test quality and code hygiene before structural refactoring in Phase 4.

Not in scope: table-driven test conversion (deferred), new test coverage (Phase 2 handled), structural refactoring (Phase 4), or tightening lint thresholds (Phase 4 after file sizes shrink).

</domain>

<decisions>
## Implementation Decisions

### Sleep Replacement Strategy
- Replace all 66 time.Sleep() violations in one pass — no partial state, clean sweep
- Default replacement: `require.Eventually` with 5s timeout and 10ms poll interval
- Create a `testutil.WaitFor(t, func() bool {...})` helper wrapping require.Eventually with standard 5s/10ms defaults for consistency and reduced boilerplate
- Use channels/select only where require.Eventually doesn't fit (e.g., goroutine signaling with specific values)

### Race Condition Fixes
- Fix all 4 known races in this phase: UpdateSessionRelayCount, runtime metrics collector, tx client mock, and stability-testing race
- UpdateSessionRelayCount fix: use `atomic.Int64` fields instead of raw int fields — zero contention, no mutex overhead
- Other races: Claude determines approach per case, preferring atomics where possible
- Validation: 1000/1000 stability runs required (matches Phase 3 success criteria)
- CI integration: 100 iterations with -race as PR gate, 1000 iterations in nightly workflow

### Lint Violation Cleanup
- Fix all 262 violations — both errcheck (220) and gosec (42) categories
- Deferred Close() pattern: log on error — `defer func() { if err := x.Close(); err != nil { logger.Warn()... } }()`
- No `//nolint` directives — fix or restructure all gosec violations, no exceptions
- Keep current lenient lint thresholds (funlen:600, gocognit:250) until Phase 4 refactoring reduces file sizes

### Claude's Discretion
- Specific sync primitive per sleep replacement when require.Eventually doesn't fit
- Race fix approach for non-UpdateSessionRelayCount races (atomics vs mutex vs restructure)
- Order of operations within the phase (sleeps first vs races first vs lint first)
- Grouping of lint fixes by package vs by violation type

</decisions>

<specifics>
## Specific Ideas

- testutil.WaitFor helper should be the go-to for all sleep replacements — consistent API across all test files
- 1000-run validation is the gold standard — no shortcuts on stability proof
- "Log on error" for deferred Close() aligns with the project's structured logging philosophy (zerolog)
- PR CI gate at 100 runs with -race provides fast feedback; nightly at 1000 runs catches rare races

</specifics>

<deferred>
## Deferred Ideas

- Table-driven test adoption (30% code reduction target) — originally in Phase 3 success criteria but not selected for discussion; researcher/planner should assess scope
- Tightening golangci-lint thresholds — explicitly deferred to Phase 4 after structural refactoring
- Additional test coverage improvements (cache orchestrator, warmer, supplier_cache) — Phase 6 or separate effort

</deferred>

---

*Phase: 03-test-quality-cleanup*
*Context gathered: 2026-02-03*
