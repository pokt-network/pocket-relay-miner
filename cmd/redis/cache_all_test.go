//go:build test

package redis

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
)

// --- Fixture key groups ---------------------------------------------------
//
// The four slices below are the single source of truth for every test in
// this file: seedCleanupFixture writes exactly these keys, and each test
// asserts survival/deletion against the same lists (no drift between what is
// seeded and what is checked).

// regenerableCacheKeys are ha:cache:* entries (non-lock) the cleanup deletes.
var regenerableCacheKeys = []string{
	"ha:cache:application:app1",
	"ha:cache:application:app2",
	"ha:cache:service:svc1",
	"ha:cache:account:acc1",
	"ha:cache:shared_params",
	"ha:cache:session_params",
	"ha:cache:proof_params",
	"ha:cache:known:applications",
	"ha:cache:known:suppliers",
}

// preservedLockKeys are ha:cache:lock:* repopulation locks the cleanup must
// never delete (they self-expire via TTL; deleting a held lock breaks the
// mutual exclusion that bounds the post-cleanup L3 refill).
var preservedLockKeys = []string{
	"ha:cache:lock:shared_params",
	"ha:cache:lock:application:app1",
}

// contaminatedSupplierKey is the one ha:supplier:* entry the cleanup deletes
// (staked+active with empty services — a failed-write artifact).
const contaminatedSupplierKey = "ha:supplier:pokt1contam"

// survivingSupplierKeys are ha:supplier:* entries that must be preserved:
// a healthy staked supplier, an unstaking one (services drained but not
// contaminated), a not-staked one, and a garbage/unparseable value
// (fail-safe: never delete what we cannot classify).
var survivingSupplierKeys = []string{
	"ha:supplier:pokt1healthy",
	"ha:supplier:pokt1unstaking",
	"ha:supplier:pokt1notstaked",
	"ha:supplier:pokt1garbage",
}

// negativeControlKeys are state keys (registry, sessions, SMST, WAL, leader,
// tx-tracking) outside the ha:cache:* and ha:supplier:* patterns. The cleanup
// must never touch them.
var negativeControlKeys = []string{
	"ha:suppliers:index",
	"ha:suppliers:pokt1healthy",
	"ha:miner:sessions:pokt1healthy:sess1",
	"ha:smst:pokt1healthy:sess1:nodes",
	"ha:relays:pokt1healthy",
	"ha:miner:global_leader",
	"ha:tx:track:pokt1healthy:100:sess1",
}

// mustSupplierJSON marshals a SupplierState to the JSON the miner writes to
// ha:supplier:*, so the fixture matches production value shape exactly.
func mustSupplierJSON(t *testing.T, s cache.SupplierState) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

// seedCleanupFixture writes the full fixture described by the key-group
// slices above plus the four typed supplier states.
func seedCleanupFixture(t *testing.T, mr *miniredis.Miniredis) {
	t.Helper()

	// Regenerable ha:cache:* string singletons and per-entity keys.
	for _, k := range []string{
		"ha:cache:application:app1",
		"ha:cache:application:app2",
		"ha:cache:service:svc1",
		"ha:cache:account:acc1",
		"ha:cache:shared_params",
		"ha:cache:session_params",
		"ha:cache:proof_params",
	} {
		require.NoError(t, mr.Set(k, "payload"))
	}

	// Known-entity tracking sets.
	if _, err := mr.SAdd("ha:cache:known:applications", "app1"); err != nil {
		t.Fatalf("seed known:applications: %v", err)
	}
	if _, err := mr.SAdd("ha:cache:known:suppliers", "pokt1healthy"); err != nil {
		t.Fatalf("seed known:suppliers: %v", err)
	}

	// Repopulation locks (must survive).
	for _, k := range preservedLockKeys {
		require.NoError(t, mr.Set(k, "held"))
	}

	// Supplier states: one healthy, one contaminated, one unstaking, one
	// not-staked, and one unparseable.
	require.NoError(t, mr.Set("ha:supplier:pokt1healthy", mustSupplierJSON(t, cache.SupplierState{
		Staked:          true,
		Status:          cache.SupplierStatusActive,
		OperatorAddress: "pokt1healthy",
		Services:        []string{"svc1"},
	})))
	require.NoError(t, mr.Set(contaminatedSupplierKey, mustSupplierJSON(t, cache.SupplierState{
		Staked:          true,
		Status:          cache.SupplierStatusActive,
		OperatorAddress: "pokt1contam",
		Services:        []string{},
	})))
	require.NoError(t, mr.Set("ha:supplier:pokt1unstaking", mustSupplierJSON(t, cache.SupplierState{
		Staked:          true,
		Status:          cache.SupplierStatusUnstaking,
		OperatorAddress: "pokt1unstaking",
		Services:        []string{},
	})))
	require.NoError(t, mr.Set("ha:supplier:pokt1notstaked", mustSupplierJSON(t, cache.SupplierState{
		Staked:          false,
		Status:          cache.SupplierStatusNotStaked,
		OperatorAddress: "pokt1notstaked",
		Services:        []string{},
	})))
	require.NoError(t, mr.Set("ha:supplier:pokt1garbage", "not-json"))

	// Negative controls: state keys that must never be touched.
	if _, err := mr.SAdd("ha:suppliers:index", "pokt1healthy"); err != nil {
		t.Fatalf("seed suppliers:index: %v", err)
	}
	mr.HSet("ha:suppliers:pokt1healthy", "operator_address", "pokt1healthy")
	require.NoError(t, mr.Set("ha:miner:sessions:pokt1healthy:sess1", "session-meta"))
	mr.HSet("ha:smst:pokt1healthy:sess1:nodes", "node1", "data")
	require.NoError(t, mr.Set("ha:relays:pokt1healthy", "wal"))
	require.NoError(t, mr.Set("ha:miner:global_leader", "instance-1"))
	require.NoError(t, mr.Set("ha:tx:track:pokt1healthy:100:sess1", "tracking"))
}

