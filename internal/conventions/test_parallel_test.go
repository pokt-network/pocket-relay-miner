package conventions

import (
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// Three packages mutate PROCESS-WIDE state inside their tests, so two of their
// tests running at once read each other's writes:
//
//   - cache reassigns the L1 TTL globals (serviceCacheL1TTL and its four
//     siblings) and restores them in t.Cleanup;
//   - cache, miner and relayer all read Prometheus counters through
//     testutil.ToFloat64 as before/after deltas;
//   - the miner testify suites clear their whole suite-wide key prefix in
//     SetupTest, which would delete a sibling test's keys mid-run.
//
// scripts/gates/lib.sh pins `-p 1 -parallel 1` for these packages, but ONLY
// when PKG names one of them: the whole-tree run that every gate and CI
// actually perform falls through to -parallel 4. So the flag cannot be the
// guard, and this check is. t.Parallel() is what would make those mutations
// concurrent, and none of these packages has one today.
var noParallelPackages = map[string]string{
	"cache":   "reassigns L1 TTL globals and reads Prometheus counters as before/after deltas",
	"miner":   "reads Prometheus counters as deltas; the testify suites clear a suite-wide key prefix in SetupTest",
	"relayer": "reads Prometheus counters as before/after deltas",
}

// callsTestParallel reports whether a file calls t.Parallel() (or b.Parallel(),
// or the same through a testify suite's T()).
func callsTestParallel(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Parallel" || len(call.Args) != 0 {
			return true
		}
		found = true
		return false
	})
	return found
}

// packageOf returns the top-level directory of a repo-relative path.
func packageOf(path string) string {
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i]
	}
	return ""
}

// TestNoTestParallelWhereStateIsShared fails on a t.Parallel() in a package
// whose tests share process-wide state.
//
// The gate flags are not enough on their own -- they only apply to a run that
// narrows to one of these packages -- so the guard has to live where the
// hazard is introduced. If a package genuinely stops sharing state, remove it
// from the map above rather than working around this check.
func TestNoTestParallelWhereStateIsShared(t *testing.T) {
	files, _ := goFiles(t, true)

	var violations []string
	for path, f := range files {
		reason, guarded := noParallelPackages[packageOf(path)]
		if !guarded || !callsTestParallel(f) {
			continue
		}
		violations = append(violations, path+" ("+reason+")")
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("t.Parallel() in a package whose tests share process-wide state:\n%s",
			joinLines(violations))
	}
}

// TestParallelMatcherCatchesHostileShapes proves the matcher reads a CALL, not
// the word appearing in a name or a comment.
func TestParallelMatcherCatchesHostileShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "plain t.Parallel()",
			src: `package x
import "testing"
func TestA(t *testing.T) { t.Parallel() }`,
			want: true,
		},
		{
			name: "b.Parallel() in a benchmark",
			src: `package x
import "testing"
func BenchmarkA(b *testing.B) { b.Parallel() }`,
			want: true,
		},
		{
			name: "through a suite's T()",
			src: `package x
func (s *S) TestA() { s.T().Parallel() }`,
			want: true,
		},
		{
			name: "the word in a comment is not a call",
			src: `package x
import "testing"
// t.Parallel() would be wrong here.
func TestA(t *testing.T) { _ = t }`,
			want: false,
		},
		{
			name: "a Parallel identifier that is not a call",
			src: `package x
type cfg struct{ Parallel int }
func f() int { c := cfg{}; return c.Parallel }`,
			want: false,
		},
		{
			name: "RunParallel is a different method",
			src: `package x
import "testing"
func BenchmarkA(b *testing.B) { b.RunParallel(nil) }`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := parseSource(t, tc.src)
			if got := callsTestParallel(f); got != tc.want {
				t.Errorf("callsTestParallel = %v, want %v", got, tc.want)
			}
		})
	}
}
