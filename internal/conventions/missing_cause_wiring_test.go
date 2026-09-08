package conventions

import (
	"go/ast"
	"testing"
)

// The pair this check exists for. resolveMissingCause decides whether a cause
// may be named at all; recordMissingCause is the only place that turns a cause
// into a metric label an operator reads.
const (
	causeDecider  = "resolveMissingCause"
	causeReporter = "recordMissingCause"
)

// callsWithin reports whether the function named fn in f contains a call to
// callee.
//
// It returns whether the function was FOUND separately from whether it calls,
// and both matter: a check that only fires when the function exists goes green
// forever the day somebody renames it, which is the shape of a guard that stops
// biting without anyone noticing.
func callsWithin(f *ast.File, fn, callee string) (found, calls bool) {
	for _, decl := range f.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Name.Name != fn || d.Body == nil {
			continue
		}
		found = true
		ast.Inspect(d.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == callee {
				calls = true
			}
			return true
		})
	}
	return found, calls
}

// TestMissingCauseDeciderStaysWired asserts that the reporter still consults the
// decider.
//
// It exists because the decision was extracted into a pure function precisely so
// it could be tested — TxClient is concrete, so recordMissingCause cannot be
// driven from a test without inventing an interface for no other purpose — and
// that extraction left the CALL untested. Measured, not assumed: replacing that
// one line with a discard leaves the whole miner package green.
//
// What is lost if the call goes is not cosmetic. Past one resend the entry no
// longer holds the hash of every transaction broadcast, so a NotInBlock verdict
// can be INVERTED rather than merely incomplete: the operator is told the tx
// never reached a block when it may have been included and failed, which is the
// opposite diagnosis and points at the opposite fix.
//
// This is a check and not a general "every resolve* must have a caller" rule.
// That rule would fire on code nobody is worried about, and noise nobody pays
// for is how a convention suite stops being read.
func TestMissingCauseDeciderStaysWired(t *testing.T) {
	files, _ := goFiles(t, false)

	var foundAnywhere bool
	for path, f := range files {
		found, calls := callsWithin(f, causeReporter, causeDecider)
		if !found {
			continue
		}
		foundAnywhere = true
		if !calls {
			t.Fatalf(
				"%s: %s no longer calls %s.\n"+
					"Without it a cause is named from a strict subset of the transactions actually "+
					"broadcast, so an attempt that was included and FAILED reads as one that never "+
					"reached a block — an inverted label, not a missing one.",
				path, causeReporter, causeDecider,
			)
		}
	}

	// The other half of the guard: if the reporter is gone or renamed, the loop
	// above has nothing to judge and would pass in silence.
	if !foundAnywhere {
		t.Fatalf(
			"%s was not found in production code. If it was renamed, point this check at the new "+
				"name; if the reporting path was removed, delete this check deliberately rather than "+
				"leaving it green over nothing.",
			causeReporter,
		)
	}
}

// TestMissingCauseWiringMatcherCatchesHostileShapes proves the matcher
// discriminates. Both halves of this check compare an AST against a name
// written by hand, so it is not the tautology of comparing a set with itself —
// but the matcher still has to be shown to separate the cases.
func TestMissingCauseWiringMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
func recordMissingCause() { cause := read(); _ = cause }        // found, does NOT call
func somethingElse()      { _ = resolveMissingCause(1, 2) }     // the call, wrong function
`
	f, _ := parseSource(t, hostile)

	found, calls := callsWithin(f, causeReporter, causeDecider)
	if !found {
		t.Fatalf("the matcher must find %s when it is declared", causeReporter)
	}
	if calls {
		t.Fatalf("a call from a DIFFERENT function must not satisfy the check")
	}

	wired := `package x
func recordMissingCause() { cause := read(); cause = resolveMissingCause(cause, 2); _ = cause }
`
	g, _ := parseSource(t, wired)
	if found, calls := callsWithin(g, causeReporter, causeDecider); !found || !calls {
		t.Fatalf("the matcher must accept the wired shape, got found=%v calls=%v", found, calls)
	}
}
