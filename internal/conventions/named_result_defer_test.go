package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"testing"
)

// A function with a NAMED error result runs its defers AFTER the return value
// is set, so a defer that assigns to that name overwrites what the function
// returned. Measured 2026-08-19 in cmd/cmd_miner.go: two cleanup defers did
// `err = X.Close()`, so the miner exited 0 even when the leader controller had
// failed and the error was explicitly propagated — every error return after
// the first such defer was discarded. The fix is a local variable
// (`if closeErr := X.Close(); closeErr != nil`).
//
// Deliberately NOT flagged: a defer that assigns the result on purpose, which
// in this repo is only the panic-recovery idiom `if r := recover(); r != nil {
// err = ... }`. That one is the reason the result is named at all.

// namedErrorResults returns the names of a function's named error results.
func namedErrorResults(fn *ast.FuncDecl) []string {
	var names []string
	if fn.Type.Results == nil {
		return nil
	}
	for _, field := range fn.Type.Results.List {
		id, ok := field.Type.(*ast.Ident)
		if !ok || id.Name != "error" {
			continue
		}
		for _, n := range field.Names {
			if n.Name != "" && n.Name != "_" {
				names = append(names, n.Name)
			}
		}
	}
	return names
}

// deferClobbersResult reports assignments to one of `names` inside a defer,
// skipping any defer whose body calls recover().
func deferClobbersResult(fn *ast.FuncDecl, names []string, fset *token.FileSet) []string {
	if len(names) == 0 || fn.Body == nil {
		return nil
	}
	named := map[string]bool{}
	for _, n := range names {
		named[n] = true
	}
	var hits []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		recovers := false
		ast.Inspect(d, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recover" {
					recovers = true
				}
			}
			return true
		})
		if recovers {
			return true
		}
		ast.Inspect(d, func(inner ast.Node) bool {
			as, ok := inner.(*ast.AssignStmt)
			if !ok || as.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && named[id.Name] {
					hits = append(hits, fmt.Sprintf("%s (assigns %q)", fset.Position(as.Pos()), id.Name))
				}
			}
			return true
		})
		return true
	})
	return hits
}

// TestNoDeferAssignsTheNamedErrorResult fails when a cleanup defer overwrites
// the function's named error result.
func TestNoDeferAssignsTheNamedErrorResult(t *testing.T) {
	files, fset := goFiles(t, false)

	var violations []string
	for path, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			for _, hit := range deferClobbersResult(fn, namedErrorResults(fn), fset) {
				violations = append(violations, fmt.Sprintf("%s: %s %s", path, fn.Name.Name, hit))
			}
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("a defer overwrites the function's named error result (use a local closeErr):\n%s",
			joinLines(violations))
	}
}

// TestNamedResultMatcherCatchesHostileShapes proves the matcher fires on the
// real shape and spares the recover idiom and local variables.
func TestNamedResultMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
func bad() (err error) {                       // MUST match
	defer func() { if err = closeIt(); err != nil { log(err) } }()
	return nil
}
func good() (err error) {                      // must not: local variable
	defer func() { if closeErr := closeIt(); closeErr != nil { log(closeErr) } }()
	return nil
}
func recovers() (err error) {                  // must not: the recover idiom
	defer func() { if r := recover(); r != nil { err = wrap(r) } }()
	return nil
}
func unnamed() error {                         // must not: result not named
	defer func() { _ = closeIt() }()
	return nil
}
func closeIt() error { return nil }
func log(...any) {}
func wrap(any) error { return nil }
`
	f, fset := parseSource(t, hostile)
	var got []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if hits := deferClobbersResult(fn, namedErrorResults(fn), fset); len(hits) > 0 {
			got = append(got, fn.Name.Name)
		}
	}
	if len(got) != 1 || got[0] != "bad" {
		t.Fatalf("matcher flagged %v, want exactly [bad]", got)
	}
}
