package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Closing miniredis in the middle of a test, to prove the code under test
// fails when Redis is gone, is a flake generator. The close frees the port,
// and package test binaries run CONCURRENTLY under `go test -p 4`, each
// starting its own miniredis on a kernel-assigned port — so another binary can
// bind the address this test just released. The call under test then reaches a
// FOREIGN Redis, succeeds, and the assertion passes or fails on whichever
// server answered. go-redis widens the window further by retrying a failed
// dial five times with backoff.
//
// Measured 2026-08-19: the level-2 race gate went red on
// TestSimVerify_DedupUnavailableFailsClosed with "got nil" — Verify admitted a
// relay because SetNX had landed on somebody else's miniredis. Proven by
// binding a second miniredis to the freed address, which reproduces it every
// time. Three more tests carried the same shape.
//
// The fix is miniredis.SetError, a pre-hook that errors every command while
// the connection stays up: deterministic, and a closer model of a degraded
// store than a vanished one. Closing the CLIENT works too where the test only
// needs the store to be unreachable.
//
// What makes a close dangerous is not the close: it is an ASSERTION that runs
// after it, because that assertion is what observes whichever server answered.
// So the check flags a close only when the enclosing function asserts later in
// the source. That spares every teardown shape without needing a list of them:
// a `defer`, a `t.Cleanup(func(){...})` literal, `t.Cleanup(mr.Close)` (a
// method value, not a call), a named `cleanup()` / `TearDownSuite()` harness
// method, and a close written as the last statement of a test.

// miniredisReceiver matches the names this repository gives a *miniredis.Miniredis.
var miniredisReceiver = regexp.MustCompile(`(?i)^(mr|mini_?redis|redis_?srv|redis_?server)$`)

// importsMiniredis reports whether the file imports the miniredis package, so
// the name heuristic below is only applied where it can mean anything.
func importsMiniredis(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		if strings.Contains(strings.Trim(imp.Path.Value, `"`), "alicebob/miniredis") {
			return true
		}
	}
	return false
}

// runsLaterRanges returns the source ranges of every DeferStmt and FuncLit. A
// close inside one of those does not run where it is written, so the
// assertions that FOLLOW it in the source still run before it.
func runsLaterRanges(fn ast.Node) [][2]token.Pos {
	var ranges [][2]token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.DeferStmt, *ast.FuncLit:
			ranges = append(ranges, [2]token.Pos{n.Pos(), n.End()})
		}
		return true
	})
	return ranges
}

// assertionPositions returns the position of every call that can fail a test:
// the testify entry points and t.Fatal/t.Error and friends.
func assertionPositions(body ast.Node) []token.Pos {
	failing := map[string]bool{
		"Fatal": true, "Fatalf": true, "Error": true, "Errorf": true,
		"FailNow": true, "Fail": true,
	}
	var out []token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			if id.Name == "require" || id.Name == "assert" {
				out = append(out, call.Pos())
				return true
			}
			if failing[sel.Sel.Name] {
				out = append(out, call.Pos())
			}
		}
		return true
	})
	return out
}

// assertsAfter reports whether fn contains an assertion positioned after pos.
func assertsAfter(fn ast.Node, pos token.Pos) bool {
	for _, a := range assertionPositions(fn) {
		if a > pos {
			return true
		}
	}
	return false
}

// miniredisClosedMidTest returns the positions where a miniredis is closed in
// the straight-line body of a test, where a later assertion can observe a
// different server on the freed port.
func miniredisClosedMidTest(f *ast.File, fset *token.FileSet) []string {
	if !importsMiniredis(f) {
		return nil
	}
	var hits []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		later := runsLaterRanges(fn.Body)
		runsLater := func(p token.Pos) bool {
			for _, r := range later {
				if p >= r[0] && p < r[1] {
					return true
				}
			}
			return false
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Close" {
				return true
			}
			var recv string
			switch x := sel.X.(type) {
			case *ast.Ident:
				recv = x.Name
			case *ast.SelectorExpr:
				recv = x.Sel.Name
			default:
				return true
			}
			if !miniredisReceiver.MatchString(recv) {
				return true
			}
			// Two ways a close cannot be observed: it is deferred to the end
			// of the test (defer, or a literal handed to t.Cleanup), or the
			// test never asserts again after it.
			if runsLater(call.Pos()) || !assertsAfter(fn.Body, call.Pos()) {
				return true
			}
			hits = append(hits, fmt.Sprintf("%s (closes %s)", fset.Position(call.Pos()), recv))
			return true
		})
	}
	return hits
}

