package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The drain added to ProxyServer.Close on 2026-09-12 rests on two decisions
// that live at CALL SITES, not inside the code they protect, so every test the
// drain ships with stays green when either one is undone:
//
//   - the WebSocket handler joins the counter BEFORE running the bridge. The
//     drain's own tests build the registry by hand -- they call trackBridge
//     themselves -- so removing the handler's call leaves them passing while
//     production stops waiting for the transport that publishes last.
//   - cmd_relayer passes the real 30s shutdown budget to proxy.Close. Every
//     drain test builds its own context, and no test in the tree mentions
//     shutdownCtx at all, so `proxy.Close(context.Background())` loses the
//     entire bound with nothing going red. That is not hypothetical: the line
//     the drain replaced was `_ = shutdownCtx // Used for graceful shutdown
//     timing`, a comment claiming a wiring that did not exist.
//
// Both are the shape a cleanup deletes, which is why they are frozen here
// rather than described in a comment next to them.

// guardedCallPositions returns the positions of calls to the method `name` that
// sit inside an if-statement's CONDITION -- calls whose result is tested. A
// bare `p.trackBridge(bridge)` joins the counter and then runs the bridge
// anyway, which is the misuse of sync.WaitGroup the guard exists to prevent, so
// "called" is not the property worth freezing; "checked" is.
func guardedCallPositions(f *ast.File, name string) []token.Pos {
	var out []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Cond == nil {
			return true
		}
		ast.Inspect(ifs.Cond, func(c ast.Node) bool {
			call, ok := c.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == name {
				out = append(out, call.Pos())
			}
			return true
		})
		return true
	})
	return out
}

// receiverCalls returns every call of the form recv.method(...) where recv is a
// plain identifier, in source order.
func receiverCalls(f *ast.File, recv, method string) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != method {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
			out = append(out, call)
		}
		return true
	})
	return out
}

// isFreshBackgroundCtx reports whether e is a context.Background() or
// context.TODO() built on the spot. A context VARIABLE is fine and is the
// point: it carries whatever deadline its builder gave it.
func isFreshBackgroundCtx(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "context" {
		return false
	}
	return sel.Sel.Name == "Background" || sel.Sel.Name == "TODO"
}

func TestTheWebSocketHandlerJoinsTheDrainBeforeRunningTheBridge(t *testing.T) {
	files, _ := goFiles(t, false)
	const path = "relayer/websocket.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}

	runs := receiverCalls(f, "bridge", "Run")
	if len(runs) == 0 {
		t.Fatalf("%s no longer calls bridge.Run(): this rule is pointed at the wrong file or the\n"+
			"  variable was renamed. Re-point it rather than deleting it -- the invariant it\n"+
			"  freezes is that the drain can wait for every running bridge.", path)
	}

	guards := guardedCallPositions(f, "trackBridge")
	if len(guards) == 0 {
		t.Fatalf("%s runs a bridge without a CHECKED trackBridge call.\n"+
			"  A bridge that is running and not counted is one ProxyServer.Close cannot wait\n"+
			"  for, and WebSocket is the transport that keeps publishing longest -- the caller\n"+
			"  closes the publisher and the Redis client underneath it.\n"+
			"  The drain's own tests do NOT catch this: they populate the registry by hand.", path)
	}

	first := guards[0]
	for _, g := range guards {
		if g < first {
			first = g
		}
	}
	for _, r := range runs {
		if first > r.Pos() {
			t.Errorf("%s calls bridge.Run() before joining the drain.\n"+
				"  The Add must happen first, and with no I/O in between: an Add that lands\n"+
				"  after the drain's Wait already reached zero is documented misuse of\n"+
				"  sync.WaitGroup, and it panics inside the shutdown path.", path)
		}
	}
}

func TestTheProxyShutdownIsGivenItsBudget(t *testing.T) {
	files, _ := goFiles(t, false)
	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}

	calls := receiverCalls(f, "proxy", "Close")
	if len(calls) == 0 {
		t.Fatalf("%s no longer calls proxy.Close: without it nothing drains, and the relays in\n"+
			"  flight are answered to clients and never written.", path)
	}
	for _, c := range calls {
		if len(c.Args) != 1 {
			t.Errorf("%s calls proxy.Close with %d arguments; it takes the shutdown budget.",
				path, len(c.Args))
			continue
		}
		if isFreshBackgroundCtx(c.Args[0]) {
			t.Errorf("%s passes a fresh context to proxy.Close.\n"+
				"  That context has no deadline, so the drain has no bound: a bridge that\n"+
				"  ignores the close signal is capped only by wsFirstFrameWait, two minutes,\n"+
				"  four times the budget this shutdown is supposed to have. Pass shutdownCtx.\n"+
				"  No test observes this -- none of them mentions shutdownCtx.", path)
		}
	}
}

// TestDrainWiringMatchersCatchHostileShapes feeds the matchers sources the tree
// does not contain, in both directions.
func TestDrainWiringMatchersCatchHostileShapes(t *testing.T) {
	guarded := `package x

func h() {
	if !p.trackBridge(b) {
		return
	}
	bridge.Run()
}
`
	unguarded := `package x

func h() {
	p.trackBridge(b)
	bridge.Run()
}
`
	mentioned := `package x

// trackBridge is only named here
var s = "if !p.trackBridge(b) {"
`
	f, _ := parseSource(t, guarded)
	if len(guardedCallPositions(f, "trackBridge")) != 1 {
		t.Fatal("matcher missed a trackBridge tested in an if condition")
	}
	if len(receiverCalls(f, "bridge", "Run")) != 1 {
		t.Fatal("matcher missed bridge.Run()")
	}

	f, _ = parseSource(t, unguarded)
	if len(guardedCallPositions(f, "trackBridge")) != 0 {
		t.Fatal("matcher accepted a trackBridge whose result is discarded")
	}

	f, _ = parseSource(t, mentioned)
	if len(guardedCallPositions(f, "trackBridge")) != 0 {
		t.Fatal("matcher fired on a mention in a comment and a string")
	}

	// Ordering: the same two calls, the wrong way round.
	reversed := `package x

func h() {
	bridge.Run()
	if !p.trackBridge(b) {
		return
	}
}
`
	f, _ = parseSource(t, reversed)
	g, r := guardedCallPositions(f, "trackBridge"), receiverCalls(f, "bridge", "Run")
	if len(g) != 1 || len(r) != 1 {
		t.Fatal("matcher lost one of the two calls it must order")
	}
	if g[0] < r[0].Pos() {
		t.Fatal("matcher reported the guard before a Run that precedes it")
	}

	ctxSrc := `package x

import "context"

func m() {
	_ = proxy.Close(shutdownCtx)
	_ = proxy.Close(context.Background())
	_ = proxy.Close(context.TODO())
	_ = other.Close(context.Background())
}
`
	f, _ = parseSource(t, ctxSrc)
	calls := receiverCalls(f, "proxy", "Close")
	if len(calls) != 3 {
		t.Fatalf("matcher found %d proxy.Close calls, want 3 (it must ignore other.Close)", len(calls))
	}
	if isFreshBackgroundCtx(calls[0].Args[0]) {
		t.Fatal("matcher rejected a context VARIABLE, which is the shape being required")
	}
	if !isFreshBackgroundCtx(calls[1].Args[0]) || !isFreshBackgroundCtx(calls[2].Args[0]) {
		t.Fatal("matcher missed a context built on the spot")
	}
}
