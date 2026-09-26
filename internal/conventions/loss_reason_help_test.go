package conventions

import (
	"go/ast"
	"strings"
	"testing"
)

// The four loss counters carry a `reason` label, and their Help enumerates the
// reasons an operator can expect. That enumeration is the only place the set is
// written down, and it was already wrong: `sessions_failed_total` listed five
// reasons and omitted `panic_recovered`, which `relays_lost_total` has carried
// since the panic path started counting the relay it dropped.
//
// A Help string has no test of its own -- it reads correct to anyone who does
// not know a sixth reason exists -- so this guard compares the Help against the
// reasons the code actually passes. It is the tooth the Help itself cannot have:
// add a reason without naming it, and this goes red.
//
// What is frozen is the completeness of the enumeration, not the wording around
// it.
const lossReasonHelpPath = "miner/metrics.go"

// The enumeration lives in ONE Help, sessions_failed_total, and the other three
// point at it rather than repeating it: four copies of a list is four places to
// forget. So the rule has two halves -- the enumeration is complete, and the
// pointer to it exists.
const lossReasonEnumerator = "sessions_failed_total"

// lossReasonPointers must name lossReasonEnumerator in their Help, so a reader
// who starts at any of them has a next step.
var lossReasonPointers = []string{
	"compute_units_lost_total",
	"upokt_lost_total",
	"relays_lost_total",
}

// lossReasonSinks are the shared recorders that take a `reason` as their third
// argument. Every reason an operator can see on a loss counter is a literal
// passed to one of these.
//
// There used to be ONE, recordSessionFailure, because every failure wrote the
// same four series. The ledger change split that single verdict into four --
// attempt, lost, forgone, unresolved -- which differ only in WHERE the money
// goes, so the reason literals moved with them. The set is listed rather than
// matched by prefix: a prefix would silently start accepting a fifth verdict
// nobody reviewed, and the point of this guard is that adding one is noticed.
var lossReasonSinks = map[string]bool{
	"recordSessionAttemptFailure":   true,
	"recordSessionLoss":             true,
	"recordSessionForgone":          true,
	"RecordSessionUnresolvedOpened": true,
}

// lossReasonLiterals collects the string literals passed as the `reason`
// argument of any lossReasonSinks call, which is the shared path every loss
// counter is written from.
func lossReasonLiterals(f *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || !lossReasonSinks[fun.Name] || len(call.Args) < 3 {
			return true
		}
		if s, ok := stringLit(call.Args[2]); ok {
			out[s] = true
		}
		return true
	})
	return out
}

// lossReasonsByCounter collects reasons written straight to one counter, which
// is how relays_lost_total receives panic_recovered without the others doing so.
func lossReasonsByCounter(f *ast.File) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || fun.Sel.Name != "WithLabelValues" || len(call.Args) != 3 {
			return true
		}
		recv, ok := fun.X.(*ast.Ident)
		if !ok {
			return true
		}
		if !strings.HasSuffix(recv.Name, "LostTotal") && !strings.HasSuffix(recv.Name, "FailedTotal") {
			return true
		}
		reason, ok := stringLit(call.Args[2])
		if !ok {
			return true
		}
		if out[recv.Name] == nil {
			out[recv.Name] = map[string]bool{}
		}
		out[recv.Name][reason] = true
		return true
	})
	return out
}

