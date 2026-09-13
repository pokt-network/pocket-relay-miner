package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// Served relays are charged by the batch dispatcher, and the meter closes
// admission when that dispatcher stops reaching Redis. runHARelayer builds a whole
// process and has no test, so the wiring is frozen by reading it.
//
// What goes red: either call missing, repeated or under a condition (the meter
// would refuse every relay, or serve with nothing writing its charges); a
// heartbeat taken from anything but the concrete batcher's LastSuccess, such as a
// type assertion on the publisher interface; and wiring placed after the relay
// pipeline is initialized.

// chargeWiringViolations reports what is wrong with the ledger and heartbeat
// wiring in f. The concrete batcher is the variable assigned from
// NewBatchingPublisher.
func chargeWiringViolations(f *ast.File, fset *token.FileSet) []string {
	var out []string

	batcher := ""
	ast.Inspect(f, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || !callsName(call, "NewBatchingPublisher") {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			batcher = id.Name
		}
		return true
	})
	if batcher == "" {
		return []string{"no variable is assigned from NewBatchingPublisher"}
	}

	calls := map[string]*ast.CallExpr{}
	for _, wanted := range []string{"SetChargeLedger", "SetDispatcherHeartbeat"} {
		unconditional, conditional := callsOf(f, wanted)
		for _, pos := range conditional {
			out = append(out, wanted+" is called under a condition at "+fset.Position(pos).String())
		}
		if len(unconditional) != 1 {
			out = append(out, wanted+" must be called unconditionally exactly once")
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && call.Pos() == unconditional[0] {
				calls[wanted] = call
			}
			return true
		})
	}

	if call := calls["SetChargeLedger"]; call != nil {
		sel, _ := call.Fun.(*ast.SelectorExpr)
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != batcher {
			out = append(out, "SetChargeLedger is not called on the concrete batcher "+batcher)
		}
	}
	if call := calls["SetDispatcherHeartbeat"]; call != nil {
		ok := false
		if len(call.Args) == 1 {
			if sel, isSel := call.Args[0].(*ast.SelectorExpr); isSel && sel.Sel.Name == "LastSuccess" {
				id, isIdent := sel.X.(*ast.Ident)
				ok = isIdent && id.Name == batcher
			}
		}
		if !ok {
			out = append(out, "SetDispatcherHeartbeat must be given "+batcher+".LastSuccess, from the concrete batcher")
		}
	}

	pipelineInit, _ := callsOf(f, "InitializeRelayPipeline")
	for _, name := range []string{"SetChargeLedger", "SetDispatcherHeartbeat"} {
		call := calls[name]
		if call == nil {
			continue
		}
		for _, pos := range pipelineInit {
			if call.Pos() > pos {
				out = append(out, name+" at "+fset.Position(call.Pos()).String()+" comes after InitializeRelayPipeline")
			}
		}
	}
	return out
}

func TestTheRelayerWiresChargesAndHeartbeatFromTheBatcher(t *testing.T) {
	files, fset := goFiles(t, false)

	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}
	for _, v := range chargeWiringViolations(f, fset) {
		t.Errorf("%s: %s", path, v)
	}
}

// TestChargeWiringViolationsReadsTheShape proves the rule reads the receiver, the
// argument, the guard and the order, not merely the names.
func TestChargeWiringViolationsReadsTheShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"wired", `
	batcher := x.NewBatchingPublisher()
	batcher.SetChargeLedger(meter.ChargeLedger())
	meter.SetDispatcherHeartbeat(batcher.LastSuccess)
	proxy.InitializeRelayPipeline()`, 0},
		{"heartbeat through a type assertion", `
	batcher := x.NewBatchingPublisher()
	var publisher transport.MinedRelayPublisher = batcher
	batcher.SetChargeLedger(meter.ChargeLedger())
	meter.SetDispatcherHeartbeat(publisher.(*x.BatchingPublisher).LastSuccess)
	proxy.InitializeRelayPipeline()`, 1},
		{"ledger on another receiver", `
	batcher := x.NewBatchingPublisher()
	other.SetChargeLedger(meter.ChargeLedger())
	meter.SetDispatcherHeartbeat(batcher.LastSuccess)
	proxy.InitializeRelayPipeline()`, 1},
		{"guarded ledger", `
	batcher := x.NewBatchingPublisher()
	if on {
		batcher.SetChargeLedger(meter.ChargeLedger())
	}
	meter.SetDispatcherHeartbeat(batcher.LastSuccess)
	proxy.InitializeRelayPipeline()`, 2},
		{"heartbeat missing", `
	batcher := x.NewBatchingPublisher()
	batcher.SetChargeLedger(meter.ChargeLedger())
	proxy.InitializeRelayPipeline()`, 1},
		{"wired after the pipeline", `
	batcher := x.NewBatchingPublisher()
	proxy.InitializeRelayPipeline()
	batcher.SetChargeLedger(meter.ChargeLedger())
	meter.SetDispatcherHeartbeat(batcher.LastSuccess)`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fset := parseSource(t, "package x\n\nfunc run() {"+tc.body+"\n}\n")
			got := chargeWiringViolations(f, fset)
			if len(got) != tc.want {
				t.Fatalf("want %d violations, got %d: %q", tc.want, len(got), got)
			}
		})
	}
}
