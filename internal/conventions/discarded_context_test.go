package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"testing"
)

// `_, cancel := context.WithCancel(ctx)` keeps the cancel func and throws the
// CONTEXT away. Nothing downstream can ever observe the cancellation, so the
// cancel call is a no-op dressed as a shutdown step -- and a reader auditing
// shutdown sees a contract that is not there.
//
// Measured 2026-08-19 in cache/session_cache.go: Start did exactly this and
// Close dutifully called the cancel, next to a WaitGroup that was waited on
// without a single Add. The cache runs no goroutines at all; the whole
// lifecycle was theatre. Its five sibling caches keep the context and use it,
// which is why the odd one out was invisible.
//
// Not flagged: discarding the CANCEL (`ctx, _ := context.WithCancel(...)`) is a
// different mistake -- a real leak, which `go vet` already reports as lostcancel
// -- and discarding both, which is dead code the compiler complains about.

// discardsContextKeepsCancel reports assignments that drop the context returned
// by a context.With* constructor while binding its cancel func.
func discardsContextKeepsCancel(f *ast.File, fset *token.FileSet) []string {
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 2 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "context" {
			return true
		}
		switch sel.Sel.Name {
		case "WithCancel", "WithTimeout", "WithDeadline", "WithCancelCause":
		default:
			return true
		}
		// First result discarded, second one bound to something real.
		ctxIdent, ok := as.Lhs[0].(*ast.Ident)
		if !ok || ctxIdent.Name != "_" {
			return true
		}
		if cancelIdent, ok := as.Lhs[1].(*ast.Ident); ok && cancelIdent.Name == "_" {
			return true // both discarded: not this defect
		}
		hits = append(hits, fmt.Sprintf("%s (context.%s)", fset.Position(as.Pos()), sel.Sel.Name))
		return true
	})
	return hits
}

// TestNoDiscardedContextWithKeptCancel fails when a cancel func is kept for a
// context nobody holds.
func TestNoDiscardedContextWithKeptCancel(t *testing.T) {
	var violations []string
	for _, testFiles := range []bool{false, true} {
		files, fset := goFiles(t, testFiles)
		for path, f := range files {
			for _, hit := range discardsContextKeepsCancel(f, fset) {
				violations = append(violations, fmt.Sprintf("%s: %s", path, hit))
			}
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("a cancel func is kept for a context that was thrown away, so cancelling it does nothing:\n%s",
			joinLines(violations))
	}
}

// TestDiscardedContextMatcherCatchesHostileShapes proves the matcher fires on
// the real shape and spares the legitimate ones.
func TestDiscardedContextMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x

import "context"

type c struct{ cancelFn context.CancelFunc }

func bad(ctx context.Context) {                    // MUST match
	var s c
	_, s.cancelFn = context.WithCancel(ctx)
}

func badLocal(ctx context.Context) {               // MUST match
	_, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
}

func good(ctx context.Context) {                   // must not: context is kept
	c2, cancel := context.WithCancel(ctx)
	defer cancel()
	_ = c2
}

func goodBothDropped(ctx context.Context) {        // must not: different mistake
	_, _ = context.WithCancel(ctx)
}

func goodOtherPackage(ctx context.Context) {       // must not: not context.With*
	_, cancel := other.WithCancel(ctx)
	defer cancel()
}
`
	f, fset := parseSource(t, hostile)
	hits := discardsContextKeepsCancel(f, fset)
	if len(hits) != 2 {
		t.Fatalf("matcher found %d violations, want exactly 2:\n%s", len(hits), joinLines(hits))
	}
}
