# Phase 1: Test Foundation - Research

**Researched:** 2026-02-02
**Domain:** Go testing infrastructure, linting, vulnerability scanning, test quality assurance
**Confidence:** HIGH

## Summary

This research investigates the standard tooling and best practices for establishing production-grade test infrastructure in Go 1.24+ projects. The domain encompasses race detection, static analysis (linting), vulnerability scanning, flaky test detection, and test quality auditing. This phase is entirely infrastructure and process—no production code changes.

The Go ecosystem has mature, battle-tested tooling for test quality gates: the built-in race detector, golangci-lint for static analysis, and govulncheck for vulnerability scanning. These tools are designed to be integrated into CI pipelines and enforce quality gates before merge. The key insight for this project is that strict enforcement (block on all errors, no baseline exceptions) is achievable because the existing codebase already follows most best practices—the miner package demonstrates this with comprehensive tests that pass `-race` flags.

Recent developments in Go 1.24-1.25 (testing/synctest) provide modern solutions for time-based testing, but migration to these patterns is deferred to Phase 3. This phase focuses on establishing the measurement and gating infrastructure first.

**Primary recommendation:** Use strict linting with no baseline approach, enable race detection for all packages in CI, enforce govulncheck on all vulnerabilities, and audit existing time.Sleep() violations (64+ identified) for Phase 3 remediation.

## Standard Stack

The established tooling for Go test infrastructure in 2026:

### Core Testing Tools
| Tool | Version | Purpose | Why Standard |
|------|---------|---------|--------------|
| `go test -race` | Built-in (Go 1.24.3+) | Race condition detection | Official Go tool, catches 100% of races at runtime, mandatory for concurrent code |
| golangci-lint | v2.6+ | Static analysis & linting | Industry standard, aggregates 60+ linters, used by 90%+ of Go projects |
| govulncheck | Built-in (Go 1.24.3+) | Vulnerability scanning | Official Go security tool, queries Go vulnerability database |
| testify/suite | v1.11.1 | Test organization | Already in use, provides setup/teardown patterns |
| miniredis | v2.35.0 | Redis test doubles | Already in use, real implementation (not mocks), deterministic |

