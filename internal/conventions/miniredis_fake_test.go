package conventions

import (
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// miniredis is not Redis where the interesting behaviour lives. It answers a
// blocking XREADGROUP immediately rather than blocking, it does not age PEL
// entries, and its expiry and eviction are approximations.
//
// That is not a style preference. The stream consumer parked on XREADGROUP
// BLOCK 0 and could not be shut down; the whole suite stayed green because the
// fake never blocked, and the bug reached production. A test asserting on those
// semantics against miniredis is not evidence, and the ones that do not assert
// on them still teach the next author that the fake is the house style.
//
// internal/testredis hands tests a real Redis 8; scripts/gates/redis.sh starts
// one for the whole run. The files below still use the fake and are FROZEN at
// 2026-08-19: this check only stops new ones from landing, and forces the list
// to shrink as packages are migrated. transport/redis was migrated with this
// check and are deliberately absent, as are cache and relayer.
var miniredisAllowlist = map[string]bool{
	"client/relay_client/simulated_test.go":              true,
	"cmd/redis/cache_all_test.go":                        true,
	"cmd/redis/cache_test.go":                            true,
	"leader/redis_health_test.go":                        true,
	"miner/deduplicator_test.go":                         true,
	"miner/economic_viability_test.go":                   true,
	"miner/inclusion_reconciler_test.go":                 true,
	"miner/pre_proof_guard_test.go":                      true,
	"miner/rebroadcast_store_test.go":                    true,
	"miner/redis_smst_bench_test.go":                     true,
	"miner/redis_smst_utils_test.go":                     true,
	"miner/session_store_test.go":                        true,
	"miner/smst_defensive_test.go":                       true,
	"miner/smst_migration_test.go":                       true,
	"miner/smst_redis_lifecycle_test.go":                 true,
	"miner/submission_tracker_onchain_test.go":           true,
	"miner/supplier_claimer_test.go":                     true,
	"miner/supplier_manager_history_test.go":             true,
	"miner/supplier_manager_reconcile_test.go":           true,
	"miner/supplier_manager_test.go":                     true,
	"miner/supplier_worker_discovery_test.go":            true,
	"miner/supplier_worker_error_classification_test.go": true,
}

// importsMiniredisFake reports whether a file imports the miniredis package.
func importsMiniredisFake(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		if strings.Contains(strings.Trim(imp.Path.Value, `"`), "alicebob/miniredis") {
			return true
		}
	}
	return false
}

// TestNoNewMiniredis fails on a test file that reaches for the fake without
// being on the frozen list, and on frozen entries that no longer use it.
func TestNoNewMiniredis(t *testing.T) {
	files, _ := goFiles(t, true)

	found := map[string]bool{}
	for path, f := range files {
		if importsMiniredisFake(f) {
			found[path] = true
		}
	}

	var violations, stale []string
	for path := range found {
		if !miniredisAllowlist[path] {
			violations = append(violations, path)
		}
	}
	for path := range miniredisAllowlist {
		if !found[path] {
			stale = append(stale, path+" (migrated — remove it from the allowlist)")
		}
	}
	sort.Strings(violations)
	sort.Strings(stale)
	if len(violations) > 0 {
		t.Errorf("new test files using the miniredis fake (use internal/testredis for a real Redis 8):\n%s",
			joinLines(violations))
	}
	if len(stale) > 0 {
		t.Errorf("stale miniredis allowlist entries:\n%s", joinLines(stale))
	}
}

// TestMiniredisImportMatcherCatchesHostileShapes proves the matcher reads
// IMPORTS, not the word appearing in a comment or a string.
func TestMiniredisImportMatcherCatchesHostileShapes(t *testing.T) {
	usesIt := `package x

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestA(t *testing.T) { _ = miniredis.RunT(t) }
`
	mentionsIt := `package x

import "testing"

// miniredis is not used here, only named: "github.com/alicebob/miniredis/v2"
func TestB(t *testing.T) { s := "alicebob/miniredis"; _ = s; _ = t }
`
	f, _ := parseSource(t, usesIt)
	if !importsMiniredisFake(f) {
		t.Fatal("matcher missed a real import")
	}
	f, _ = parseSource(t, mentionsIt)
	if importsMiniredisFake(f) {
		t.Fatal("matcher fired on a mention rather than an import")
	}
}
