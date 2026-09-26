package conventions

import (
	"fmt"
	"go/ast"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A test that assigns a package-level var declared in production code makes
// that var process-wide mutable state, and nothing orders its write against a
// read happening on a goroutine the test does not join.
//
// Measured 2026-09-20 (item 390). `relayer/websocket_lifecycle_test.go:191`
// shortens `wsFirstFrameWait`; `NewWebSocketBridge` reads it at
// `relayer/websocket.go:399` from the HTTP handler goroutine of ANOTHER test's
// httptest.Server. `-race -count=5 ./relayer/` reports it about once in five.
//
// Three things make this worth a guard rather than a rule in prose:
//
//   - The repository's own prescribed fix was already applied and did not work.
//     `websocket.go:241-244` captures the var into a struct field at
//     construction "because the constructor runs on the caller's goroutine" --
//     true only while the caller is the test. When it is a leftover handler,
//     capturing changes where the copy lives, not when it is ordered.
//   - The comment authorising one of these mutations argues from a true premise
//     to a false conclusion: `websocket_readlimit_test.go:20-21` says it is safe
//     because no test calls t.Parallel(). Tests do not overlap; what overlaps is
//     a goroutine of a test that already finished.
//   - The failure is intermittent, so review does not catch it and a green run
//     does not clear it. Only a deterministic check does.
//
// This freezes what existed and fails on anything new, like
// bareGoroutineAllowlist and the miniredis allowlist above it.
//
// NOTE for whoever is tempted to silence a violation by making the var atomic:
// that is why atomic stores count here too. Under sync/atomic the race detector
// goes quiet while the value still bleeds between tests -- the reader keeps
// getting another test's setting -- so a check that only looked at plain
// assignment would approve exactly the change that fixes nothing.
var testPackageVarAllowlist = map[string]int{
	"cache: accountCacheL1TTL":          3,
	"cache: applicationCacheL1TTL":      3,
	"cache: serviceCacheL1TTL":          3,
	"cache: sessionCacheL1TTL":          3,
	"cache: supplierCacheL1TTL":         3,
	"cache: sharedParamsLocalTTL":       2,
	"cmd/redis: RedisBasePrefix":        5,
	"cmd/redis: RedisConfig":            5,
	"cmd/redis: RedisURL":               2,
	"cmd/redis: publishClearAll":        4,
	"cmd/relay: RelayAllSuppliers":      2,
	"cmd/relay: RelayAppKeyName":        1,
	"cmd/relay: RelayAppPrivKey":        7,
	"cmd/relay: RelayConcurrency":       2,
	"cmd/relay: RelayCount":             2,
	"cmd/relay: RelayGRPCMethod":        4,
	"cmd/relay: RelayGRPCRequestHex":    4,
	"cmd/relay: RelayGatewayPrivKey":    5,
	"cmd/relay: RelayKeysFile":          3,
	"cmd/relay: RelayPayloadJSON":       8,
	"cmd/relay: RelayRPS":               2,
	"cmd/relay: RelayRelayerURL":        12,
	"cmd/relay: RelayServiceID":         12,
	"cmd/relay: RelaySimAppPubKey":      4,
	"cmd/relay: RelaySimGatewayPubKeys": 4,
	"cmd/relay: RelaySimKeyID":          12,
	"cmd/relay: RelaySimulate":          15,
	"cmd/relay: RelaySupplierAddr":      9,
	"cmd/relay: RelayTimeout":           10,
	"miner: flushTreeSealWaitHook":      1,
	"miner: hostnameFn":                 2,
	"miner: processIdentityFellBack":    2,
	"miner: processIdentityOnce":        2,
	"miner: processIdentityValue":       2,
	"miner: shutdownDrainWindow":        2,
	"query: immutableCacheTTLFloor":     6,
	"query: liveEntityCacheTTL":         9,
	"query: liveParamsCacheTTL":         13,
	"query: serviceCacheTTL":            3,
	"relayer: wsFirstFrameWait":         4,
	"relayer: wsMaxMessageBytes":        2,
	"rings: ringPointsCacheTTL":         2,
	"transport/redis: blockInterval":    8,
}

// productionPackageVars maps "<package dir>: <name>" for every package-level
// var declared OUTSIDE a test file. A var declared in a _test.go file is the
// test's own and is not shared with production code.
func productionPackageVars(t *testing.T) map[string]bool {
	t.Helper()
	files, _ := goFiles(t, false)

	vars := map[string]bool{}
	for path, f := range files {
		dir := filepath.ToSlash(filepath.Dir(path))
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok.String() != "var" {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					if name.Name != "_" {
						vars[dir+": "+name.Name] = true
					}
				}
			}
		}
	}
	return vars
}

// TestNoTestMutatesAProductionPackageVar fails on any test assignment to a
// package var declared in production code that is not frozen above, and on
// frozen entries that no longer match, so the list shrinks as they are fixed
// instead of rotting.
func TestNoTestMutatesAProductionPackageVar(t *testing.T) {
	production := productionPackageVars(t)
	files, _ := goFiles(t, true)

	found := map[string]int{}
	for path, f := range files {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(path))

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				// Only plain assignment: `:=` declares a new local that
				// shadows, which is not a mutation of the package var.
				if node.Tok.String() != "=" {
					return true
				}
				for _, lhs := range node.Lhs {
					ident, ok := lhs.(*ast.Ident)
					if !ok {
						continue
					}
					if key := dir + ": " + ident.Name; production[key] {
						found[key]++
					}
				}
			case *ast.CallExpr:
				// `v.Store(x)` on one of these vars is a mutation too. See the
				// NOTE on the allowlist: without this, converting the var to an
				// atomic would satisfy the check while changing nothing.
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Store" {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if key := dir + ": " + ident.Name; production[key] {
					found[key]++
				}
			}
			return true
		})
	}

	var violations []string
	for key, n := range found {
		if frozen := testPackageVarAllowlist[key]; n > frozen {
			violations = append(violations, fmt.Sprintf("%s (%d assignments in tests, %d frozen)", key, n, frozen))
		}
	}
	var stale []string
	for key, frozen := range testPackageVarAllowlist {
		if found[key] < frozen {
			stale = append(stale, fmt.Sprintf("%s (frozen %d, found %d)", key, frozen, found[key]))
		}
	}
	sort.Strings(violations)
	sort.Strings(stale)

	if len(violations) > 0 {
		t.Errorf("a test mutates a package var declared in production code:\n  %s\n\n"+
			"Nothing orders that write against a read on a goroutine the test does not join -- "+
			"see item 390, where exactly this pattern races through an httptest.Server handler "+
			"that outlived its own test. Pass the value in, or have the test join whatever reads it.",
			strings.Join(violations, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("allowlist entries that no longer match -- lower or remove them:\n  %s",
			strings.Join(stale, "\n  "))
	}
}
