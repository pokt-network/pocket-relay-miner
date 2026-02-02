# Phase 1: Test Foundation - Context

**Gathered:** 2026-02-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Establish baseline test infrastructure and quality gates before any code changes. This phase sets up race detection, linting, vulnerability scanning, and audits existing test quality. No production code changes — infrastructure and process only.

</domain>

<decisions>
## Implementation Decisions

### Linter Configuration
- Strict everywhere — all code (existing and new) must pass linting
- No baseline approach — fix existing violations before merge
- errcheck strict — all unchecked errors are failures, no exceptions
- Enable all recommended linters: staticcheck, errcheck, govet, gosec, ineffassign, unused, etc.
- Enforce style beyond gofmt: goimports + style linters for import grouping and naming conventions

### CI Integration
- Block on errors only — warnings reported but don't fail the build
- govulncheck fails on ALL known vulnerabilities (not just HIGH/CRITICAL)
- Race detection runs on every PR (all tests with -race flag)
- Lint, test, and vuln-check run in parallel jobs for faster feedback

### Flaky Test Handling
- 100 runs defines stability threshold
- Any failure (1 in 100) = flaky = must be addressed
- Policy for existing flaky tests: Skip with t.Skip() and TODO comment, fix in Phase 3
- 100-run stability script runs in CI as scheduled (nightly) job with notifications

### Audit Documentation
- Audit report lives in `docs/audits/` as permanent documentation
- Format: Markdown tables (file, line, violation type, suggested fix)
- Snapshot only — no status tracking, delete when all violations fixed
- Categorize ALL patterns: time.Sleep, global state, non-deterministic data, missing race tests

### Claude's Discretion
- Specific linter version selection
- Exact CI job configuration syntax (GitHub Actions, etc.)
- Nightly schedule timing
- Audit report filename and structure within the defined format

</decisions>

<specifics>
## Specific Ideas

- CLAUDE.md Rule #1 is absolute: "No flaky tests, no race conditions, no mock/fake tests"
- 64+ time.Sleep() violations already identified in research — audit should confirm and categorize
- Only miner/ package currently has comprehensive tests — audit should note coverage gaps in relayer/ and cache/

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 01-test-foundation*
*Context gathered: 2026-02-02*
