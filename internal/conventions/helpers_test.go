// Package conventions is a test-only package that mechanically enforces the
// repository's written conventions (CLAUDE.md "Code Standards" and the
// KeyBuilder STRONG RULE). It contains no production code on purpose: the
// checks run wherever `go test ./...` runs — the level-2 gates and CI pick
// them up with no wiring.
//
// Patterns (borrowed from a sibling repo's conventions suite):
//   - walks parse the tree fresh on every run and FAIL BELOW A FLOOR of files,
//     so "I found nothing" can never be produced by "I looked nowhere";
//   - each matcher is a pure function with a self-test feeding it hostile
//     synthetic sources the tree does not contain;
//   - exemptions are literal lists with a written reason, so adding one is a
//     deliberate edit somebody reviews, never a judgement call inside a run.
package conventions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot returns the repository root (two levels above this package) and
// fails if it does not look like the module root — the test moved, not the repo.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod — did this package move?", root)
	}
	return root
}

// skipDirs are directories the walks never enter. tilt/ holds a separate Go
// module (tilt/backend-server) plus deploy manifests; scripts/ holds shell and
// localonly scratch; internal/ is this package itself (its synthetic sources
// in self-tests must not trip the real checks).
var skipDirs = map[string]bool{
	".git":     true,
	".claude":  true,
	"tilt":     true,
	"scripts":  true,
	"internal": true,
	"docs":     true,
	"examples": true,
}

// prodGoFileFloor is the number of production (non-test) .go files the root
// module held when this floor was set. Measured 2026-08-19: 163. If a walk
// sees fewer than the floor, the walk is broken, not the tree clean.
const prodGoFileFloor = 130

// testGoFileFloor is the equivalent floor for _test.go files. Measured
// 2026-08-19: 165.
const testGoFileFloor = 130

// goFiles walks the repository and returns parsed ASTs for .go files.
// testFiles selects _test.go files instead of production files.
// The returned map is path (repo-relative) -> parsed file.
func goFiles(t *testing.T, testFiles bool) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && filepath.Dir(path) == root {
				return fs.SkipDir
			}
			if d.Name() == "localonly" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		isTest := strings.HasSuffix(path, "_test.go")
		if isTest != testFiles {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		files[filepath.ToSlash(rel)] = parsed
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}

	floor := prodGoFileFloor
	kind := "production"
	if testFiles {
		floor = testGoFileFloor
		kind = "test"
	}
	if len(files) < floor {
		t.Fatalf("walk parsed only %d %s .go files (floor %d) — the walk is broken, not the tree clean", len(files), kind, floor)
	}
	return files, fset
}

// parseSource parses a synthetic source string for matcher self-tests.
func parseSource(t *testing.T, src string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing synthetic source: %v", err)
	}
	return f, fset
}