// allFixtureKeys returns every key seedCleanupFixture writes, for the dry-run
// "nothing deleted" assertion.
func allFixtureKeys() []string {
	keys := make([]string, 0, 20)
	keys = append(keys, regenerableCacheKeys...)
	keys = append(keys, preservedLockKeys...)
	keys = append(keys, contaminatedSupplierKey)
	keys = append(keys, survivingSupplierKeys...)
	keys = append(keys, negativeControlKeys...)
	return keys
}

func TestInvalidateAllTypes_DryRunDeletesNothing(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedCleanupFixture(t, mr)

	err := invalidateAllTypes(context.Background(), client, true /*dryRun*/, false /*yes*/)
	require.NoError(t, err)

	for _, k := range allFixtureKeys() {
		assert.Truef(t, mr.Exists(k), "dry-run must not delete %s", k)
	}
}

func TestInvalidateAllTypes_DeletesRegenerableOnly(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedCleanupFixture(t, mr)

	err := invalidateAllTypes(context.Background(), client, false /*dryRun*/, true /*yes*/)
	require.NoError(t, err)

	// Deleted: every regenerable ha:cache:* key plus the contaminated supplier.
	for _, k := range regenerableCacheKeys {
		assert.Falsef(t, mr.Exists(k), "regenerable key %s should be deleted", k)
	}
	assert.Falsef(t, mr.Exists(contaminatedSupplierKey),
		"contaminated supplier %s should be deleted", contaminatedSupplierKey)

	// Survives: repopulation locks.
	for _, k := range preservedLockKeys {
		assert.Truef(t, mr.Exists(k), "repopulation lock %s must survive", k)
	}
	// Survives: every healthy/unclassifiable supplier entry.
	for _, k := range survivingSupplierKeys {
		assert.Truef(t, mr.Exists(k), "supplier %s must survive", k)
	}
	// Survives: every state key (negative controls).
	for _, k := range negativeControlKeys {
		assert.Truef(t, mr.Exists(k), "state key %s must never be touched", k)
	}
}

func TestInvalidateAllTypes_PublishesClearAllToSixChannels(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedCleanupFixture(t, mr)

	expectedChannels := []string{
		"ha:events:cache:application:invalidate",
		"ha:events:cache:service:invalidate",
		"ha:events:cache:supplier:invalidate",
		"ha:events:cache:account:invalidate",
		"ha:events:cache:shared_params:invalidate",
		"ha:events:cache:proof_params:invalidate",
		// supplier_params subscribes on a nonstandard channel derived from
		// EventsCachePrefix (miner wires PubSubPrefix to it — see
		// miner/leader_controller.go) and its L1 has no TTL — the cleanup
		// must notify it too or running instances serve stale params until
		// restart.
		"ha:events:cache:invalidate:supplier_params",
	}

	// Lock the source contract: the cleanup must publish to exactly these
	// EventChannel cache types (plus the supplier_params channel above).
	assert.ElementsMatch(t, []string{
		"application", "service", "supplier", "account", "shared_params", "proof_params",
	}, clearAllChannelTypes)
	assert.ElementsMatch(t, expectedChannels, newCleanupScope(client).channels)

	// miniredis' in-process subscriber delivers on an UNBUFFERED channel, so
	// each PUBLISH blocks until read. Drain the six deliveries in a helper
	// goroutine into a buffered channel: a deterministic rendezvous that needs
	// no sleeps. invalidateAllTypes then runs synchronously (exactly as
	// production calls it) and each publish hands off to the drainer.
	sub := mr.NewSubscriber()
	for _, ch := range expectedChannels {
		sub.Subscribe(ch)
	}
	collected := make(chan miniredis.PubsubMessage, len(expectedChannels))
	go func() {
		for i := 0; i < len(expectedChannels); i++ {
			collected <- <-sub.Messages()
		}
	}()

	err := invalidateAllTypes(context.Background(), client, false /*dryRun*/, true /*yes*/)
	require.NoError(t, err)

	got := make(map[string]string, len(expectedChannels))
	for i := 0; i < len(expectedChannels); i++ {
		select {
		case msg := <-collected:
			got[msg.Channel] = msg.Message
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for clear-all message %d/%d", i+1, len(expectedChannels))
		}
	}

	require.Len(t, got, len(expectedChannels))
	for _, ch := range expectedChannels {
		payload, ok := got[ch]
		assert.Truef(t, ok, "no clear-all published to %s", ch)
		assert.Equalf(t, "{}", payload, "clear-all payload on %s must be exactly {}", ch)
	}
}

