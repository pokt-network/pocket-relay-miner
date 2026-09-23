package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// An open WebSocket asks the batch-queue gate for every client frame and every
// backend message, and the bridge learns the gate from its constructor. The
// handler that builds it has no test that runs the whole relayer, and a test of
// the bridge hands it the gate itself -- so a handler that passed nil would leave
// every such test green and every open connection publishing past the cap, which
// is the 2026-09-23 excess (1568 MiB against 512). The wiring is frozen by reading
// it.

// wsQueueGateViolations reports what is wrong with how f builds WebSocket bridges.
func wsQueueGateViolations(f *ast.File, fset *token.FileSet) []string {
	var problems []string
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !callsName(call, "NewWebSocketBridge") {
			return true
		}
		calls++
		if len(call.Args) == 0 {
			problems = append(problems, "NewWebSocketBridge called without arguments at "+fset.Position(call.Pos()).String())
			return true
		}
		sel, ok := call.Args[len(call.Args)-1].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "queueFull" {
			problems = append(problems, "NewWebSocketBridge must be given the proxy's queueFull gate as its last argument at "+
				fset.Position(call.Pos()).String())
		}
		return true
	})
	if calls != 1 {
		problems = append(problems, "the relayer must build WebSocket bridges in exactly one place")
	}
	return problems
}

func TestTheWebSocketHandlerGivesTheBridgeTheQueueGate(t *testing.T) {
	files, fset := goFiles(t, false)

	const path = "relayer/websocket.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}
	for _, p := range wsQueueGateViolations(f, fset) {
		t.Errorf("%s: %s", path, p)
	}
}

// TestWSQueueGateViolationsReadsTheWiring proves the rule reads what the bridge
// is given, and not merely that it is built.
func TestWSQueueGateViolationsReadsTheWiring(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"wired", `package x
func a() { NewWebSocketBridge(conn, url, p.queueFull) }`, 0},
		{"nil gate", `package x
func a() { NewWebSocketBridge(conn, url, nil) }`, 1},
		{"another gate", `package x
func a() { NewWebSocketBridge(conn, url, p.storeSaturated) }`, 1},
		{"not built", `package x
func a() {}`, 1},
		{"built twice", `package x
func a() { NewWebSocketBridge(conn, url, p.queueFull); NewWebSocketBridge(conn, url, p.queueFull) }`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fset := parseSource(t, tc.src)
			got := wsQueueGateViolations(f, fset)
			if len(got) != tc.want {
				t.Fatalf("want %d violations, got %d: %v", tc.want, len(got), got)
			}
		})
	}
}
