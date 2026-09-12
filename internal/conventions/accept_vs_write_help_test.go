package conventions

import (
	"go/ast"
	"strings"
	"testing"
)

// The relayer has four counters that move when a mined relay is ACCEPTED by the
// publisher, and one drop reason that moves when it is refused. None of them
// means the relay reached Redis: with redis.batch_publish_interval_ms set,
// accepting is enqueuing and the XADD happens later on the dispatcher's
// goroutine. The counter that means "written" is ha_transport_published_total,
// and the errors on that side are ha_transport_publish_errors_total.
//
// This rule exists because the gap it closes already happened. The council that
// decided how to count settled on "change nothing but the Help" -- and the Help
// was never changed, so for two commits `relays_published_total` said "published
// to the store" while counting things that were merely queued. Nothing noticed,
// because a Help string has no test and reads correct to anyone who does not
// know a second publisher exists.
//
// What is frozen is not the wording. It is that each of these Helps NAMES the
// series an operator has to look at instead: a Help that says only "this is not
// what you think" leaves them with no next step, which is the difference between
// a correction and a useful one.
var acceptTimeCounterHelp = map[string]string{
	"relays_published_total":         "ha_transport_published_total",
	"grpc_relays_published_total":    "ha_transport_published_total",
	"websocket_relays_emitted_total": "ha_transport_published_total",
	"relays_mined_total":             "ha_transport_published_total",
	"relays_dropped_total":           "ha_transport_publish_errors_total",
}

// counterHelpByName returns Name -> Help for every prometheus *Opts composite
// literal in f that sets both as plain string literals.
func counterHelpByName(f *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		var name, help string
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			s, ok := stringLit(kv.Value)
			if !ok {
				continue
			}
			switch key.Name {
			case "Name":
				name = s
			case "Help":
				help = s
			}
		}
		if name != "" && help != "" {
			out[name] = help
		}
		return true
	})
	return out
}

// stringLit unquotes a plain string literal. Anything built at run time is not
// a literal and is deliberately invisible here: a Help assembled from variables
// cannot be read by an operator from the source either.
func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind.String() != "STRING" {
		return "", false
	}
	v := bl.Value
	if len(v) < 2 || v[0] != '"' {
		return "", false
	}
	return v[1 : len(v)-1], true
}

func TestAcceptTimeCountersNameTheWriteCounter(t *testing.T) {
	files, _ := goFiles(t, false)
	const path = "relayer/metrics.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}

	// The series these Helps point at have to EXIST. A substring check alone
	// freezes the text and not the fact: rename ha_transport_published_total and
	// every Help here keeps a dangling pointer while this rule stays green.
	declared := declaredTransportSeries(t, files)

	helps := counterHelpByName(f)
	// A floor, so an empty walk cannot pass as a clean tree: this file declares
	// dozens of counters and reading none of them is a broken matcher.
	if len(helps) < 20 {
		t.Fatalf("only %d metric Help strings parsed out of %s -- the matcher is broken, not the file clean",
			len(helps), path)
	}

	for name, mustName := range acceptTimeCounterHelp {
		help, ok := helps[name]
		if !ok {
			t.Errorf("%s no longer declares %s.\n"+
				"  If it was renamed, re-point this rule. Renaming it is itself a decision: the\n"+
				"  counting council settled on NOT migrating these names, because a mixed fleet\n"+
				"  and every dashboard read them.", path, name)
			continue
		}
		if !declared[mustName] {
			t.Errorf("the Help of %s points at %s, which is not declared anywhere.\n"+
				"  A Help naming a series that does not exist is worse than one naming none:\n"+
				"  it sends the operator to an empty query and reads like their scrape is\n"+
				"  broken. If the transport counter was renamed, this Help follows it.",
				name, mustName)
			continue
		}
		if !strings.Contains(help, mustName) {
			t.Errorf("the Help of %s does not name %s.\n"+
				"  This counter moves when the publisher ACCEPTS a relay, which with\n"+
				"  redis.batch_publish_interval_ms set means QUEUED, not written. An operator\n"+
				"  reading it has to be told which series does mean written, or the honest\n"+
				"  reading of a batched run is 'served minus published = 0' while a whole\n"+
				"  chunk was lost.\n"+
				"  Help is: %q", name, mustName, help)
		}
	}
}

// TestCounterHelpMatcherCatchesHostileShapes proves the matcher reads the pair
// out of one literal and does not pick a Name from one and a Help from another.
// declaredTransportSeries builds the full Prometheus names the transport
// package declares, from its namespace and subsystem constants plus each
// counter's Name -- the same three pieces Prometheus itself joins.
func declaredTransportSeries(t *testing.T, files map[string]*ast.File) map[string]bool {
	t.Helper()
	const path = "transport/redis/metrics.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}

	ns, sub := constString(f, "metricsNamespace"), constString(f, "metricsSubsystem")
	if ns == "" || sub == "" {
		t.Fatalf("%s: could not read metricsNamespace/metricsSubsystem (%q/%q)", path, ns, sub)
	}

	out := map[string]bool{}
	for name := range counterHelpByName(f) {
		out[ns+"_"+sub+"_"+name] = true
	}
	if len(out) < 5 {
		t.Fatalf("only %d series parsed out of %s -- the matcher is broken, not the file empty",
			len(out), path)
	}
	return out
}

// constString reads a `name = "literal"` declaration.
func constString(f *ast.File, name string) string {
	got := ""
	ast.Inspect(f, func(n ast.Node) bool {
		if got != "" {
			return false
		}
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 || vs.Names[0].Name != name {
			return true
		}
		if s, ok := stringLit(vs.Values[0]); ok {
			got = s
		}
		return got == ""
	})
	return got
}

func TestCounterHelpMatcherCatchesHostileShapes(t *testing.T) {
	good := `package x

var a = prometheus.CounterOpts{
	Name: "one_total",
	Help: "counts one, see other_total",
}
var b = prometheus.CounterOpts{
	Name: "two_total",
	Help: "counts two",
}
`
	f, _ := parseSource(t, good)
	got := counterHelpByName(f)
	if got["one_total"] != "counts one, see other_total" || got["two_total"] != "counts two" {
		t.Fatalf("matcher paired Name and Help across literals: %#v", got)
	}

	// A Help built at run time is not a literal and must simply be absent,
	// rather than silently reported as an empty string that passes.
	computed := `package x

var c = prometheus.CounterOpts{
	Name: "three_total",
	Help: prefix + " counts three",
}
`
	f, _ = parseSource(t, computed)
	if _, ok := counterHelpByName(f)["three_total"]; ok {
		t.Fatal("matcher accepted a Help that is not a string literal")
	}
}
