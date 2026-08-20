package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"testing"
)

// CLAUDE.md concurrency rule: no unbounded bare goroutines. The accepted
// shapes are a worker pool submission (pond's pool.Go — a method call, not a
// go statement) and `go logging.RecoverGoRoutine(...)(ctx)`, which caps
// nothing but at least converts a panic into a counted, logged recovery.
//
// bareGoStatements returns "FuncName" entries for go statements in a file
// that are NOT the RecoverGoRoutine shape.
func bareGoStatements(f *ast.File, fset *token.FileSet) []string {
	var hits []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			gostmt, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			if isRecoverGoRoutineCall(gostmt.Call) {
				return true
			}
			hits = append(hits, fn.Name.Name)
			return true
		})
	}
	_ = fset
	return hits
}

// isRecoverGoRoutineCall matches `logging.RecoverGoRoutine(...)(ctx)`: the go
// statement's call whose Fun is itself a call to a selector or identifier
// named RecoverGoRoutine (the identifier form covers the logging package's
// own internal use).
func isRecoverGoRoutineCall(call *ast.CallExpr) bool {
	inner, ok := call.Fun.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := inner.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == "RecoverGoRoutine"
	case *ast.Ident:
		return fun.Name == "RecoverGoRoutine"
	}
	return false
}

// bareGoroutineAllowlist freezes the bare go statements that existed when
// this check was installed (2026-08-19), keyed "file: FuncName" with the
// number of bare go statements in that function. The fix is a queued
// campaign (migrate to pond pools / RecoverGoRoutine); this list only stops
// NEW ones from landing. Do not add entries — wrap the goroutine instead.
var bareGoroutineAllowlist = map[string]int{
	"cache/block_publisher.go: Start":                         1,
	"cache/block_subscriber.go: Start":                        1,
	"cache/block_subscriber.go: Subscribe":                    1,
	"cache/block_subscriber_adapter.go: Start":                1,
	"cache/orchestrator.go: warmupCaches":                     1,
	"cache/pubsub.go: SubscribeToInvalidations":               1,
	"cache/redis_block_client_adapter.go: Start":              1,
	"cache/redis_block_client_adapter.go: Subscribe":          1,
	"cache/supplier_cache.go: WarmupFromRedis":                1,
	"cache/supplier_params.go: Start":                         1,
	"client/block_subscriber.go: Start":                       1,
	"client/block_subscriber.go: Subscribe":                   1,
	"cmd/cmd_relayer.go: runHARelayer":                        1,
	"cmd/cmd_relayer.go: startHealthServer":                   2,
	"cmd/relay/common.go: runLoadTest":                        1,
	"cmd/relay/http.go: runHTTPLoadTest":                      1,
	"keys/manager.go: Start":                                  1,
	"keys/supplier_keys_file.go: WatchForChanges":             1,
	"leader/global_leader.go: Start":                          1,
	"leader/redis_health.go: Start":                           1,
	"miner/balance_monitor.go: Start":                         1,
	"miner/block_health_monitor.go: Start":                    1,
	"miner/lifecycle_callback.go: OnSessionsNeedClaim":        1,
	"miner/lifecycle_callback.go: OnSessionsNeedProof":        1,
	"miner/metrics.go: StartWorkerPoolMetricsTicker":          1,
	"miner/session_lifecycle.go: Start":                       1,
	"miner/session_lifecycle.go: runCoalescingBlockLoop":      1,
	"miner/supplier_claimer.go: Start":                        3,
	"miner/supplier_manager.go: Start":                        1,
	"miner/supplier_manager.go: addSupplierWithData":          1,
	"miner/supplier_manager.go: handleKeyChange":              1,
	"miner/supplier_manager.go: onSupplierReleased":           1,
	"miner/supplier_manager.go: startReconcilerBlockLoop":     1,
	"miner/supplier_manager.go: startWithDistributedClaiming": 1,
	"observability/runtime_metrics.go: Start":                 1,
	"observability/server.go: startMetricsServer":             2,
	"observability/server.go: startPprofServer":               2,
	"query/test_helpers.go: setupMockQueryServer":             1,
	"relayer/healthcheck.go: Start":                           1,
	"relayer/proxy.go: Start":                                 2,
	"relayer/relay_meter.go: Start":                           2,
	"relayer/session_monitor.go: notifyBridges":               1,
	"transport/redis/consumer.go: Consume":                    3,
	"tx/test_helpers.go: setupMockGRPCServer":                 1,
}

// TestNoNewBareGoroutines fails on any bare `go` statement not frozen in the
// allowlist, and on allowlist entries that no longer match (so the list
// shrinks as the campaign progresses instead of rotting).
func TestNoNewBareGoroutines(t *testing.T) {
	files, fset := goFiles(t, false)

	found := map[string]int{}
	for path, f := range files {
		for _, fn := range bareGoStatements(f, fset) {
			found[path+": "+fn]++
		}
	}

	var violations []string
	for key, n := range found {
		frozen := bareGoroutineAllowlist[key]
		if n > frozen {
			violations = append(violations, fmt.Sprintf("%s (%d bare, %d frozen)", key, n, frozen))
		}
	}
	var stale []string
	for key, frozen := range bareGoroutineAllowlist {
		if found[key] < frozen {
			stale = append(stale, fmt.Sprintf("%s (frozen %d, found %d — shrink the allowlist)", key, frozen, found[key]))
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("new bare goroutines (use a pond pool or logging.RecoverGoRoutine):\n%s", joinLines(violations))
	}
	if len(stale) > 0 {
		t.Errorf("stale allowlist entries:\n%s", joinLines(stale))
	}
}

// TestBareGoroutineMatcherCatchesHostileShapes proves the matcher can fail
// and accepts exactly the RecoverGoRoutine shape.
func TestBareGoroutineMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
import "logging"
func a() { go func() {}() }                                     // MUST match
func b() { go work() }                                          // MUST match
func c(ctx C) { go logging.RecoverGoRoutine(l, "n", fn)(ctx) }  // must not
func d() { pool.Go(func() {}) }                                 // must not (method call, no go stmt)
func e(ctx C) { go RecoverGoRoutine(l, "n", fn)(ctx) }          // must not (in-package form)
func work() {}
type C int
`
	f, fset := parseSource(t, hostile)
	hits := bareGoStatements(f, fset)
	if len(hits) != 2 {
		t.Fatalf("matcher found %d bare go statements in the synthetic source, want 2 (a, b): %v", len(hits), hits)
	}
}

// fatalCalls returns "file: FuncName" entries for logger .Fatal() chains in
// production code outside func main / the cobra command bootstrap.
func fatalCalls(f *ast.File) []string {
	var hits []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Fatal" {
				return true
			}
			hits = append(hits, fn.Name.Name)
			return true
		})
	}
	return hits
}

// TestNoLoggerFatalOutsideMain enforces "never logger.Fatal in goroutines":
// zerolog's Fatal calls os.Exit, skipping every deferred cleanup. The only
// place that may exit the process directly is main's bootstrap, where nothing
// is running yet.
func TestNoLoggerFatalOutsideMain(t *testing.T) {
	files, _ := goFiles(t, false)

	var violations []string
	for path, f := range files {
		for _, fn := range fatalCalls(f) {
			if fn == "main" {
				continue
			}
			violations = append(violations, path+": "+fn)
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf(".Fatal() outside main (os.Exit skips deferred cleanup — propagate an error instead):\n%s",
			joinLines(violations))
	}
}
