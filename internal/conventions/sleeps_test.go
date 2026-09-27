package conventions

import (
	"fmt"
	"go/ast"
	"sort"
	"testing"
)

// Rule #1 (CLAUDE.md): no time.Sleep for synchronization in tests. The
// existing sleeps are frozen per file (2026-08-19); the fix is a queued
// campaign (Go 1.26's testing/synctest makes most of them mechanical). This
// check only stops NEW ones from landing and forces the list to shrink as
// files are cleaned. Do not raise a number — remove the sleep instead.
var testSleepAllowlist = map[string]int{
	"client/block_subscriber_integration_test.go": 17,
	"cmd/relay/metrics_test.go":                   2,
	"keys/watch_helper_test.go":                   1,
	"miner/redis_smst_manager_test.go":            3,
	"miner/supplier_manager_race_test.go":         2,
	"observability/runtime_metrics_test.go":       8,
	"observability/server_test.go":                9,
	"pool/auto_recovery_event_test.go":            3,
	"pool/circuit_breaker_test.go":                3,
	"query/application_query_test.go":             1,
	"query/params_query_test.go":                  2,
	"query/proof_query_test.go":                   1,
	"query/query_test.go":                         1,
	"query/service_query_test.go":                 1,
	"query/session_query_test.go":                 1,
	"query/supplier_query_test.go":                1,
	"relayer/healthcheck_autorecovery_test.go":    1,
	"relayer/relay_grpc_publish_test.go":          1,
	"relayer/websocket_writedeadline_test.go":     1,
	"transport/redis/consumer_lifecycle_test.go":  2,
	"tx/tx_client_test.go":                        1,
}

// timeSleepCalls counts time.Sleep call expressions in a file.
func timeSleepCalls(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sleep" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" {
			n++
		}
		return true
	})
	return n
}

// TestNoNewSleepsInTests fails on any _test.go file whose time.Sleep count
// exceeds its frozen value, and on frozen entries that shrank (update the
// list downward so progress is pinned).
func TestNoNewSleepsInTests(t *testing.T) {
	files, _ := goFiles(t, true)

	found := map[string]int{}
	for path, f := range files {
		if n := timeSleepCalls(f); n > 0 {
			found[path] = n
		}
	}

	var violations, stale []string
	for path, n := range found {
		if frozen := testSleepAllowlist[path]; n > frozen {
			violations = append(violations, fmt.Sprintf("%s (%d sleeps, %d frozen)", path, n, frozen))
		}
	}
	for path, frozen := range testSleepAllowlist {
		if found[path] < frozen {
			stale = append(stale, fmt.Sprintf("%s (frozen %d, found %d — shrink the allowlist)", path, frozen, found[path]))
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("new time.Sleep in tests (Rule #1 — synchronize on state, or use testing/synctest):\n%s", joinLines(violations))
	}
	if len(stale) > 0 {
		t.Errorf("stale sleep allowlist entries:\n%s", joinLines(stale))
	}
}

// TestSleepMatcherCatchesHostileShapes proves the counter counts calls, not
// mentions.
func TestSleepMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
import "time"
// time.Sleep in a comment must not count
func a() { time.Sleep(time.Second) }
func b() { s := "time.Sleep"; _ = s }
func c() { myTime.Sleep(1) } // different receiver ident named time? no: myTime
var myTime T
type T int
func (T) Sleep(int) {}
`
	f, _ := parseSource(t, hostile)
	if got := timeSleepCalls(f); got != 1 {
		t.Fatalf("sleep counter got %d, want 1", got)
	}
}
