//go:build test

package miner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLiveGateHealthyRebroadcastResultsAreEmitted ties the live gate's reading
// of the resend counters to the values this package actually emits.
//
// scripts/gates/live.sh counts a failed resend by NEGATION -- every result
// except the ones it names as healthy -- so a value added later counts as a
// failure until someone decides otherwise. What negation cannot catch is a
// healthy name that nothing emits: a value renamed here and not there stops
// being excluded and turns every run with it red, and a name that never existed
// is dead text that reads like a decision. The gate this replaced asked for
// result="failure", which no code emits, so it read zero on every run; that is
// the shape this pins. The two sources are independent on purpose: the gate's
// own text, and the literals the reconciler passes to recordRebroadcast, read
// from the AST. There is no shared list for them to agree through.
//
// ITS LIMIT, stated rather than implied: the counters are written in exactly
// two places, supplier_manager.go:2682 (claim) and :2773 (proof), and both pass
// through the result recordRebroadcast was given. A WithLabelValues on either
// vector with a literal of its own, anywhere else, would not be seen here.
func TestLiveGateHealthyRebroadcastResultsAreEmitted(t *testing.T) {
	gate, err := os.ReadFile("../scripts/gates/live.sh")
	require.NoError(t, err, "the live gate must be readable: it is what these values are for")

	matches := regexp.MustCompile(`result!~"([^"]*)"`).FindAllStringSubmatch(string(gate), -1)
	require.Lenf(t, matches, 1,
		"scripts/gates/live.sh must name its healthy resend results in exactly one result!~\"...\" "+
			"selector; found %d. If the check moved or changed shape, move this assertion with it", len(matches))
	healthy := strings.Split(matches[0][1], "|")

	emitted := rebroadcastResultsEmitted(t, "inclusion_reconciler.go")
	require.Containsf(t, emitted, "success",
		"control: the scan found %v but not \"success\" -- the scanner is broken, not the emitter", emitted)

	for _, value := range healthy {
		require.Containsf(t, emitted, value,
			"scripts/gates/live.sh treats result=%q as a healthy resend, but the reconciler never passes it "+
				"to recordRebroadcast (it emits %v). Rename it in both places or remove it from the gate", value, emitted)
	}
}

// rebroadcastResultsEmitted returns every string the file passes as the result
// argument of recordRebroadcast. An argument that is neither a string literal
// nor a variable assigned only string literals FAILS the test, naming its line:
// a result computed at runtime cannot be checked against the gate, so it has to
// be looked at rather than skipped.
func rebroadcastResultsEmitted(t *testing.T, file string) map[string]struct{} {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	require.NoError(t, err)

	emitted := make(map[string]struct{})
	calls := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "recordRebroadcast" || len(call.Args) != 3 {
				return true
			}
			calls++
			switch arg := call.Args[2].(type) {
			case *ast.BasicLit:
				emitted[stringLiteral(t, fset, arg)] = struct{}{}
			case *ast.Ident:
				for _, v := range literalsAssignedTo(t, fset, fn.Body, arg.Name) {
					emitted[v] = struct{}{}
				}
			default:
				t.Fatalf("%s: recordRebroadcast's result is neither a string literal nor a variable "+
					"holding only literals, so it cannot be checked against scripts/gates/live.sh",
					fset.Position(call.Pos()))
			}
			return true
		})
	}
	require.Positivef(t, calls,
		"control: found no recordRebroadcast call in %s -- the scan is broken, not the emitter", file)
	return emitted
}

// literalsAssignedTo returns the string literals assigned to name inside body,
// by `=`, `:=` or `var`. Any other assignment to it fails the test.
func literalsAssignedTo(t *testing.T, fset *token.FileSet, body *ast.BlockStmt, name string) []string {
	t.Helper()
	var values []string
	take := func(pos token.Pos, rhs ast.Expr) {
		lit, ok := rhs.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Fatalf("%s: %s is assigned something other than a string literal, so the result it "+
				"carries to recordRebroadcast cannot be checked against scripts/gates/live.sh",
				fset.Position(pos), name)
		}
		values = append(values, stringLiteral(t, fset, lit))
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					require.Lenf(t, s.Rhs, len(s.Lhs), "%s: multi-value assignment to %s", fset.Position(s.Pos()), name)
					take(s.Pos(), s.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				if id.Name == name && i < len(s.Values) {
					take(s.Pos(), s.Values[i])
				}
			}
		}
		return true
	})
	require.NotEmptyf(t, values, "recordRebroadcast is passed %s, and nothing in its function assigns it", name)
	return values
}

func stringLiteral(t *testing.T, fset *token.FileSet, lit *ast.BasicLit) string {
	t.Helper()
	v, err := strconv.Unquote(lit.Value)
	require.NoErrorf(t, err, "%s: unquote %s", fset.Position(lit.Pos()), lit.Value)
	return v
}
