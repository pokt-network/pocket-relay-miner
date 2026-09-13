package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The relayer's mined-relay publisher is ALWAYS the batching one (Jorge,
// 2026-09-12): only its interval is configurable. runHARelayer builds a whole
// process and has no test, so the construction is frozen by reading it -- the
// same idiom as TestPublisherFlushIsDeferredAfterTheRedisClient.
//
// What goes red is a construction that sits under a condition. That is exactly
// the shape the knob had from 3519a00 until this rule: the one-relay-per-round-trip
// publisher by default and `if ms > 0 { publisher = NewBatchingPublisher(...) }`,
// which made batching an operator decision that could be left off.

// callsOf returns the positions of every call to a function or method named name
// inside a function body, split by whether an if, switch, select, for or range
// statement stands between the call and its enclosing function declaration.
func callsOf(f *ast.File, name string) (unconditional, conditional []token.Pos) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var stack []ast.Node
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			// Inspect calls back with nil after a node's children when the callback
			// returned true, which it always does here: that is the pop.
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok || !callsName(call, name) {
				return true
			}
			guarded := false
			for _, ancestor := range stack[:len(stack)-1] {
				switch ancestor.(type) {
				case *ast.IfStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.ForStmt, *ast.RangeStmt:
					guarded = true
				}
			}
			if guarded {
				conditional = append(conditional, call.Pos())
			} else {
				unconditional = append(unconditional, call.Pos())
			}
			return true
		})
	}
	return unconditional, conditional
}

// callsName reports whether call invokes an identifier or a selector named name.
func callsName(call *ast.CallExpr, name string) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == name
	case *ast.SelectorExpr:
		return fun.Sel != nil && fun.Sel.Name == name
	}
	return false
}

func TestTheRelayerAlwaysBuildsTheBatchingPublisher(t *testing.T) {
	files, fset := goFiles(t, false)

	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}

	unconditional, conditional := callsOf(f, "NewBatchingPublisher")
	for _, pos := range conditional {
		t.Errorf("%s builds the batching publisher under a condition at %s.\n"+
			"  The batch is always on: only batch_publish_interval_ms is configurable, and\n"+
			"  a guard around the construction is how it became something that could be\n"+
			"  left off.", path, fset.Position(pos))
	}
	if len(unconditional) != 1 {
		t.Errorf("%s builds the batching publisher unconditionally %d times, want exactly 1",
			path, len(unconditional))
	}

	u, c := callsOf(f, "NewStreamsPublisher")
	if len(u)+len(c) > 0 {
		t.Errorf("%s constructs the one-relay-per-round-trip publisher again; the always-on\n"+
			"  batch replaced it", path)
	}
}

// TestCallsOfSeparatesGuardedConstructions proves the matcher reads the guard and
// not merely the name.
func TestCallsOfSeparatesGuardedConstructions(t *testing.T) {
	src := `package x

func a() {
	p := x.NewBatchingPublisher()
	if ms > 0 {
		p = x.NewBatchingPublisher()
	}
	switch {
	case true:
		x.NewBatchingPublisher()
	}
	for i := 0; i < 1; i++ {
		NewBatchingPublisher()
	}
	defer func() { _ = x.NewBatchingPublisher() }()
	x.NewBatchingPublisherish()
	_ = p
}
`
	f, _ := parseSource(t, src)
	unconditional, conditional := callsOf(f, "NewBatchingPublisher")
	if len(unconditional) != 2 {
		t.Fatalf("want 2 unconditional constructions (the assignment and the deferred closure), got %d", len(unconditional))
	}
	if len(conditional) != 3 {
		t.Fatalf("want 3 guarded constructions (if, switch, for), got %d", len(conditional))
	}
}
