package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// The KeyBuilder STRONG RULE (CLAUDE.md): every Redis key and pub/sub channel
// is built through transport/redis/namespace.go. Two shapes reintroduce
// hand-built keys and both already caused real bugs (two caches listening on
// different channels; a meter whose writer and reader disagreed on segments):
//
//  1. fmt.Sprintf with a format string starting in the "ha:" namespace;
//  2. string concatenation gluing a ":"-prefixed literal onto an expression
//     (`prefix + ":session:" + id`).

// haPrefix is assembled so a mechanical rename of the namespace cannot
// rewrite the rule that polices it.
var haPrefix = "ha" + ":"

// sprintfHAKeys returns positions of fmt.Sprintf calls whose format literal
// starts with the "ha:" namespace.
func sprintfHAKeys(f *ast.File, fset *token.FileSet) []string {
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sprintf" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if strings.HasPrefix(strings.Trim(lit.Value, "`\""), haPrefix) {
			hits = append(hits, fset.Position(call.Pos()).String())
		}
		return true
	})
	return hits
}

// keySuffixConcats returns positions of BinaryExpr additions where the right
// operand is a string literal starting a key SEGMENT — ":" followed by a
// lowercase segment character — the `prefix + ":session:" + id` shape.
// Deliberately not matched: prose with spaces (help text, log messages), the
// URL separator "://", and glob suffixes like ":*" (scan patterns over a
// KeyBuilder-derived prefix, not key construction).
func keySuffixConcats(f *ast.File, fset *token.FileSet) []string {
	isSegmentChar := func(c byte) bool {
		return c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || bin.Op != token.ADD {
			return true
		}
		lit, ok := bin.Y.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val := strings.Trim(lit.Value, "`\"")
		if len(val) > 1 && val[0] == ':' && isSegmentChar(val[1]) && !strings.Contains(val, " ") {
			hits = append(hits, fset.Position(bin.Pos()).String())
		}
		return true
	})
	return hits
}

// keyLiteralExemptions are files allowed to fail these matchers, each with a
// written reason. Adding a file here is a deliberate, reviewed edit.
var keyLiteralExemptions = map[string]string{
	// The KeyBuilder itself and its namespace plumbing.
	"transport/redis/namespace.go": "this IS the KeyBuilder",
	"transport/redis/client.go":    "namespace plumbing next to the KeyBuilder",
	// Legacy-schema migration: it must spell the OLD key shapes to move data
	// out of them; giving them KB methods would keep the retired shapes alive.
	"miner/smst_migration.go": "legacy key-schema migration reads retired shapes",
	// Tombstone validation: compares a RETIRED config prefix against the
	// namespace to refuse configs that would relocate meter keys. The concat
	// documents where keys USED to live, it builds no live key.
	"relayer/config.go": "retired relay_meter.redis_key_prefix tombstone comparison",
	// Cleanup CLI: derives its scan patterns and lock prefixes FROM
	// kb.CachePrefix(); the appended segments are glob/scan syntax over that
	// prefix, not a second key constructor.
	"cmd/redis/cache_all.go": "scan patterns derived from kb.CachePrefix()",
}

// TestNoHandBuiltRedisKeysOutsideTheKeyBuilder walks production code and
// fails on either shape outside the exemption list.
func TestNoHandBuiltRedisKeysOutsideTheKeyBuilder(t *testing.T) {
	files, fset := goFiles(t, false)

	var violations []string
	for path, f := range files {
		if _, exempt := keyLiteralExemptions[path]; exempt {
			continue
		}
		for _, pos := range sprintfHAKeys(f, fset) {
			violations = append(violations, fmt.Sprintf("%s (Sprintf ha:)", pos))
		}
		for _, pos := range keySuffixConcats(f, fset) {
			violations = append(violations, fmt.Sprintf("%s (prefix + \":suffix\" concat)", pos))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("hand-built Redis keys outside transport/redis (add a KeyBuilder method instead):\n%s",
			joinLines(violations))
	}
}

// TestKeyLiteralMatchersCatchHostileShapes proves both matchers can fail and
// refuse near-misses.
func TestKeyLiteralMatchersCatchHostileShapes(t *testing.T) {
	hostile := `package x
import "fmt"
var a = fmt.Sprintf("ha:cache:%s", "k")           // MUST match (Sprintf)
var b = fmt.Sprintf("other:%s", "k")              // must not (different namespace)
var c = somePrefix + ":session:" + id             // MUST match (concat)
var d = "error" + ": " + msg                      // must not (prose, has space)
var e = base + ":"                                // must not (bare separator)
var f = fmt.Errorf("ha:cache broke")              // must not (Errorf, not Sprintf)
var g = scheme + "://" + host                     // must not (URL separator)
var h = prefix2 + ":*"                            // must not (glob suffix)
var somePrefix, id, msg, base, scheme, host, prefix2 string
`
	f, fset := parseSource(t, hostile)
	if got := len(sprintfHAKeys(f, fset)); got != 1 {
		t.Fatalf("Sprintf matcher found %d hits in the synthetic source, want 1", got)
	}
	if got := len(keySuffixConcats(f, fset)); got != 1 {
		t.Fatalf("concat matcher found %d hits in the synthetic source, want 1", got)
	}
}
