package conventions

import (
	"go/ast"
	"sort"
	"testing"
)

// syncMapUses returns the count of `sync.Map` type references in a file. The
// project standard is xsync.Map (typed, lock-free reads, no per-access type
// assertions); the last six sync.Map fields were migrated 2026-08-19.
func syncMapUses(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Map" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "sync" {
			n++
		}
		return true
	})
	return n
}

// TestNoSyncMapInProduction fails on any sync.Map in production code.
func TestNoSyncMapInProduction(t *testing.T) {
	files, _ := goFiles(t, false)

	var violations []string
	for path, f := range files {
		if n := syncMapUses(f); n > 0 {
			violations = append(violations, path)
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("sync.Map in production code (use xsync.Map — typed and lock-free):\n%s", joinLines(violations))
	}
}

// TestSyncMapMatcherCatchesHostileShapes proves the matcher sees type uses
// and ignores near-misses.
func TestSyncMapMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
import ("sync"; xsync "github.com/puzpuzpuz/xsync/v4")
type a struct{ m sync.Map }        // MUST match
type b struct{ m *xsync.Map[string, int] } // must not
var c sync.Mutex                   // must not (Mutex, not Map)
`
	f, _ := parseSource(t, hostile)
	if got := syncMapUses(f); got != 1 {
		t.Fatalf("sync.Map matcher got %d, want 1", got)
	}
}
