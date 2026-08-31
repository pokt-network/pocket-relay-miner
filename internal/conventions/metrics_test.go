package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"sort"
	"strings"
	"testing"
)

// CLAUDE.md metrics rules: no high-cardinality labels (session IDs are
// unbounded), and no dead declarations (a metric nobody writes is a dashboard
// panel that is always empty — a lie with a name).

// sessionIDLabelAllowlist freezes the metric variables allowed to carry a
// session_id label, keyed "file: varName". The four existing ones are Gauges
// whose series are deleted at session end (DeleteLabelValues — bounded by
// ACTIVE sessions, documented in miner/metrics.go). A session_id label on a
// Counter is forbidden with no exemption: Counter series are never deleted,
// so they grow one per session forever until the TSDB OOMs.
var sessionIDLabelAllowlist = map[string]bool{
	"miner/metrics.go: claimNumLeaves":       true,
	"miner/metrics.go: claimRelayAttempts":   true,
	"miner/metrics.go: claimScheduledHeight": true,
	"miner/metrics.go: proofScheduledHeight": true,
}

// sessionIDLabelVars returns "varName" for package-level vars whose declared
// label list ([]string{...}) contains "session_id".
func sessionIDLabelVars(f *ast.File) []string {
	var hits []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			has := false
			for _, v := range vs.Values {
				ast.Inspect(v, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok {
						return true
					}
					arr, ok := lit.Type.(*ast.ArrayType)
					if !ok {
						return true
					}
					if ident, ok := arr.Elt.(*ast.Ident); !ok || ident.Name != "string" {
						return true
					}
					for _, elt := range lit.Elts {
						if bl, ok := elt.(*ast.BasicLit); ok && strings.Trim(bl.Value, `"`) == "session_id" {
							has = true
						}
					}
					return true
				})
			}
			if has {
				for _, name := range vs.Names {
					hits = append(hits, name.Name)
				}
			}
		}
	}
	return hits
}

// TestSessionIDLabelsAreFrozen fails on any new metric declaring a
// session_id label, and on stale allowlist entries.
func TestSessionIDLabelsAreFrozen(t *testing.T) {
	files, _ := goFiles(t, false)

	found := map[string]bool{}
	for p, f := range files {
		for _, v := range sessionIDLabelVars(f) {
			found[p+": "+v] = true
		}
	}

	var violations, stale []string
	for key := range found {
		if !sessionIDLabelAllowlist[key] {
			violations = append(violations, key)
		}
	}
	for key := range sessionIDLabelAllowlist {
		if !found[key] {
			stale = append(stale, key+" (no longer declares the label — shrink the allowlist)")
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("new session_id metric labels (unbounded cardinality — put the session in logs, not labels):\n%s", joinLines(violations))
	}
	if len(stale) > 0 {
		t.Errorf("stale session_id allowlist entries:\n%s", joinLines(stale))
	}
}

// metricFactoryMethods are the constructor names whose package-level results
// count as metric declarations.
var metricFactoryMethods = map[string]bool{
	"NewCounter": true, "NewCounterVec": true,
	"NewGauge": true, "NewGaugeVec": true,
	"NewHistogram": true, "NewHistogramVec": true,
	"NewSummary": true, "NewSummaryVec": true,
}

// metricVarsIn returns names of package-level vars initialized by a metric
// factory call.
func metricVarsIn(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) != len(vs.Names) {
				continue
			}
			for i, v := range vs.Values {
				call, ok := v.(*ast.CallExpr)
				if !ok {
					continue
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !metricFactoryMethods[sel.Sel.Name] {
					continue
				}
				names = append(names, vs.Names[i].Name)
			}
		}
	}
	return names
}

// identUses counts references to a name in a file, excluding declarations.
func identUses(f *ast.File, name string) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		// Skip the declaring ValueSpec names themselves.
		if vs, ok := node.(*ast.ValueSpec); ok {
			for _, id := range vs.Names {
				if id.Name == name {
					// walk only the values, not the names
					for _, v := range vs.Values {
						ast.Inspect(v, func(inner ast.Node) bool {
							if id2, ok := inner.(*ast.Ident); ok && id2.Name == name {
								n++
							}
							return true
						})
					}
					return false
				}
			}
			return true
		}
		if id, ok := node.(*ast.Ident); ok && id.Name == name {
			n++
		}
		return true
	})
	return n
}

// metricVarFloor is the number of metric variable declarations when this
// floor was set. Measured 2026-08-19: ~180.
const metricVarFloor = 120

// TestEveryDeclaredMetricIsReferenced fails when a metric variable has no
// reference in its package's production code beyond its declaration: nothing
// writes it, so its series either never appears or (if pre-seeded) lies as a
// permanent zero.
func TestEveryDeclaredMetricIsReferenced(t *testing.T) {
	files, _ := goFiles(t, false)

	// Group files by directory (package).
	byDir := map[string]map[string]*ast.File{}
	for p, f := range files {
		dir := path.Dir(p)
		if byDir[dir] == nil {
			byDir[dir] = map[string]*ast.File{}
		}
		byDir[dir][p] = f
	}

	total := 0
	var violations []string
	for dir, pkgFiles := range byDir {
		// Collect metric vars declared anywhere in the package.
		type declared struct{ file, name string }
		var decls []declared
		for p, f := range pkgFiles {
			for _, name := range metricVarsIn(f) {
				decls = append(decls, declared{p, name})
			}
		}
		total += len(decls)
		for _, d := range decls {
			// Exported metrics (observability's SMST* family) are written
			// from other packages, so count references repo-wide. Metric
			// names are distinctive; a cross-package name collision could
			// only hide a violation, never invent one.
			uses := 0
			for _, f := range files {
				uses += identUses(f, d.name)
			}
			if uses == 0 {
				violations = append(violations, fmt.Sprintf("%s: %s (declared but never referenced anywhere — package %s)", d.file, d.name, dir))
			}
		}
	}
	if total < metricVarFloor {
		t.Fatalf("found only %d metric declarations (floor %d) — the scan is broken, not the tree clean", total, metricVarFloor)
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("metrics declared but never referenced (delete them or wire a writer):\n%s", joinLines(violations))
	}
}

// TestMetricMatchersCatchHostileShapes proves the declaration and label
// matchers can fail.
func TestMetricMatchersCatchHostileShapes(t *testing.T) {
	hostile := `package x
var used = factory.NewCounterVec(opts, []string{"service_id"})
var dead = factory.NewGaugeVec(opts, []string{"supplier", "session_id"})
var notAMetric = other.NewThing()
func f() { used.Inc() }
`
	f, _ := parseSource(t, hostile)

	vars := metricVarsIn(f)
	if len(vars) != 2 {
		t.Fatalf("metric var matcher found %d, want 2 (used, dead): %v", len(vars), vars)
	}
	if got := identUses(f, "used"); got != 1 {
		t.Fatalf("identUses(used) = %d, want 1", got)
	}
	if got := identUses(f, "dead"); got != 0 {
		t.Fatalf("identUses(dead) = %d, want 0", got)
	}
	labels := sessionIDLabelVars(f)
	if len(labels) != 1 || labels[0] != "dead" {
		t.Fatalf("session_id label matcher found %v, want [dead]", labels)
	}
}
