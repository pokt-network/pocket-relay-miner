package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"testing"
)

// panicCalls returns the positions of panic(...) call expressions in a file.
// It is an AST matcher, not a grep: the word "panic" in a comment or a string
// is not *ast.Ident{Name: "panic"} in call position.
func panicCalls(f *ast.File, fset *token.FileSet) []string {
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "panic" {
			hits = append(hits, fset.Position(call.Pos()).String())
		}
		return true
	})
	return hits
}

// TestNoPanicInProductionCode enforces CLAUDE.md: "Never use panic() in
// production code paths". Zero exemptions today (measured 2026-08-19: the
// only three panic calls in the tree are in _test.go files, which this walk
// does not visit).
func TestNoPanicInProductionCode(t *testing.T) {
	files, fset := goFiles(t, false)

	var violations []string
	for path, f := range files {
		for _, pos := range panicCalls(f, fset) {
			violations = append(violations, fmt.Sprintf("%s: %s", path, pos))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("panic() in production code (CLAUDE.md forbids it — return an error instead):\n%s",
			joinLines(violations))
	}
}

// TestPanicMatcherCatchesHostileShapes proves the matcher can fail: it must
// see a panic in call position and must NOT match the word in comments,
// strings, or a local function that merely contains the letters.
func TestPanicMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
// panic("this comment must not match")
func a() { panic("boom") }
func b() { s := "panic(not a call)"; _ = s }
func mypanic() {}
func c() { mypanic() }
func d() { defer panic("deferred is still a call") }
`
	f, fset := parseSource(t, hostile)
	hits := panicCalls(f, fset)
	if len(hits) != 2 {
		t.Fatalf("matcher found %d panic calls in the synthetic source, want exactly 2 (a and d): %v", len(hits), hits)
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += "  " + l + "\n"
	}
	return out
}
