package conventions

import (
	"fmt"
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// A test that starts a WebSocketBridge must JOIN its Run goroutine before the
// test ends. Closing it is not joining: Close "only SIGNALS" -- its own words,
// relayer/websocket.go:1704-1706 -- and Run's deferred release does the work
// afterwards, on its own goroutine.
//
// Measured 2026-09-20 (item 390). Nine tests started a bridge with
// `go bridge.Run()` and a cleanup that only closed it, so nine Run goroutines
// outlived their tests. That is not idle: Run serves frames, serving one calls
// the meter, and the meter increments a PROCESS-GLOBAL counter
// (relay_meter.go:431) under a service ID twelve test files share. A straggler
// lands its increment inside the next test's window, and
// TestAWebSocketChargesEachBackendMessageAndNotTheFrame -- which asserts
// `checksBefore+1` on exactly that series -- failed about once in twenty-five
// repetitions.
//
// The price of the NEXT unjoined site is what makes this a guard and not a
// review note: it is a flake of about one in twenty-five, which costs fifty
// repetitions to see and an afternoon to attribute.
//
// The allowed shapes are runBridge (relayer/ws_test_server_test.go), which
// joins at cleanup, and the three tests below that join their own channel
// inside the test body because the join IS what they assert.
var bridgeRunAllowlist = map[string]bool{
	"relayer/storage_gate_test.go: TestWebSocketALiveBridgeIsCutWhenTheStoreCloses":                  true,
	"relayer/websocket_lifecycle_test.go: TestBridgeClosesABackendDialThatLandsAfterTheBridgeClosed": true,
	"relayer/websocket_lifecycle_test.go: TestBridgeTearsDownWhenTheGatewayPathPanics":               true,
	// Joined by the PRODUCTION drain, which is what this test exists to prove:
	// it asserts runReturned after p.Close returns (proxy_drain_test.go:227).
	"relayer/proxy_drain_test.go: TestCloseWaitsForALiveBridge": true,
	"relayer/ws_test_server_test.go: runBridge":                 true,
}

// TestNoTestStartsABridgeItDoesNotJoin fails on a `go` statement in a test that
// reaches a .Run() call and is not one of the frozen shapes, and on frozen
// entries that no longer match so the list cannot rot.
func TestNoTestStartsABridgeItDoesNotJoin(t *testing.T) {
	files, _ := goFiles(t, true)

	found := map[string]bool{}
	for path, f := range files {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := path + ": " + fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				gos, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				// Any .Run() reached from inside this `go`, whether called
				// directly or from the func literal it launches.
				ast.Inspect(gos, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
						found[key] = true
					}
					return true
				})
				return true
			})
		}
	}

	var violations []string
	for key := range found {
		if !bridgeRunAllowlist[key] {
			violations = append(violations, key)
		}
	}
	var stale []string
	for key := range bridgeRunAllowlist {
		if !found[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(violations)
	sort.Strings(stale)

	if len(violations) > 0 {
		t.Errorf("a test starts a goroutine running something it never joins:\n  %s\n\n"+
			"Use runBridge, or wait on a channel the goroutine closes. Close() does not "+
			"join -- it signals, and Run keeps serving frames after the test returns "+
			"(item 390).", strings.Join(violations, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("allowlist entries that no longer match -- remove them:\n  %s",
			fmt.Sprint(strings.Join(stale, "\n  ")))
	}
}