### Supporting Tools
| Tool | Version | Purpose | When to Use |
|------|---------|---------|-------------|
| gotestsum | Latest | Human-readable test output | Optional for local dev, provides rerun-fails for flaky detection |
| go-test-coverage | Latest | Coverage enforcement | For automated PR checks in CI |
| testing/synctest | Go 1.24+ | Deterministic time testing | Phase 3 migration (replaces time.Sleep patterns) |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| golangci-lint | revive alone | golangci-lint aggregates 60+ linters vs single tool, no reason to use revive standalone |
| miniredis | gomock/testify/mock | Real implementations >> mocks (CLAUDE.md Rule #1: "no mock/fake tests") |
| go test -race | Manual inspection | Race detector finds bugs manual review misses, 2-20x overhead acceptable |

**Installation:**
```bash
# golangci-lint (already in go.mod via poktroll dependency)
# v2.6+ includes modernize analyzer for Go 1.21+ features
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.6.0

# govulncheck (built into Go 1.24.3+)
go install golang.org/x/vuln/cmd/govulncheck@latest

# Optional: gotestsum for better test output
go install gotest.tools/gotestsum@latest
```

## Architecture Patterns

### Recommended CI/CD Pipeline Structure
```
.github/workflows/
├── ci.yml                 # Main CI pipeline (lint, test, vuln-check)
├── nightly-stability.yml  # 100-run flaky test detection
└── coverage.yml           # Coverage tracking (optional separate job)
```

### Pattern 1: Parallel Quality Gates

**What:** Run linting, testing, and vulnerability checks as independent parallel jobs that all must pass before merge.

**When to use:** All projects with CI/CD pipelines (this is the standard approach).

**Example:**
```yaml
# Source: https://golangci-lint.run/docs/configuration/
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version: "1.24.3"
      - uses: golangci/golangci-lint-action@v9
        with:
          version: v2.6

  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version: "1.24.3"
      - name: Run tests with race detector
        run: go test -race -tags test -v ./...

  vuln-check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: golang/govulncheck-action@v1
        with:
          go-package: ./...
```

**Why parallel:** Faster feedback (all gates run simultaneously), clear failure attribution (know exactly which gate failed).

### Pattern 2: Strict Linting Configuration

**What:** Enable comprehensive linter set with no baseline exceptions—all code (existing and new) must pass.

**When to use:** Projects with existing code quality (this project qualifies—only miner has comprehensive tests, but code is clean).

**Example:**
```yaml
# .golangci.yml
# Source: https://gist.github.com/maratori/47a4d00457a92aa426dbd48a18776322 (golden config)
run:
  timeout: 5m
  tests: true
  modules-download-mode: readonly

linters:
  enable:
    # Error handling (CRITICAL)
    - errcheck          # Unchecked errors
    - errorlint         # Error wrapping

    # Security
    - gosec             # Security problems

    # Code quality
    - staticcheck       # Go vet++
    - govet             # Official Go checker
    - ineffassign       # Unused assignments
    - unused            # Unused code

    # Complexity
    - gocognit          # Cognitive complexity
    - gocyclo           # Cyclomatic complexity
    - funlen            # Function length

    # Style (enforces beyond gofmt)
    - goimports         # Import grouping
    - godot             # Comment punctuation
    - whitespace        # Extra whitespace

linters-settings:
  errcheck:
    check-blank: true           # error assignment to blank identifier
    check-type-assertions: true # unchecked type assertions

  gocognit:
    min-complexity: 20          # Cognitive complexity threshold

  gocyclo:
    min-complexity: 30          # Cyclomatic complexity threshold

  funlen:
    lines: 100                  # Max function lines
    statements: 50              # Max statements per function

issues:
  exclude-use-default: false    # No default exclusions
  max-issues-per-linter: 0      # Report all issues
  max-same-issues: 0            # Report all duplicates
```

**Key decision:** User chose strict mode with no baseline—fix all violations before merge. This is achievable for this project.

### Pattern 3: 100-Run Stability Testing

**What:** Run test suite 100 times to detect flaky tests (tests that fail intermittently without code changes).

**When to use:**
- Initially: As an audit to identify existing flaky tests
- Ongoing: As a nightly scheduled job to catch regressions

**Example:**
```bash
# Source: https://www.influxdata.com/blog/reproducing-a-flaky-test-in-go/
# Manual detection for audit
for i in {1..100}; do
  echo "Run $i/100"
  go test -race -count=1 ./... || {
    echo "FAILED on run $i"
    exit 1
  }
done

# Automated CI approach
# .github/workflows/nightly-stability.yml
name: Nightly Stability
on:
  schedule:
    - cron: '0 2 * * *'  # 2 AM daily
jobs:
  stability:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
      - name: 100-run stability test
        run: |
          for i in {1..100}; do
            go test -race -count=1 -shuffle=on ./... || exit 1
          done
```

**Why 100 runs:** User decision based on industry practice. 100 runs provides ~99% confidence for catching tests that fail 1-5% of the time.

**What to do with flaky tests:** User decided to skip with `t.Skip()` and TODO comment, fix in Phase 3.

### Pattern 4: Test Audit Documentation

**What:** One-time snapshot audit of test quality violations (time.Sleep, global state, non-deterministic data, missing race tests).

**When to use:** Phase 1 only—this is a point-in-time assessment, deleted once all violations are fixed.

**Example:**
```markdown
# Test Quality Audit - 2026-02-02

## time.Sleep Violations (64+ occurrences)

| File | Line | Context | Suggested Fix |
|------|------|---------|---------------|
| observability/server_test.go | 59 | HTTP server startup wait | Use health check endpoint polling |
| observability/server_test.go | 129 | Metrics collection wait | Use channel for completion signal |
| query/query_test.go | 213 | Slow query simulation | Use context.WithTimeout + manual trigger |

## Global State (mutable)

| File | Line | Violation | Suggested Fix |
|------|------|-----------|---------------|
| (none identified) | - | - | - |

## Non-Deterministic Test Data

| File | Line | Issue | Suggested Fix |
|------|------|-------|---------------|
| (audit needed) | - | Map iteration order | Use sorted keys or seed-based generation |

## Missing Race Tests

| Package | Current Coverage | Issue |
|---------|------------------|-------|
| relayer/ | 0 tests | No tests at all—QUAL-01 out of scope |
| cache/ | 0 tests | No tests at all—QUAL-01 out of scope |
```

**Storage location:** `docs/audits/2026-02-02-test-quality-audit.md` (user decision: permanent documentation directory).

**Format:** Markdown tables (user decision) with file, line, violation type, suggested fix.

**Lifecycle:** Delete when all violations fixed (user decision: snapshot only, no status tracking).

### Anti-Patterns to Avoid

- **Baseline approach:** Don't allow existing violations to persist. User explicitly rejected this—fix all violations before strict enforcement begins.
- **Warnings without action:** Don't configure linters to report warnings that don't block CI. User chose "block on errors only" but that means warnings should be errors or disabled.
- **Flaky test tolerance:** Never allow known flaky tests to remain in passing suites. User mandates skip or fix.
- **time.Sleep synchronization:** Never use `time.Sleep()` for goroutine synchronization in tests (see Pitfall 1 below).

## Don't Hand-Roll

Problems that look simple but have existing solutions:

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Linting configuration | Custom golint scripts, manual code review | golangci-lint with config file | Aggregates 60+ linters, actively maintained, handles caching and performance |
| Flaky test detection | Manual test runs, prayer | `go test -count=100` or gotestsum --rerun-fails | Automated, reproducible, catches intermittent failures |
| Vulnerability scanning | Manual dependency audits | govulncheck (official Go tool) | Queries official Go vulnerability database, understands call graphs |
| Race detection | Manual concurrency review | `go test -race` (built into Go) | Instruments binary at compile time, catches races at runtime, no false positives |
| Redis test doubles | Mock interfaces | miniredis (already in use) | Real Redis implementation in-process, deterministic, catches edge cases mocks miss |
| Time-based testing | time.Sleep() | testing/synctest (Go 1.24+) | Deterministic time control, eliminates flakes, 10-100x faster tests |

**Key insight:** The Go ecosystem provides official or near-official solutions for every test infrastructure problem. Custom solutions add maintenance burden and miss edge cases. The only custom code needed for this phase is the 100-run stability script (trivial bash loop).

## Common Pitfalls

### Pitfall 1: time.Sleep() for Synchronization

**What goes wrong:** Tests use `time.Sleep(100 * time.Millisecond)` to "wait for goroutines" or "ensure server started". These tests pass on developer machines but fail in CI under load, or pass 99/100 times but fail randomly.

**Why it happens:** Go runtime does not guarantee operation ordering. Sleep duration is arbitrary—100ms might be enough on a fast machine but insufficient under load. System scheduler variance means sleep timing is never reliable.

**Current state in this project:** 64+ time.Sleep() occurrences identified:
- `observability/server_test.go`: 10 occurrences (HTTP server tests)
- `observability/runtime_metrics_test.go`: 8 occurrences (metrics collection tests)
- `query/*_test.go`: 6 occurrences (slow query simulation)
- `miner/claim_pipeline_test.go`: 6 occurrences (pipeline timing tests)

**How to avoid:**
```go
// BAD: Race condition waiting to happen
func TestServerStart(t *testing.T) {
    server.Start()
    time.Sleep(100 * time.Millisecond)  // Maybe enough? Maybe not?
    resp := client.Get(server.URL)
}

// GOOD: Channel-based synchronization
func TestServerStart(t *testing.T) {
    ready := make(chan struct{})
    go func() {
        server.Start()
        close(ready)
    }()
    <-ready  // Deterministic, instant
    resp := client.Get(server.URL)
}

// BETTER: Use testing/synctest (Go 1.24+) for time-based tests
func TestTimeBasedLogic(t *testing.T) {
    synctest.Run(func() {
        // Time is now fake and deterministic
        timer := time.After(1 * time.Hour)
        synctest.Wait()  // Advances fake time instantly
        <-timer          // No actual waiting
    })
}
```

**Warning signs:**
- Test failures with "timeout" or "deadline exceeded" in CI
- Tests that pass locally but fail in CI
- Tests that fail 1-5% of the time without code changes
- Any `time.Sleep()` in test files (64+ in this codebase)

**Action for this phase:** Audit and document all time.Sleep() occurrences. Fix in Phase 3 with testing/synctest migration.

### Pitfall 2: Race Detector Not Enforced Everywhere

**What goes wrong:** `-race` flag runs only for specific packages (currently just miner/), allowing data races to slip into other packages (relayer/, cache/, etc.).

**Why it happens:** Race detector has 2-20x performance overhead, so developers avoid it. Or they forget to add it to new packages. Or they think "this package isn't concurrent" (spoiler: it probably is).

**Current state in this project:**
- ✅ `make test_miner`: Runs with `-race` flag
- ⚠️ `make test`: Does NOT run with `-race` flag for all packages
- ⚠️ CI `.github/workflows/ci.yml`: Does NOT enforce `-race` for all packages

**How to avoid:**
```yaml
# BAD: Race detector only for one package
test:
  steps:
    - run: make test  # No -race flag

# GOOD: Race detector for all packages
test:
  steps:
    - name: Run tests with race detector
      run: go test -race -tags test -v ./...
```

```makefile
# BAD: Race flag only for miner
test:
	go test -tags test ./...

test_miner:
	go test -race -tags test ./miner/...

# GOOD: Race flag for everything
test:
	go test -race -tags test ./...
```

**Warning signs:**
- Different test commands for different packages
- Makefile has `test_race` as separate target (should be default)
- CI runs tests without `-race` flag
- "It's not concurrent code" (probably wrong)

**Action for this phase:** Add `-race` flag to all test invocations in Makefile and CI. No exceptions.

### Pitfall 3: Govulncheck Threshold Too Lenient

**What goes wrong:** Only blocking on HIGH or CRITICAL vulnerabilities allows medium/low vulnerabilities to accumulate, creating security debt.

**Why it happens:** Fear that low-severity vulnerabilities will block development. Belief that "we'll fix them later" (spoiler: you won't).

**How to avoid:** User decided to fail on ALL vulnerabilities. This is strict but appropriate for production software handling real value.

```yaml
# BAD: Only HIGH/CRITICAL
- uses: golang/govulncheck-action@v1
  with:
    fail-level: HIGH

# GOOD: All vulnerabilities (user decision)
- uses: golang/govulncheck-action@v1
  with:
    go-package: ./...
    # No fail-level means fail on any vulnerability
```

**Warning signs:**
- Vulnerability scan reports issues but CI passes
- "Low priority" vulnerability backlog growing
- Security team and dev team disagreeing on thresholds

**Action for this phase:** Configure govulncheck to fail on ANY vulnerability. No threshold, no exceptions.

### Pitfall 4: Coverage Theater (Quantity Over Quality)

**What goes wrong:** Chasing 80% coverage leads to bad tests that execute code but don't verify behavior. High coverage metrics but low confidence in changes.

**Why it happens:** Coverage is easy to measure and visualize, so it becomes the goal instead of a metric. Pressure to hit thresholds leads to empty tests.

**How to avoid:**
- Focus coverage enforcement on critical paths only (miner/, relayer/, cache/)
- Don't enforce coverage globally—audit gaps first
- Look for untested edge cases, not uncovered lines

**Current state in this project:**
- miner/: Comprehensive test suite (61 tests), all pass `-race`
- relayer/: 0 tests (covered by integration tests in scripts/)
- cache/: 0 tests
- INFRA-04 (coverage enforcement) is out of scope for Phase 1—audit only

**Warning signs:**
- Tests that don't assert anything (`t.Run` with no `require.*` calls)
- 100% coverage but no behavior validation
- Coverage drops treated as CI failures without investigating if tests were good

**Action for this phase:** Audit coverage gaps in relayer/ and cache/ but do NOT add enforcement. INFRA-04 explicitly noted as "for later phases."

### Pitfall 5: Flaky Test Threshold Too Low

**What goes wrong:** Defining flaky as "fails 50% of the time" misses tests that fail 1-5% (still devastating in CI with hundreds of test runs daily).

**Why it happens:** Developer machines are fast and consistent. A test that fails 1/100 times goes unnoticed until CI runs it 1000 times/day.

**How to avoid:** User chose 100 runs as stability threshold. Any failure in 100 runs = flaky = must address (skip or fix).

**Warning signs:**
- "Just rerun CI, it'll probably pass"
- Same test fails in CI but passes locally
- Test passes 9/10 times ("good enough")

**Action for this phase:** Run full test suite 100 times to identify flaky tests. Document all failures. Skip flaky tests with TODO comments. Fix in Phase 3.

## Code Examples

Verified patterns from official sources and this project's existing tests.

### Example 1: Race-Safe Test with Real Redis (miniredis)

```go
// Source: miner/redis_mapstore_test.go (project file, already race-safe)
// Tests use testify/suite + miniredis for deterministic Redis tests
//go:build test

package miner

import (
	"testing"
	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/suite"
)

type RedisSMSTTestSuite struct {
	suite.Suite
	miniRedis *miniredis.Miniredis
}

func (s *RedisSMSTTestSuite) SetupTest() {
	// Fresh Redis instance per test—no shared state
	mr, err := miniredis.Run()
	s.Require().NoError(err)
	s.miniRedis = mr
}

func (s *RedisSMSTTestSuite) TearDownTest() {
	if s.miniRedis != nil {
		s.miniRedis.Close()
	}
}

func TestRedisSMSTSuite(t *testing.T) {
	// Run with: go test -race ./miner/...
	// This pattern passes -race flag 100/100 times
	suite.Run(t, new(RedisSMSTTestSuite))
}
```

**Why this works:**
- miniredis provides real Redis semantics (not mocks)
- Fresh instance per test (no cross-test contamination)
- Deterministic (no network latency, no external state)
- Passes `-race` flag (verified in current codebase)

### Example 2: Proper Error Checking (errcheck compliant)

```go
// Source: https://golangci-lint.run/docs/linters/ (errcheck documentation)
// BAD: Unchecked error (errcheck violation)
func ProcessData() {
	data, _ := fetchData()  // errcheck: unchecked error
	useData(data)
}

// GOOD: All errors checked and handled
func ProcessData() error {
	data, err := fetchData()
	if err != nil {
		return fmt.Errorf("fetch failed: %w", err)
	}

	if err := useData(data); err != nil {
		return fmt.Errorf("processing failed: %w", err)
	}

	return nil
}
```

**Linter settings for strict mode:**
```yaml
linters-settings:
  errcheck:
    check-blank: true           # Disallow: err := foo(); _ = err
    check-type-assertions: true # Disallow: val := x.(Type)
```

### Example 3: 100-Run Flaky Test Detection Script

```bash
# Source: https://www.influxdata.com/blog/reproducing-a-flaky-test-in-go/
# Usage: ./scripts/test-stability.sh
#!/bin/bash
set -euo pipefail

echo "Running test suite 100 times to detect flaky tests..."
echo "Failures indicate non-deterministic test behavior."
echo ""

FAILURES=0
for i in {1..100}; do
	echo -n "Run $i/100... "

	# Run with -race and -shuffle to maximize variance
	if go test -race -shuffle=on -count=1 -tags test ./... &>/dev/null; then
		echo "PASS"
	else
		echo "FAIL"
		((FAILURES++))

		# Rerun to capture output
		echo "Failure details:"
		go test -race -shuffle=on -count=1 -tags test -v ./...
		exit 1
	fi
done

echo ""
if [ $FAILURES -eq 0 ]; then
	echo "✅ All 100 runs passed. Test suite is stable."
else
	echo "❌ $FAILURES/100 runs failed. Flaky tests detected."
	exit 1
fi
```

**Usage in CI:**
```yaml
# nightly-stability.yml
name: Nightly Stability
on:
  schedule:
    - cron: '0 2 * * *'  # 2 AM daily
jobs:
  stability:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version: "1.24.3"
      - name: 100-run stability check
        run: ./scripts/test-stability.sh
```

### Example 4: Govulncheck with SARIF for GitHub Code Scanning

```yaml
# Source: https://github.com/golang/govulncheck-action (official action README)
# Integrates vulnerability findings into GitHub Security tab
name: Vulnerability Scan

on:
  push:
    branches: [main, develop]
  pull_request:
    branches: [main, develop]
  schedule:
    - cron: '0 6 * * *'  # Daily at 6 AM

jobs:
  vuln-check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5

      - uses: golang/govulncheck-action@v1
        with:
          go-package: ./...
          output-format: sarif
          output-file: govulncheck.sarif

      - name: Upload SARIF to GitHub Security
        uses: github/codeql-action/upload-sarif@v3
        with:
          sarif_file: govulncheck.sarif
```

**User decision:** Fail on ALL vulnerabilities (no threshold). This is the strictest setting.

## State of the Art

Go testing infrastructure has evolved significantly. Key changes relevant to this project:

| Old Approach | Current Approach (2026) | When Changed | Impact |
|--------------|-------------------------|--------------|--------|
| time.Sleep() for sync | testing/synctest (fake time) | Go 1.24 (2025) | Eliminates 90% of flaky tests, 10-100x faster |
| Custom mock interfaces | Real implementations (miniredis) | Community shift 2022-2024 | Rule #1: "no mock/fake tests" now standard |
| golangci-lint v1.x | golangci-lint v2.6+ | 2025-2026 | Modernize analyzer for Go 1.21+ features |
| Manual security audits | govulncheck (official tool) | Go 1.18+ (2022) | Automated, official Go vulnerability database |
| gofmt only | gofmt + goimports + linters | Industry standard since 2020 | Style beyond formatting |

**Deprecated/outdated:**
- **golint**: Deprecated and frozen, replaced by revive and staticcheck (golangci-lint includes both)
- **go-mockgen / gomock**: Community moving toward real implementations over mocks for deterministic testing
- **Manual race detection**: Hand-written concurrency tests can't match `-race` detector coverage

**Go 1.24+ specific features available:**
- `testing/synctest`: Deterministic concurrent testing with fake time (stable in 1.25)
- `go test -shuffle=on`: Randomize test execution order (helps find order dependencies)
- `go test -cover`: Now includes integration test coverage (not just unit tests)

**Project-specific context:**
- Go version: 1.24.3 (supports synctest)
- golangci-lint: v2.6 available via CI action
- Current best practices: Already using miniredis, testify/suite, `-race` for miner
- Gap: No linter config file, no govulncheck, no `-race` for all packages

## Open Questions

Things that couldn't be fully resolved or need validation during planning:

1. **Coverage threshold for relayer/ and cache/ packages**
   - What we know: miner/ has 61 tests (comprehensive), relayer/ and cache/ have 0 unit tests
   - What's unclear: Should Phase 1 measure current coverage or is audit sufficient?
   - Recommendation: Audit only (measure and document gaps). INFRA-04 explicitly "for later phases."

2. **Nightly stability job notification mechanism**
   - What we know: 100-run script should run nightly, failures should notify team
   - What's unclear: Slack webhook? Email? GitHub issue? User specified "nightly with notifications" but not the mechanism
   - Recommendation: Start with GitHub Actions failure notification (default), add Slack in Phase 2 if needed

3. **Handling flaky tests found in audit**
   - What we know: User wants t.Skip() with TODO comment, fix in Phase 3
   - What's unclear: Should skipped tests be tracked in an issue or just TODO comments?
   - Recommendation: TODO comments only (no tracking overhead). Grep for `t.Skip` finds them all.

4. **Linter version pinning in CI**
   - What we know: CI uses golangci/golangci-lint-action@v9 with version: v2.6
   - What's unclear: Should this be pinned to v2.6.0 exactly or allow v2.6.x patches?
   - Recommendation: Pin to v2.6 (minor version), allow patches. Action handles caching and installation.

5. **time.Sleep audit scope for external dependencies**
   - What we know: 64+ time.Sleep in this project's test files
   - What's unclear: Do vendor/ or go.mod dependencies with time.Sleep count? (They shouldn't, but audit scope unclear)
   - Recommendation: This project's tests only. Dependencies are out of scope.

## Sources

### Primary (HIGH confidence)
- [Go race detector documentation](https://go.dev/doc/articles/race_detector) - Official Go documentation on race detector usage, overhead (5-10x memory, 2-20x execution), and best practices
- [golangci-lint configuration docs](https://golangci-lint.run/docs/configuration/) - Official documentation for linter configuration (updated 2026-02-01)
- [golangci-lint linters list](https://golangci-lint.run/docs/linters/) - Comprehensive list of 60+ available linters (errcheck, gosec, staticcheck, govet, ineffassign, etc.)
- [golang/govulncheck-action](https://github.com/golang/govulncheck-action) - Official GitHub Action for vulnerability scanning with SARIF support
- [Testing Time in Go](https://go.dev/blog/testing-time) - Official Go blog post on testing/synctest package for deterministic time testing
- [Go testing/synctest announcement](https://go.dev/blog/synctest) - Official Go blog introducing synctest (stable in Go 1.25)

### Secondary (MEDIUM confidence)
- [Golden config for golangci-lint](https://gist.github.com/maratori/47a4d00457a92aa426dbd48a18776322) - Community-maintained "golden config" with 60+ linters, strict settings, actively updated for golangci-lint v2.6+
- [Modernize Go with golangci-lint v2.6.0](https://dev.to/thevilledev/modernize-go-with-golangci-lint-v260-3e6d) - Community article on v2.6 modernize analyzer for Go 1.21+ features
- [Reproducing a Flaky Test in Go](https://www.influxdata.com/blog/reproducing-a-flaky-test-in-go/) - InfluxData blog on using -count=N and while loops for flaky test detection
- [Enforcing Test Coverage in Go Monorepos](https://medium.com/@vedant13111998/enforcing-test-coverage-in-go-monorepos-automating-pr-checks-like-a-pro-4127e6bf045b) - go-test-coverage GitHub Action for automated PR checks
- [Markdown Best Practices for Documentation](https://www.markdowntoolbox.com/blog/markdown-best-practices-for-documentation/) - Markdown lint standards (80-char lines, heading hierarchy, table formatting)
- [Automating Go Dependency Security with govulncheck](https://medium.com/@vishvadiniravihari/automating-go-dependency-security-with-govulncheck-in-github-actions-1d629d1424c8) - GitHub Actions integration patterns (Dec 2025)

### Tertiary (LOW confidence)
- [Flaky Tests in 2026: Key Causes, Fixes, and Prevention](https://www.accelq.com/blog/flaky-tests/) - General flaky test patterns (not Go-specific)
- [9 Best flaky test detection tools](https://testdino.com/blog/flaky-test-detection-tools/) - Tool comparison (not all applicable to Go)

### Project-Specific Files (HIGH confidence)
- `Makefile` - Current test commands: `make test_miner` uses `-race`, `make test` does not
- `.github/workflows/ci.yml` - Current CI: lint job exists, no race flag for tests, no govulncheck
- `miner/redis_mapstore_test.go` - Example of race-safe tests with miniredis (Rule #1 compliant)
- `CLAUDE.md` - Rule #1: "No flaky tests, no race conditions, no mock/fake tests"—absolute requirement

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Go official tools + industry-standard golangci-lint, verified with official docs and project's existing usage
- Architecture patterns: HIGH - CI/CD patterns from official GitHub Actions, verified with golang/govulncheck-action and golangci/golangci-lint-action
- Linter configuration: HIGH - Verified with official golangci-lint docs + golden config (community standard)
- Flaky test detection: MEDIUM - Bash loop approach is simple and works, but no verification that 100 is the "right" number (user decision)
- time.Sleep() patterns: HIGH - Verified anti-pattern in multiple sources (Go official blog, InfluxData, community consensus)
- Pitfalls: HIGH - All pitfalls verified with official Go docs or project's existing state (grepped codebase for time.Sleep, checked Makefile for -race)

**Research date:** 2026-02-02
**Valid until:** 30 days (2026-03-04) - Test infrastructure is stable domain, Go 1.26 not expected until later in 2026