func TestLossCounterHelpNamesEveryReason(t *testing.T) {
	files, _ := goFiles(t, false)
	f, ok := files[lossReasonHelpPath]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", lossReasonHelpPath)
	}

	reasons := lossReasonLiterals(f)
	if len(reasons) < 5 {
		t.Fatalf("CONTROL: found only %d reason literals in %s (%v); the matcher stopped seeing them",
			len(reasons), lossReasonHelpPath, reasons)
	}
	helps := counterHelpByName(f)

	enumerated, ok := helps[lossReasonEnumerator]
	if !ok {
		t.Fatalf("%s declares no Help literal in %s", lossReasonEnumerator, lossReasonHelpPath)
	}
	for reason := range reasons {
		if !strings.Contains(enumerated, reason) {
			t.Errorf("%s: Help does not name the reason %q that the code records:\n  %s",
				lossReasonEnumerator, reason, enumerated)
		}
	}

	// A reason written straight to one counter is named by that counter's own
	// Help: panic_recovered reaches relays_lost_total and nothing else.
	for counter, own := range lossReasonsByCounter(f) {
		for reason := range own {
			if reasons[reason] {
				continue // already covered by the enumeration above
			}
			metric := lossCounterMetricName(counter)
			help, ok := helps[metric]
			if !ok {
				t.Fatalf("%s (%s) declares no Help literal", counter, metric)
			}
			if !strings.Contains(help, reason) {
				t.Errorf("%s: Help does not name %q, which only this counter receives:\n  %s",
					metric, reason, help)
			}
		}
	}

	for _, counter := range lossReasonPointers {
		help, ok := helps[counter]
		if !ok {
			t.Fatalf("%s declares no Help literal in %s", counter, lossReasonHelpPath)
		}
		if !strings.Contains(help, lossReasonEnumerator) {
			t.Errorf("%s: Help must point at %s, where the reasons are enumerated:\n  %s",
				counter, lossReasonEnumerator, help)
		}
	}
}

// lossCounterMetricName maps the Go variable of a loss counter to its metric
// name. Kept explicit: deriving it from the identifier would guess.
func lossCounterMetricName(varName string) string {
	switch varName {
	case "sessionsFailedTotal":
		return "sessions_failed_total"
	case "relaysLostTotal":
		return "relays_lost_total"
	case "computeUnitsLostTotal":
		return "compute_units_lost_total"
	case "upoktLostTotal":
		return "upokt_lost_total"
	}
	return varName
}

// TestLossReasonMatcherCatchesHostileShapes: the guard above is worth only what
// its matchers see, so this pins both of them against the two shapes the code
// uses and against a reason that is not a literal.
func TestLossReasonMatcherCatchesHostileShapes(t *testing.T) {
	// One fixture line per verdict, because the four are exactly what replaced
	// the single recordSessionFailure and a matcher that saw only some of them
	// would leave whole verdicts unenumerated.
	src := `package p

func f() {
	recordSessionAttemptFailure(s, id, "via_attempt")
	recordSessionLoss(s, id, "via_loss", 1, 2)
	recordSessionForgone(s, id, "via_forgone", 1, 2)
	RecordSessionUnresolvedOpened(s, id, "via_unresolved", phase, 1, 2)
	notASink(s, id, "via_stranger", 1, 2)
	relaysLostTotal.WithLabelValues(s, id, "via_counter").Add(1)
	sessionsFailedTotal.WithLabelValues(s, id, notALiteral).Inc()
}
`
	f, _ := parseSource(t, src)

	viaHelper := lossReasonLiterals(f)
	for _, want := range []string{"via_attempt", "via_loss", "via_forgone", "via_unresolved"} {
		if !viaHelper[want] {
			t.Errorf("the matcher misses %q, a reason passed to a shared verdict recorder", want)
		}
	}
	if viaHelper["via_stranger"] {
		t.Error("a third argument to a function that is NOT a verdict recorder must not be collected")
	}
	if len(viaHelper) != 4 {
		t.Errorf("only the shared verdict paths belong in the enumeration: %v", viaHelper)
	}

	perCounter := lossReasonsByCounter(f)
	if !perCounter["relaysLostTotal"]["via_counter"] {
		t.Error("the matcher misses a reason passed straight to a loss counter")
	}
	if got := len(perCounter["sessionsFailedTotal"]); got != 0 {
		t.Errorf("a reason built at run time is not a literal and must not be collected: %v",
			perCounter["sessionsFailedTotal"])
	}
}
