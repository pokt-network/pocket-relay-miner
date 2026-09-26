package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The relay meter warmup runs only when cache warmup is enabled, warms the pairs
// of the suppliers this replica signs for, and runs after the meter starts and
// before the proxy serves. runHARelayer builds a whole process and has no test,
// so the wiring is frozen by reading it.

// meterWarmupViolations reports what is wrong with the meter warmup wiring in f.
// The meter is the variable assigned from NewRelayMeter.
func meterWarmupViolations(f *ast.File, fset *token.FileSet) []string {
	meter := ""
	ast.Inspect(f, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && callsName(call, "NewRelayMeter") {
			if id, ok := assign.Lhs[0].(*ast.Ident); ok {
				meter = id.Name
			}
		}
		return true
	})
	if meter == "" {
		return []string{"no variable is assigned from NewRelayMeter"}
	}

	var warmups []*ast.CallExpr
	guarded := map[*ast.CallExpr]bool{}
	meterStart, proxyStart := token.NoPos, token.NoPos
	ast.Inspect(f, func(n ast.Node) bool {
		if ifStmt, ok := n.(*ast.IfStmt); ok && selectsCacheWarmupEnabled(ifStmt.Cond) {
			ast.Inspect(ifStmt.Body, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok && callsName(call, "WarmFromRedis") {
					guarded[call] = true
				}
				return true
			})
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callsName(call, "WarmFromRedis") {
			warmups = append(warmups, call)
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Start" {
			if id, ok := sel.X.(*ast.Ident); ok {
				switch id.Name {
				case meter:
					meterStart = call.Pos()
				case "proxy":
					proxyStart = call.Pos()
				}
			}
		}
		return true
	})

	if len(warmups) != 1 {
		return []string{"WarmFromRedis must be called exactly once"}
	}
	call := warmups[0]
	at := fset.Position(call.Pos()).String()

	var out []string
	if !guarded[call] {
		out = append(out, "WarmFromRedis at "+at+" is not guarded by config.CacheWarmup.Enabled")
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || !isIdentNamed(sel.X, meter) {
		out = append(out, "WarmFromRedis at "+at+" is not called on the relay meter "+meter)
	}
	if len(call.Args) != 2 || !selectsField(call.Args[1], "HasSigner") {
		out = append(out, "WarmFromRedis at "+at+" must be given the response signer's HasSigner")
	}
	if meterStart == token.NoPos || call.Pos() < meterStart {
		out = append(out, "WarmFromRedis at "+at+" runs before the relay meter starts")
	}
	if proxyStart == token.NoPos || call.Pos() > proxyStart {
		out = append(out, "WarmFromRedis at "+at+" runs after the proxy starts")
	}
	return out
}

func selectsCacheWarmupEnabled(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Enabled" && selectsField(sel.X, "CacheWarmup")
}

func selectsField(e ast.Expr, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}

func isIdentNamed(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func TestTheRelayerWarmsItsMeterOnlyWhenCacheWarmupIsEnabled(t *testing.T) {
	files, fset := goFiles(t, false)

	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}
	for _, v := range meterWarmupViolations(f, fset) {
		t.Errorf("%s: %s", path, v)
	}
}

// TestMeterWarmupViolationsReadsTheShape proves the rule reads the guard, the
// receiver, the argument and the order, not merely the name.
func TestMeterWarmupViolationsReadsTheShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"wired", `
	relayMeter := relayer.NewRelayMeter()
	relayMeter.Start(ctx)
	if config.CacheWarmup.Enabled {
		relayMeter.WarmFromRedis(ctx, responseSigner.HasSigner)
	}
	proxy.Start(ctx)`, 0},
		{"unguarded", `
	relayMeter := relayer.NewRelayMeter()
	relayMeter.Start(ctx)
	relayMeter.WarmFromRedis(ctx, responseSigner.HasSigner)
	proxy.Start(ctx)`, 1},
		{"guarded by another switch", `
	relayMeter := relayer.NewRelayMeter()
	relayMeter.Start(ctx)
	if config.HealthCheck.Enabled {
		relayMeter.WarmFromRedis(ctx, responseSigner.HasSigner)
	}
	proxy.Start(ctx)`, 1},
		{"every supplier", `
	relayMeter := relayer.NewRelayMeter()
	relayMeter.Start(ctx)
	if config.CacheWarmup.Enabled {
		relayMeter.WarmFromRedis(ctx, func(string) bool { return true })
	}
	proxy.Start(ctx)`, 1},
		{"on another receiver", `
	relayMeter := relayer.NewRelayMeter()
	relayMeter.Start(ctx)
	if config.CacheWarmup.Enabled {
		other.WarmFromRedis(ctx, responseSigner.HasSigner)
	}
	proxy.Start(ctx)`, 1},
		{"before the meter starts", `
	relayMeter := relayer.NewRelayMeter()
	if config.CacheWarmup.Enabled {
		relayMeter.WarmFromRedis(ctx, responseSigner.HasSigner)
	}
	relayMeter.Start(ctx)
	proxy.Start(ctx)`, 1},
		{"after the proxy starts", `
	relayMeter := relayer.NewRelayMeter()
	relayMeter.Start(ctx)
	proxy.Start(ctx)
	if config.CacheWarmup.Enabled {
		relayMeter.WarmFromRedis(ctx, responseSigner.HasSigner)
	}`, 1},
		{"twice", `
	relayMeter := relayer.NewRelayMeter()
	relayMeter.Start(ctx)
	if config.CacheWarmup.Enabled {
		relayMeter.WarmFromRedis(ctx, responseSigner.HasSigner)
		relayMeter.WarmFromRedis(ctx, responseSigner.HasSigner)
	}
	proxy.Start(ctx)`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fset := parseSource(t, "package x\n\nfunc run() {"+tc.body+"\n}\n")
			got := meterWarmupViolations(f, fset)
			if len(got) != tc.want {
				t.Fatalf("want %d violations, got %d: %q", tc.want, len(got), got)
			}
		})
	}
}
