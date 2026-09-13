package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The relayer stops admitting relays while the batch queue holds more bytes than
// redis.batch_max_queued_mib allows. runHARelayer builds a whole process and has
// no test, so the wiring is frozen by reading it: without the SetPublishQueueFull
// call every transport admits forever and the queue grows with the heap.
//
// The gate must read the CONCRETE batcher. The proxy wraps the publisher it is
// given in a counting decorator, so a type assertion on the wrapped value fails
// silently and leaves the gate reading nothing.

// queueGateViolations reports what is wrong with the queue gate wiring in f.
func queueGateViolations(f *ast.File, fset *token.FileSet) []string {
	var problems []string

	unconditional, conditional := callsOf(f, "SetPublishQueueFull")
	for _, pos := range conditional {
		problems = append(problems, "SetPublishQueueFull is called under a condition at "+fset.Position(pos).String())
	}
	if len(unconditional) != 1 {
		problems = append(problems, "SetPublishQueueFull must be called unconditionally exactly once")
	}

	readsBatcher := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !callsName(call, "SetPublishQueueFull") || len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok {
			return true
		}
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			if c, ok := m.(*ast.CallExpr); ok && callsName(c, "QueuedBytes") {
				readsBatcher = true
			}
			return true
		})
		return true
	})
	if len(unconditional) == 1 && !readsBatcher {
		problems = append(problems, "the gate passed to SetPublishQueueFull must read QueuedBytes from the batcher")
	}

	ast.Inspect(f, func(n ast.Node) bool {
		ta, ok := n.(*ast.TypeAssertExpr)
		if !ok {
			return true
		}
		star, ok := ta.Type.(*ast.StarExpr)
		if !ok {
			return true
		}
		if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "BatchingPublisher" {
			problems = append(problems, "type assertion to *BatchingPublisher at "+fset.Position(ta.Pos()).String())
		}
		return true
	})
	return problems
}

func TestTheRelayerWiresThePublishQueueGate(t *testing.T) {
	files, fset := goFiles(t, false)

	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}
	for _, p := range queueGateViolations(f, fset) {
		t.Errorf("%s: %s", path, p)
	}
}

// TestQueueGateViolationsReadsTheWiring proves the rule reads the call, its guard,
// what the gate reads and the assertion, and not merely the name.
func TestQueueGateViolationsReadsTheWiring(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"wired", `package x
func a() {
	proxy.SetPublishQueueFull(func() bool { return batcher.QueuedBytes() >= max })
}`, 0},
		{"missing", `package x
func a() {}`, 1},
		{"guarded", `package x
func a() {
	if on {
		proxy.SetPublishQueueFull(func() bool { return batcher.QueuedBytes() >= max })
	}
}`, 2},
		{"reads something else", `package x
func a() {
	proxy.SetPublishQueueFull(func() bool { return false })
}`, 1},
		{"asserts through the decorator", `package x
func a() {
	b := publisher.(*redistransport.BatchingPublisher)
	proxy.SetPublishQueueFull(func() bool { return b.QueuedBytes() >= max })
}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fset := parseSource(t, tc.src)
			got := queueGateViolations(f, fset)
			if len(got) != tc.want {
				t.Fatalf("want %d violations, got %d: %v", tc.want, len(got), got)
			}
		})
	}
}