func TestBuildCleanupPlan_Classification(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedCleanupFixture(t, mr)

	plan, err := buildCleanupPlan(context.Background(), client, newCleanupScope(client))
	require.NoError(t, err)

	// 9 regenerable ha:cache:* keys + 1 contaminated supplier candidate.
	assert.Len(t, plan.cacheKeys, 9)
	assert.Len(t, plan.supplierCandidates, 1)
	assert.Equal(t, 10, plan.totalPlanned())

	assert.Equal(t, 2, plan.deleteCounts["application"])
	assert.Equal(t, 1, plan.deleteCounts["service"])
	assert.Equal(t, 1, plan.deleteCounts["account"])
	assert.Equal(t, 2, plan.deleteCounts["known"])
	assert.Equal(t, 1, plan.deleteCounts["shared_params"])
	assert.Equal(t, 1, plan.deleteCounts["session_params"])
	assert.Equal(t, 1, plan.deleteCounts["proof_params"])
	assert.Equal(t, 1, plan.deleteCounts["supplier (contaminated)"])

	assert.Equal(t, 2, plan.locksPreserved)
	assert.Equal(t, 3, plan.suppliersHealthy)
	assert.Equal(t, 1, plan.supplierReadErrors)

	// Field-level cross-check: the contaminated supplier is the only
	// candidate; no healthy/garbage supplier leaked in, and no supplier key
	// leaked into the unconditional cacheKeys list.
	assert.Equal(t, []string{contaminatedSupplierKey}, plan.supplierCandidates)
	for _, k := range plan.cacheKeys {
		assert.NotContainsf(t, k, "ha:supplier:", "supplier key %s must not be in cacheKeys", k)
	}
	for _, sk := range survivingSupplierKeys {
		assert.NotContains(t, plan.supplierCandidates, sk)
	}
}

func TestCacheCmd_TypeAllFlagValidation(t *testing.T) {
	cases := []struct {
		name            string
		args            []string
		wantErrContains string
	}{
		{
			name:            "invalidate without all",
			args:            []string{"--type", "all", "--invalidate"},
			wantErrContains: "requires --invalidate --all",
		},
		{
			name:            "all without invalidate",
			args:            []string{"--type", "all", "--all"},
			wantErrContains: "requires --invalidate --all",
		},
		{
			name:            "with key",
			args:            []string{"--type", "all", "--invalidate", "--all", "--key", "x"},
			wantErrContains: "does not support",
		},
		{
			name:            "with key-file",
			args:            []string{"--type", "all", "--invalidate", "--all", "--key-file", "f"},
			wantErrContains: "does not support",
		},
		{
			name:            "with list",
			args:            []string{"--type", "all", "--list"},
			wantErrContains: "requires --invalidate --all",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CacheCmd()
			c.SetArgs(tc.args)
			c.SilenceUsage = true
			c.SilenceErrors = true
			err := c.Execute()
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErrContains)
		})
	}
}

func TestInvalidateAllTypes_ZeroKeysCleanExit(t *testing.T) {
	client, _ := newTestCacheClient(t)

	err := invalidateAllTypes(context.Background(), client, false /*dryRun*/, true /*yes*/)
	require.NoError(t, err)
}

func TestCacheKeyGroup(t *testing.T) {
	client, _ := newTestCacheClient(t)
	scope := newCleanupScope(client)
	cases := []struct {
		name     string
		redisKey string
		want     string
	}{
		{"application entity", "ha:cache:application:app1", "application"},
		{"shared_params singleton", "ha:cache:shared_params", "shared_params"},
		{"known tracking set", "ha:cache:known:applications", "known"},
		{"account entity", "ha:cache:account:acc1", "account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cacheKeyGroup(scope, tc.redisKey))
		})
	}
}
