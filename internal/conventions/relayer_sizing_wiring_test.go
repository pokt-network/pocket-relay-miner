package conventions

import (
	"go/ast"
	"testing"
)

// The relayer's startup wiring has no unit test -- runRelayer is one long
// function that builds a whole process -- so the three decisions this file
// pins would otherwise be provable only by reading. Each of them was WRONG in
// production until 2026-09-12, and each failed silently:
//
//   - the worker count came from runtime.NumCPU(), which ignores the
//     container's CPU limit, so a pod capped at 2 cores on an 18-core node
//     built 144 workers. On a developer laptop NumCPU and GOMAXPROCS agree, so
//     no test that merely runs can tell the two apart.
//   - the Redis pool came from a literal, not from those workers, so the two
//     numbers describing one concurrency drifted apart.
//   - nothing measured how long a Redis command took, which is why a relayer
//     queueing thousands of callers looked identical to an idle one.
//
// These are wiring assertions, in the shape this suite already uses for
// TestMissingCauseDeciderStaysWired: replacing one call at the startup site
// leaves every package green, and this is what goes red instead.

// callsFunc reports whether a file contains a call to a function by name,
// either bare (f) or qualified (pkg.f).
func callsFunc(f *ast.File, name string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == name {
				found = true
			}
		case *ast.SelectorExpr:
			if fn.Sel != nil && fn.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// mustCall asserts that path calls name, and that path still exists: a renamed
// file would otherwise retire the rule silently, which looks exactly like the
// rule passing.
func mustCall(t *testing.T, files map[string]*ast.File, path, name, why string) {
	t.Helper()
	f, ok := files[path]
	if !ok {
		t.Errorf("%s not found: if it moved, point this rule at its new path", path)
		return
	}
	if !callsFunc(f, name) {
		t.Errorf("%s no longer calls %s.\n%s", path, name, why)
	}
}

func TestRelayerSizesItselfFromGOMAXPROCS(t *testing.T) {
	files, _ := goFiles(t, false)

	mustCall(t, files, "cmd/cmd_relayer.go", "ComputeWorkerSizingForProcess",
		"  The worker count and the Redis pool both come from it. Computing either one\n"+
			"  another way -- runtime.NumCPU()*8 is what used to be here -- sizes this\n"+
			"  process for the NODE's cores instead of the container's CPU limit, and\n"+
			"  decouples the pool from the workers that use it. Neither shows up on a\n"+
			"  machine whose limit equals its core count, which is every laptop and the\n"+
			"  CI box: the divergence only appears in a limited pod, in production.")

	mustCall(t, files, "cmd/cmd_relayer.go", "EffectivePoolOptions",
		"  The startup guard and the pool gauges must read what the CLIENT holds. Reading\n"+
			"  the config instead certifies the request: pool_timeout_seconds is unset in\n"+
			"  every deployment, so a config-sourced gauge publishes 0, and a zero pool\n"+
			"  timeout reads as 'waits forever' when the real deadline is seconds.")
}

func TestBothBinariesMeasureRedisCommandLatency(t *testing.T) {
	files, _ := goFiles(t, false)

	why := "  Without it there is no unbiased measure of how long a Redis command takes.\n" +
		"  The pool's own wait series cannot substitute: they count only waits that ended\n" +
		"  in a connection, so their mean improves as the pool starts timing out.\n" +
		"  Registered in ONE binary and not the other, the missing one reports nothing --\n" +
		"  which is indistinguishable from reporting that nothing waited."

	mustCall(t, files, "cmd/cmd_relayer.go", "NewCommandLatencyHook", why)
	mustCall(t, files, "cmd/cmd_miner.go", "NewCommandLatencyHook", why)
}

// TestCallsFuncMatcherCatchesHostileShapes proves the matcher reads CALLS, not
// the name appearing in a comment or a string.
func TestCallsFuncMatcherCatchesHostileShapes(t *testing.T) {
	callsIt := `package x

func target() int { return 1 }
func a() int      { return target() }
`
	qualified := `package x

import "p"

func b() { p.target() }
`
	mentionsIt := `package x

// target() is only named here
var s = "target()"
`
	for _, src := range []string{callsIt, qualified} {
		f, _ := parseSource(t, src)
		if !callsFunc(f, "target") {
			t.Fatalf("matcher missed a real call in:\n%s", src)
		}
	}
	f, _ := parseSource(t, mentionsIt)
	if callsFunc(f, "target") {
		t.Fatal("matcher fired on a mention in a comment and a string")
	}
}
