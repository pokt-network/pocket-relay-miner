package conventions

import (
	"go/ast"
	"path/filepath"
	"sort"
	"testing"
)

// time.Unix, time.UnixMilli and time.UnixMicro build an instant that carries NO
// monotonic reading. Every Sub, Since or comparison against a later time.Now()
// on such an instant therefore measures the WALL clock, and a wall clock that
// steps -- NTP correcting a cold boot, a VM resuming, a container starting
// before the host has synced -- moves the result by the size of the step with
// nothing wrong in the system being measured.
//
// Measured 2026-09-20 (item 388). The relayer's admission read the batch
// dispatcher's progress through a mark kept as an int64 of UnixNano and rebuilt
// with time.Unix(0, n). A +3.34s wall-clock step at startup, with Redis healthy
// and the dispatcher writing, refused 1203 relays as "metering unavailable".
// The fix keeps the instant as an instant and never crosses that boundary.
//
// This freezes what the tree had when the rule landed and fails on anything
// new, like bareGoroutineAllowlist and testPackageVarAllowlist. It is by
// PATTERN and not by wiring on purpose: a guard written against the one call
// site that caused the incident passes while the same round trip sits intact in
// eight other files.
//
// A new call is not automatically wrong. Formatting a persisted timestamp for a
// human, or filling a proto field, is exactly what these functions are for.
// What is wrong is SUBTRACTING from one. So, to add an entry: say in one line
// why the instant is never subtracted from -- and if it is, keep the instant.
var instantFromIntAllowlist = map[string]int{
	// Printing a stored submission timestamp to the operator's terminal.
	"cmd/redis/submissions.go": 2,
	// Formatting a supplier state's LastUpdated for display.
	"cmd/redis/supplier.go": 3,
	// Filling a struct field that carries a deadline already agreed as wall time.
	"miner/inclusion_reconciler.go": 1,
	// SUBTRACTS: the same class as item 388, on the gRPC stats path.
	"observability/grpcstats.go": 1,
	// SUBTRACTS: endpoint recovery measured from an int64 of UnixNano.
	"pool/endpoint.go": 1,
	// SUBTRACTS: downtime reported from the same int64.
	"pool/pool.go": 2,
	// Rebuilds the publish instant of a message that crossed Redis, where the
	// wire carries nanoseconds and no monotonic reading can survive the hop.
	"transport/types.go": 1,
	// SUBTRACTS: seconds since the last probe.
	"tx/conn_probe.go": 1,
}

// instantFromIntCalls counts calls that build a time.Time out of an integer.
func instantFromIntCalls(f *ast.File) int {
	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "time" {
			return true
		}
		switch sel.Sel.Name {
		case "Unix", "UnixMilli", "UnixMicro":
			found++
		}
		return true
	})
	return found
}

// TestNoNewInstantIsBuiltFromAnInteger fails on any production call that is not
// frozen above, and on frozen entries whose count no longer matches, so the list
// shrinks as they are fixed instead of rotting.
func TestNoNewInstantIsBuiltFromAnInteger(t *testing.T) {
	files, _ := goFiles(t, false)

	found := map[string]int{}
	for path, f := range files {
		if n := instantFromIntCalls(f); n > 0 {
			found[filepath.ToSlash(path)] = n
		}
	}

	var paths []string
	for path := range found {
		paths = append(paths, path)
	}
	for path := range instantFromIntAllowlist {
		if _, seen := found[path]; !seen {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	for _, path := range paths {
		got, want := found[path], instantFromIntAllowlist[path]
		switch {
		case want == 0:
			t.Errorf("%s: %d new instant(s) built from an integer. An instant rebuilt with "+
				"time.Unix carries no monotonic reading, so subtracting from it measures the wall "+
				"clock and a clock step alone changes the answer (item 388). Keep the instant, or "+
				"freeze this file with a line saying why it is never subtracted from", path, got)
		case got == 0:
			t.Errorf("%s: frozen with %d call(s) and has none left — remove it from "+
				"instantFromIntAllowlist so the list keeps shrinking", path, want)
		case got != want:
			t.Errorf("%s: frozen at %d call(s), found %d — update the entry, or keep the instant "+
				"instead of rebuilding it from an integer", path, want, got)
		}
	}
}

// TestInstantFromIntCallsReadsTheShape proves the matcher reads the call and not
// the word: it counts every constructor, ignores a method of the same name on an
// instant, and ignores another package's Unix.
func TestInstantFromIntCallsReadsTheShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"the class itself", `func f(n int64) { _ = time.Since(time.Unix(0, n)) }`, 1},
		{"milli and micro count too", `func f(n int64) { _, _ = time.UnixMilli(n), time.UnixMicro(n) }`, 2},
		{"a method on an instant is the other direction", `func f(t time.Time) int64 { return t.UnixNano() }`, 0},
		{"another package's Unix is not this one", `func f(n int64) { _ = syscall.Unix(n) }`, 0},
		{"keeping the instant is the fix", `func f(at time.Time) { _ = time.Since(at) }`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := parseSource(t, "package x\n\n"+tc.body+"\n")
			if got := instantFromIntCalls(f); got != tc.want {
				t.Fatalf("want %d, got %d", tc.want, got)
			}
		})
	}
}
