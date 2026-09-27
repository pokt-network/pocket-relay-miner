package conventions

import (
	"go/ast"
	"go/printer"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// typeString renders a type expression so shapes can be compared as text.
func typeString(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, e); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(sb.String()), "")
}

// parallelSessionResults returns the names of functions that take a slice of
// session snapshots and hand back a SEPARATE slice, which is the shape where the
// answer for a session travels in a POSITION instead of with the session.
//
// It exists because that shape was in the claim path and drifted: the outgoing
// slice was filled through a counter that advanced only for sessions that
// submitted, so it left-packed, and the caller -- reading position i as "the
// answer for session i" -- transitioned the first k sessions whatever they were,
// resurrecting ones already closed as terminal. The fix removed the slice and
// named the sessions instead, and this check is what keeps it removed: the
// guarantee is structural (the shape no longer compiles into the interface), and
// this makes reintroducing it a red rather than a reviewer's catch.
func parallelSessionResults(fset *token.FileSet, f *ast.File) []string {
	var found []string

	check := func(name string, ft *ast.FuncType) {
		if ft.Params == nil || ft.Results == nil {
			return
		}
		takesSessions := false
		for _, p := range ft.Params.List {
			if typeString(fset, p.Type) == "[]*SessionSnapshot" {
				takesSessions = true
			}
		}
		if !takesSessions {
			return
		}
		for _, r := range ft.Results.List {
			rt := typeString(fset, r.Type)
			if !strings.HasPrefix(rt, "[][]") {
				continue
			}
			// A result that CARRIES the sessions is not the dangerous shape: a
			// partition like [][]*SessionSnapshot holds each session inside the
			// element, so there is no position to read as "the answer for
			// session i" and nothing to drift. The shape that bites is the one
			// whose elements have no identity of their own.
			if strings.Contains(rt, "SessionSnapshot") {
				continue
			}
			found = append(found, name)
			return
		}
	}

	ast.Inspect(f, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			check(n.Name.Name, n.Type)
		case *ast.InterfaceType:
			for _, m := range n.Methods.List {
				ft, ok := m.Type.(*ast.FuncType)
				if !ok || len(m.Names) == 0 {
					continue
				}
				check(m.Names[0].Name, ft)
			}
		}
		return true
	})
	return found
}

// TestNoParallelSliceResultForSessions fails on any production function that
// answers about a batch of sessions through a second, positional slice.
func TestNoParallelSliceResultForSessions(t *testing.T) {
	files, fset := goFiles(t, false)

	var violations []string
	for path, f := range files {
		for _, name := range parallelSessionResults(fset, f) {
			violations = append(violations, path+": "+name)
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("a batch of sessions answered through a parallel slice — return the answer keyed by "+
			"session ID instead, so it cannot drift out of step with the sessions:\n%s", joinLines(violations))
	}
}

// TestParallelSliceMatcherCatchesHostileShapes proves the matcher sees the shape
// that was removed and ignores the ones that replaced it.
func TestParallelSliceMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
type SessionSnapshot struct{}
type ClaimCycleResult struct{}
// MUST match: the exact shape that was removed.
func A(s []*SessionSnapshot) ([][]byte, error) { return nil, nil }
// MUST match too: renaming the element type does not change the shape.
func B(s []*SessionSnapshot) ([][]string, error) { return nil, nil }
// must NOT match: the answer is keyed by identity.
func C(s []*SessionSnapshot) (ClaimCycleResult, error) { return ClaimCycleResult{}, nil }
// must NOT match: a parallel slice about something that is not sessions.
func D(s []string) ([][]byte, error) { return nil, nil }
// must NOT match: one session, one answer -- no positions to misalign.
func E(s *SessionSnapshot) ([]byte, error) { return nil, nil }
// must NOT match: a partition carries each session inside the element, so no
// position is being read as "the answer for session i".
func F(s []*SessionSnapshot) [][]*SessionSnapshot { return nil }
`
	f, fset := parseSource(t, hostile)
	got := parallelSessionResults(fset, f)
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("matcher got %v, want [A B]", got)
	}
}
