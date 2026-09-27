package conventions

import (
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// maxmemory, maxmemory-policy, FLUSHALL and CLIENT KILL are configuration of
// the SERVER, not of a key or a database. internal/testredis isolates tests
// from each other BY KEY PREFIX, which cannot contain any of them: while one
// test holds a lowered maxmemory, every other package writing to that server
// gets "OOM command not allowed", and the failure is reported against whatever
// code happened to be writing.
//
// Measured 2026-09-18: TestSubmissionTracker_WritesEveryKindWhileTheStoreIsClosed
// held the shared server for 133 ms, and TestClaimPendingMessages_DrainsWholePEL
// -- a different package -- started inside that window and died of OOM, taking
// the gate with it. Two of three runs that day passed on timing alone, which is
// worse than a steady red: it makes green cheap.
//
// A test that must configure the server asks for one of its own with
// testredis.Exclusive / testredis.ExclusiveURL. The allowlist below is
// therefore NOT "these may use the shared server": every entry must ALSO name
// the exclusive helper, and this check enforces that. An entry that stops
// configuring the server must leave the list.
var serverConfigAllowlist = map[string]string{
	"internal/testredis/exclusive_test.go":       "tests the exclusive helper itself, including that its maxmemory does not reach the shared server",
	"miner/submission_tracker_brake_test.go":     "closes the store by reserve, and asserts the OOM reply reaches the caller labelled oom",
	"miner/smst_cold_compaction_measure_test.go": "asserts Redis refuses the leaf blob with a real OOM and keeps the node hash",
	"transport/redis/store_full_backlog_test.go": "needs a real used/maxmemory sample of a store full of stream backlog",
	"transport/redis/batching_oom_test.go":       "asserts a real MULTI refused for memory aborts and spends no attempt",
}

// serverConfigCalls are method names that belong to a Redis client and to
// nothing else in this tree. Do returns a generic command, so a CONFIG issued
// through it is matched by its arguments instead.
//
// WHAT THIS DELIBERATELY DOES NOT MATCH, and why: FlushAll, FlushDB and
// Shutdown are equally server-wide on Redis, but those names are taken here --
// the miner's relay batch has its own FlushAll, and eight test files call it
// (miner/relay_batch_test.go, miner/smst_batch_commit_test.go and six
// siblings). Matching by name flagged all eight, which is a check that cries
// wolf until someone silences it. Telling them apart needs the receiver's
// TYPE, which this package does not resolve.
//
// The gap is real and stated rather than hidden: a test that calls FLUSHALL on
// the shared server would pass this check. Nothing in the tree does today
// (verified: every FlushAll call site is the relay batch), and
// internal/testredis already forbids it in prose with DeletePrefix as the
// replacement. If one ever appears, this check needs a type-aware matcher, not
// a wider list of names.
var serverConfigCalls = map[string]bool{
	"ConfigSet":  true,
	"ClientKill": true,
}

// configuresServer reports whether the file reconfigures the server, reading
// the AST rather than the text: "maxmemory" appears in thirteen test files and
// only five of them write it -- the rest name it inside an error string.
func configuresServer(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if serverConfigCalls[sel.Sel.Name] {
			found = true
			return false
		}
		// Do(ctx, "config", "set", ...) reaches the same setting without
		// naming the method, so the string arguments decide.
		if sel.Sel.Name == "Do" {
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok {
					continue
				}
				if strings.EqualFold(strings.Trim(lit.Value, `"`), "config") {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// namesExclusiveHelper reports whether the file asks for a Redis of its own.
// It matches the helper by NAME rather than by import, because miner reaches
// it through its own newExclusiveTestRedis wrapper.
func namesExclusiveHelper(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if strings.Contains(ident.Name, "Exclusive") {
			found = true
			return false
		}
		return true
	})
	return found
}

// classifyServerConfig splits the walked files into the three ways this rule
// can be broken. It is separate from the walk so each way can be tested on its
// own: the tree happens to contain no allowlisted file that skips the helper,
// so driving that branch through the walk is impossible, and a branch no test
// reaches is a branch that can be deleted without anything going red.
func classifyServerConfig(configuring, exclusive map[string]bool, allowlist map[string]string) (violations, stale, unprotected []string) {
	for path := range configuring {
		if _, ok := allowlist[path]; !ok {
			violations = append(violations, path)
			continue
		}
		if !exclusive[path] {
			unprotected = append(unprotected, path+" (allowlisted but never asks for its own Redis)")
		}
	}
	for path := range allowlist {
		if !configuring[path] {
			stale = append(stale, path+" (no longer configures the server -- remove it from the allowlist)")
		}
	}
	sort.Strings(violations)
	sort.Strings(stale)
	sort.Strings(unprotected)
	return violations, stale, unprotected
}

// TestServerConfigClassifierNamesEachWayTheRuleBreaks drives the three
// branches the tree cannot produce on its own.
func TestServerConfigClassifierNamesEachWayTheRuleBreaks(t *testing.T) {
	allowlist := map[string]string{
		"allowed/protected_test.go":   "configures the server and asks for its own",
		"allowed/unprotected_test.go": "configures the server but forgot to",
		"allowed/retired_test.go":     "stopped configuring anything",
	}
	configuring := map[string]bool{
		"allowed/protected_test.go":   true,
		"allowed/unprotected_test.go": true,
		"stranger/rogue_test.go":      true,
	}
	exclusive := map[string]bool{
		"allowed/protected_test.go": true,
	}

	violations, stale, unprotected := classifyServerConfig(configuring, exclusive, allowlist)

	if len(violations) != 1 || violations[0] != "stranger/rogue_test.go" {
		t.Errorf("a file configuring the shared server off the allowlist must be a violation, got %v", violations)
	}
	if len(stale) != 1 || !strings.HasPrefix(stale[0], "allowed/retired_test.go") {
		t.Errorf("an allowlist entry that no longer configures anything must be stale, got %v", stale)
	}
	if len(unprotected) != 1 || !strings.HasPrefix(unprotected[0], "allowed/unprotected_test.go") {
		t.Errorf("an allowlisted file that never asks for its own Redis must be reported, got %v", unprotected)
	}
}

// TestNoServerConfigAgainstTheSharedRedis fails on a test file that configures
// the server without being on the allowlist, on an allowlist entry that no
// longer configures anything, and on an allowlist entry that configures the
// server WITHOUT asking for one of its own -- which is the shape the list
// exists to prevent, not to permit.
func TestNoServerConfigAgainstTheSharedRedis(t *testing.T) {
	files, _ := goFiles(t, true)

	configuring := map[string]bool{}
	exclusive := map[string]bool{}
	for path, f := range files {
		if configuresServer(f) {
			configuring[path] = true
		}
		if namesExclusiveHelper(f) {
			exclusive[path] = true
		}
	}

	violations, stale, unprotected := classifyServerConfig(configuring, exclusive, serverConfigAllowlist)

	if len(violations) > 0 {
		t.Errorf("test files configuring the shared Redis server (ask for your own with testredis.Exclusive):\n%s",
			joinLines(violations))
	}
	if len(stale) > 0 {
		t.Errorf("stale server-config allowlist entries:\n%s", joinLines(stale))
	}
	if len(unprotected) > 0 {
		t.Errorf("allowlisted files that configure the server without an exclusive instance:\n%s",
			joinLines(unprotected))
	}
}

// TestServerConfigMatcherCatchesHostileShapes proves the matcher reads CALLS,
// not the word: a file that only names maxmemory is clean, one that reaches
// CONFIG through Do is not, and one that configures through a renamed client
// still is.
func TestServerConfigMatcherCatchesHostileShapes(t *testing.T) {
	mentionsIt := `package x

import "testing"

// maxmemory is named here, not written: "OOM command not allowed when used memory > 'maxmemory'"
func TestA(t *testing.T) { s := "maxmemory"; _ = s; _ = t }
`
	writesIt := `package x

import "testing"

func TestB(t *testing.T) { client.ConfigSet(ctx, "maxmemory", "0"); _ = t }
`
	throughDo := `package x

import "testing"

func TestC(t *testing.T) { client.Do(ctx, "CONFIG", "SET", "maxmemory", "0"); _ = t }
`
	aliased := `package x

import "testing"

func TestD(t *testing.T) { c := someClient(); c.ConfigSet(ctx, "maxmemory", "0"); _ = t }
`
	// A call to a method that is NOT server config. Without this case the
	// matcher can be replaced by "any method call at all" and every other
	// case still passes: mentionsIt contains no calls, so it cannot tell a
	// narrow matcher from a wide one. Measured by a tooth on 2026-09-18.
	otherCall := `package x

import "testing"

func TestE(t *testing.T) { client.Set(ctx, "k", "v", 0); _ = t }
`
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"only names it", mentionsIt, false},
		{"calls something that is not server config", otherCall, false},
		{"writes it", writesIt, true},
		{"reaches CONFIG through Do", throughDo, true},
		{"configures through a renamed client", aliased, true},
	} {
		f, _ := parseSource(t, tc.src)
		if got := configuresServer(f); got != tc.want {
			t.Errorf("%s: configuresServer = %v, want %v", tc.name, got, tc.want)
		}
	}
}
