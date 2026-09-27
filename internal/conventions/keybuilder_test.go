package conventions

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// keyBuilderMethodFloor is the KeyBuilder method count when this floor was
// set. Measured 2026-08-19: 56. Below the floor the parse is broken.
const keyBuilderMethodFloor = 40

// keyBuilderMethods parses transport/redis/namespace.go and returns the names
// of every exported method on *KeyBuilder.
func keyBuilderMethods(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "transport", "redis", "namespace.go"), nil, 0)
	if err != nil {
		t.Fatalf("parsing namespace.go: %v", err)
	}

	var methods []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "KeyBuilder" {
			continue
		}
		if fn.Name.IsExported() {
			methods = append(methods, fn.Name.Name)
		}
	}
	if len(methods) < keyBuilderMethodFloor {
		t.Fatalf("parsed only %d KeyBuilder methods (floor %d) — the parse is broken, not the type small", len(methods), keyBuilderMethodFloor)
	}
	return methods
}

// mapKeysIn returns the string literal KEYS of the composite-literal map
// assigned in the named function of the given file (used to read the
// hand-maintained allKeyBuilderOutputs and golden maps in namespace_test.go
// without importing the package's test files).
func mapKeysIn(t *testing.T, path, funcName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	keys := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				keys[strings.Trim(lit.Value, `"`)] = true
			}
			return true
		})
	}
	if len(keys) == 0 {
		t.Fatalf("found no map keys in %s's %s — the function moved or was renamed", path, funcName)
	}
	return keys
}

// TestEveryKeyBuilderMethodIsPinnedByTheGoldenTests is the guard the golden
// tests themselves cannot provide: allKeyBuilderOutputs and the golden-string
// map in transport/redis/namespace_test.go are maintained BY HAND ("New KB
// methods MUST be added here — the property tests below only protect what
// they can see"). A method missing from those maps is a key pattern with no
// golden pin and no partial-namespace property coverage — changing its default
// string would break mixed-version fleets silently.
func TestEveryKeyBuilderMethodIsPinnedByTheGoldenTests(t *testing.T) {
	root := repoRoot(t)
	methods := keyBuilderMethods(t)

	testPath := filepath.Join(root, "transport", "redis", "namespace_test.go")
	outputs := mapKeysIn(t, testPath, "allKeyBuilderOutputs")
	golden := mapKeysIn(t, testPath, "TestKeyBuilder_DefaultGoldenStrings")

	var missing []string
	for _, m := range methods {
		if !outputs[m] {
			missing = append(missing, m+" (missing from allKeyBuilderOutputs)")
		}
		if !golden[m] {
			missing = append(missing, m+" (missing from the golden-string map)")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("KeyBuilder methods not pinned in transport/redis/namespace_test.go:\n%s", joinLines(missing))
	}
}