// TestNoMidTestMiniredisClose fails when a test takes Redis away by freeing
// its port instead of breaking it in place.
func TestNoMidTestMiniredisClose(t *testing.T) {
	files, fset := goFiles(t, true)

	var violations []string
	for path, f := range files {
		for _, hit := range miniredisClosedMidTest(f, fset) {
			violations = append(violations, fmt.Sprintf("%s: %s", path, hit))
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("miniredis closed mid-test, freeing a port another test binary can bind "+
			"(use mr.SetError(...) or close the client instead):\n%s", joinLines(violations))
	}
}

// TestMiniredisCloseMatcherCatchesHostileShapes proves the matcher fires on the
// real shape and spares every form whose close cannot be observed.
func TestMiniredisCloseMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

func TestBadPlain(t *testing.T) {              // MUST match
	mr, _ := miniredis.Run()
	mr.Close()
	require.Error(t, doWork())
}

func TestBadThroughHarness(t *testing.T) {     // MUST match: selector receiver
	h := newHarness()
	h.miniRedis.Close()
	if doWork() == nil {
		t.Fatal("expected failure")
	}
}

func TestGoodDefer(t *testing.T) {             // must not: runs after the test
	mr, _ := miniredis.Run()
	defer mr.Close()
	require.Error(t, doWork())
}

func TestGoodCleanupLiteral(t *testing.T) {    // must not: inside a FuncLit
	mr, _ := miniredis.Run()
	t.Cleanup(func() { mr.Close() })
	require.Error(t, doWork())
}

func TestGoodCleanupMethodValue(t *testing.T) { // must not: a method value, not a call
	mr, _ := miniredis.Run()
	t.Cleanup(mr.Close)
	require.Error(t, doWork())
}

func TestGoodCloseIsLast(t *testing.T) {       // must not: nothing asserts after it
	mr, _ := miniredis.Run()
	require.NoError(t, doWork())
	mr.Close()
}

func (h *harness) cleanup() {                  // must not: teardown, no assertion
	h.miniRedis.Close()
}

func TestGoodOtherCloser(t *testing.T) {       // must not: not a miniredis name
	client := newClient()
	client.Close()
	require.Error(t, doWork())
}

type harness struct{ miniRedis *miniredis.Miniredis }

func newHarness() *harness                  { return nil }
func newClient() interface{ Close() error } { return nil }
func doWork() error                         { return nil }
`
	f, fset := parseSource(t, hostile)
	hits := miniredisClosedMidTest(f, fset)
	if len(hits) != 2 {
		t.Fatalf("matcher found %d violations, want exactly 2 (the plain and the harness close):\n%s",
			len(hits), joinLines(hits))
	}
	joined := strings.Join(hits, "\n")
	if !strings.Contains(joined, "closes mr)") || !strings.Contains(joined, "closes miniRedis)") {
		t.Fatalf("matcher flagged the wrong receivers:\n%s", joined)
	}
}

// TestMiniredisCloseMatcherIgnoresFilesWithoutTheImport proves the name
// heuristic cannot fire on a file that has no miniredis in it at all.
func TestMiniredisCloseMatcherIgnoresFilesWithoutTheImport(t *testing.T) {
	src := `package x

import "testing"

func TestX(t *testing.T) {
	mr := newThing()
	mr.Close()
}

func newThing() interface{ Close() error } { return nil }
`
	f, fset := parseSource(t, src)
	if hits := miniredisClosedMidTest(f, fset); len(hits) != 0 {
		t.Fatalf("matcher fired without the miniredis import:\n%s", joinLines(hits))
	}
}
