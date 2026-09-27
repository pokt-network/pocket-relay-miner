package conventions

import (
	"go/ast"
	"testing"
)

// The pair this check exists for.
//
// settleLedgerOutcome is the only place a session's money CLOSES: it brings the
// unresolved balance down and names where the money went. Its two callers are
// anonymous closures -- recordClaimOutcome and recordProofOutcome -- built inside
// ensureSharedTrackers and handed to the inclusion reconciler as function values.
// Nothing named calls it, so no test in the miner package can reach the call
// sites without standing up a reconciler, a block client and a proof query
// client.
//
// That makes this an untested WIRE with money behind it. Removing either call
// leaves the whole miner package green while an unresolved balance never comes
// down again: the ledger identity claimed = proved + lost + unresolved stops
// closing, and the series whose entire job is to reveal a gap becomes the gap.
// Measured, not assumed: settleLedgerOutcome has unit tests of its own and every
// one of them passes with both call sites deleted.
const (
	ledgerSettler  = "settleLedgerOutcome"
	ledgerWireHost = "ensureSharedTrackers"

	// One per phase, claim and proof. A single call would silently leave one
	// phase's balances open forever, which is the failure this count catches and
	// a bare "is it called at all" check would not.
	ledgerSettlerCalls = 2
)

// selectorCallsWithin counts calls of the form <recv>.callee inside the function
// named fn, including calls nested in closures declared there -- which is the
// whole point, since both call sites live in closures.
//
// It reports whether the host function was FOUND separately from the count,
// because a check that only fires when the function exists goes green forever the
// day somebody renames it.
func selectorCallsWithin(f *ast.File, fn, callee string) (found bool, calls int) {
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
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == callee {
				calls++
			}
			return true
		})
	}
	return found, calls
}

// TestLedgerSettlementStaysWired asserts both inclusion-outcome closures still
// hand their outcome to the ledger.
func TestLedgerSettlementStaysWired(t *testing.T) {
	files, _ := goFiles(t, false)

	var foundAnywhere bool
	for path, f := range files {
		found, calls := selectorCallsWithin(f, ledgerWireHost, ledgerSettler)
		if !found {
			continue
		}
		foundAnywhere = true
		if calls != ledgerSettlerCalls {
			t.Fatalf(
				"%s: %s calls %s %d times, expected %d (one per phase).\n"+
					"Each missing call is a phase whose unresolved balance never closes: the money "+
					"stays counted as pending forever and claimed = proved + lost + unresolved stops "+
					"holding. If a phase genuinely stopped needing settlement, change the expected "+
					"count deliberately rather than letting this check pass over a silent gap.",
				path, ledgerWireHost, ledgerSettler, calls, ledgerSettlerCalls,
			)
		}
	}

	// The other half of the guard: if the host is gone or renamed, the loop above
	// has nothing to judge and would pass in silence.
	if !foundAnywhere {
		t.Fatalf(
			"%s was not found in production code. If it was renamed, point this check at the new "+
				"name; if the inclusion-outcome closures moved, re-anchor this check there rather "+
				"than leaving it green over nothing.",
			ledgerWireHost,
		)
	}
}

// TestLedgerSettlementMatcherCatchesHostileShapes proves the matcher
// discriminates. Both halves compare an AST against a name written by hand, so
// it is not the tautology of comparing a set with itself -- but the matcher still
// has to be shown to separate the cases.
func TestLedgerSettlementMatcherCatchesHostileShapes(t *testing.T) {
	hostile := `package x
func ensureSharedTrackers() { f := func() { _ = 1 }; _ = f }
func somewhereElse()        { m.settleLedgerOutcome(1) }
`
	f, _ := parseSource(t, hostile)

	found, calls := selectorCallsWithin(f, ledgerWireHost, ledgerSettler)
	if !found {
		t.Fatalf("the matcher must find %s when it is declared", ledgerWireHost)
	}
	if calls != 0 {
		t.Fatalf("a call from a DIFFERENT function must not be counted, got %d", calls)
	}

	wired := `package x
func ensureSharedTrackers() {
	a := func() { m.settleLedgerOutcome(1) }
	b := func() { m.settleLedgerOutcome(2) }
	_, _ = a, b
}
`
	g, _ := parseSource(t, wired)
	if found, calls := selectorCallsWithin(g, ledgerWireHost, ledgerSettler); !found || calls != 2 {
		t.Fatalf("the matcher must count calls inside closures, got found=%v calls=%d", found, calls)
	}

	// One call where two are required is the case the count exists for, and it
	// must be distinguishable from both zero and two.
	half := `package x
func ensureSharedTrackers() { a := func() { m.settleLedgerOutcome(1) }; _ = a }
`
	h, _ := parseSource(t, half)
	if _, calls := selectorCallsWithin(h, ledgerWireHost, ledgerSettler); calls != 1 {
		t.Fatalf("a single call must count as exactly 1, got %d", calls)
	}
}
