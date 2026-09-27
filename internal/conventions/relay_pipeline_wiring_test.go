package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The gRPC relay service copies the relay pipeline when it is built. The relayer
// once built it first, so every gRPC relay in production was served without
// validating its ring signature and without being charged, while the pipeline was
// created a line later for nobody. InitGRPCHandler now refuses a nil pipeline, but
// that only helps if its error stops startup: a discarded error, or the old order
// with the error ignored, is the same defect again. Both call sites are frozen here.

// checkedCallPositions returns the positions of calls to the method name that sit in
// an if statement's init or condition -- calls whose result the if tests.
func checkedCallPositions(f *ast.File, name string) []token.Pos {
	var out []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		for _, part := range []ast.Node{ifs.Init, ifs.Cond} {
			if part == nil {
				continue
			}
			ast.Inspect(part, func(c ast.Node) bool {
				call, ok := c.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == name {
					out = append(out, call.Pos())
				}
				return true
			})
		}
		return true
	})
	return out
}

// relayInitViolations reports what is wrong with the relay pipeline and gRPC
// handler initialisation in f.
func relayInitViolations(f *ast.File, fset *token.FileSet) []string {
	var problems []string
	pipeline := receiverCalls(f, "proxy", "InitializeRelayPipeline")
	grpc := receiverCalls(f, "proxy", "InitGRPCHandler")
	if len(pipeline) != 1 {
		problems = append(problems, "proxy.InitializeRelayPipeline must be called exactly once")
	}
	if len(grpc) != 1 {
		problems = append(problems, "proxy.InitGRPCHandler must be called exactly once")
	}
	if len(pipeline) != 1 || len(grpc) != 1 {
		return problems
	}
	if len(checkedCallPositions(f, "InitializeRelayPipeline")) != 1 {
		problems = append(problems, "the error of proxy.InitializeRelayPipeline is not checked at "+
			fset.Position(pipeline[0].Pos()).String())
	}
	if len(checkedCallPositions(f, "InitGRPCHandler")) != 1 {
		problems = append(problems, "the error of proxy.InitGRPCHandler is not checked at "+
			fset.Position(grpc[0].Pos()).String())
	}
	if grpc[0].Pos() < pipeline[0].Pos() {
		problems = append(problems, "proxy.InitGRPCHandler runs before proxy.InitializeRelayPipeline at "+
			fset.Position(grpc[0].Pos()).String())
	}
	return problems
}

func TestTheRelayerInitializesThePipelineBeforeGRPC(t *testing.T) {
	files, fset := goFiles(t, false)
	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}
	for _, p := range relayInitViolations(f, fset) {
		t.Errorf("%s: %s", path, p)
	}
}

// TestRelayInitViolationsCatchHostileShapes feeds the rule sources the tree does not
// contain, in both directions.
func TestRelayInitViolationsCatchHostileShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"wired", `package x
func a() error {
	if err := proxy.InitializeRelayPipeline(); err != nil {
		return err
	}
	if err := proxy.InitGRPCHandler(); err != nil {
		return err
	}
	return nil
}`, 0},
		{"old order", `package x
func a() error {
	if err := proxy.InitGRPCHandler(); err != nil {
		return err
	}
	if err := proxy.InitializeRelayPipeline(); err != nil {
		return err
	}
	return nil
}`, 1},
		{"discarded errors", `package x
func a() {
	_ = proxy.InitializeRelayPipeline()
	proxy.InitGRPCHandler()
}`, 2},
		{"only mentioned", `package x
// if err := proxy.InitGRPCHandler(); err != nil {
var s = "proxy.InitializeRelayPipeline()"
`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fset := parseSource(t, tc.src)
			got := relayInitViolations(f, fset)
			if len(got) != tc.want {
				t.Fatalf("want %d violations, got %d: %v", tc.want, len(got), got)
			}
		})
	}
}
