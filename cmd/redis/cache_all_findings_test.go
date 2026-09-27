//go:build test

package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	transportredis "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// These tests lock in the fixes for the adversarial-review findings on the
// --type all cleanup: the classify-then-delete TOCTOU, the delete-before-
// publish ordering (invariant 4), namespace awareness, the confirmation
// abort path, and the typed single-key invalidation payloads.

func TestDeleteIfStillContaminated_TOCTOU(t *testing.T) {
	healthyJSON := `{"staked":true,"status":"active","operator_address":"pokt1x","services":["svc1"]}`
	contaminatedJSON := `{"staked":true,"status":"active","operator_address":"pokt1x","services":[]}`

	cases := []struct {
		name        string
		value       string
		missing     bool
		wantDeleted bool
	}{
		{name: "still contaminated -> deleted", value: contaminatedJSON, wantDeleted: true},
		{name: "healed to healthy -> preserved", value: healthyJSON, wantDeleted: false},
		{name: "key gone -> preserved (no-op)", missing: true, wantDeleted: false},
		{name: "unparseable -> preserved (fail-safe)", value: "not-json", wantDeleted: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, mr := newTestCacheClient(t)
			key := "ha:supplier:pokt1x"
			if !tc.missing {
				require.NoError(t, mr.Set(key, tc.value))
			}

			// The scan classified this key as contaminated earlier; by delete
			// time the value is whatever tc says (the TOCTOU window).
			deleted, err := deleteIfStillContaminated(context.Background(), client, key)
			require.NoError(t, err)
			assert.Equal(t, tc.wantDeleted, deleted)
			assert.Equal(t, tc.wantDeleted || tc.missing, !mr.Exists(key))
			if !tc.wantDeleted && !tc.missing {
				// The surviving value must be byte-identical (no partial write).
				got, getErr := mr.Get(key)
				require.NoError(t, getErr)
				assert.Equal(t, tc.value, got)
			}
		})
	}
}

func TestInvalidateAllTypes_HealedSupplierPreservedEndToEnd(t *testing.T) {
	client, mr := newTestCacheClient(t)

	// Contaminated at scan time...
	healed := "ha:supplier:pokt1healed"
	require.NoError(t, mr.Set(healed, `{"staked":true,"status":"active","operator_address":"pokt1healed","services":[]}`))
	require.NoError(t, mr.Set("ha:cache:application:app1", "x"))

	// ...healed before the delete phase. We simulate the miner reconcile's
	// concurrent rewrite by swapping the value inside the publish seam's
	// pre-delete window: buildCleanupPlan runs first, so mutate right after
	// planning by wrapping the publish (which runs post-delete) is too late.
	// Instead, exercise the real sequence: plan externally, heal, then run
	// the same conditional delete the cleanup uses.
	scope := newCleanupScope(client)
	plan, err := buildCleanupPlan(context.Background(), client, scope)
	require.NoError(t, err)
	require.Equal(t, []string{healed}, plan.supplierCandidates, "sanity: scan saw it contaminated")

	require.NoError(t, mr.Set(healed, `{"staked":true,"status":"active","operator_address":"pokt1healed","services":["svc1"]}`))

	deleted, err := deleteIfStillContaminated(context.Background(), client, healed)
	require.NoError(t, err)
	assert.False(t, deleted, "healed supplier must survive the delete phase")
	assert.True(t, mr.Exists(healed))
}

func TestInvalidateAllTypes_PublishesOnlyAfterAllDeletes(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedCleanupFixture(t, mr)

	// Invariant 4: the clear-all publish must observe a fully-deleted L2.
	// Wrap the publish seam to capture what still exists at publish time —
	// deterministic because invalidateAllTypes runs the whole sequence on
	// this goroutine.
	orig := publishClearAll
	t.Cleanup(func() { publishClearAll = orig })

	var leftoverAtPublish []string
	publishClearAll = func(ctx context.Context, c *DebugRedisClient, s cleanupScope) int {
		plan, planErr := buildCleanupPlan(ctx, c, s)
		require.NoError(t, planErr)
		leftoverAtPublish = append(leftoverAtPublish, plan.cacheKeys...)
		leftoverAtPublish = append(leftoverAtPublish, plan.supplierCandidates...)
		return orig(ctx, c, s)
	}

	require.NoError(t, invalidateAllTypes(context.Background(), client, false, true))
	assert.Emptyf(t, leftoverAtPublish,
		"clear-all published while %d regenerable keys still existed (publish-before-delete regression)", len(leftoverAtPublish))
}

func TestInvalidateAllTypes_ConfirmationAbortDeletesNothing(t *testing.T) {
	client, mr := newTestCacheClient(t)
	seedCleanupFixture(t, mr)

	// Answer 'n' on stdin: the run must abort without deleting or publishing.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	_, err = w.WriteString("n\n")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	require.NoError(t, invalidateAllTypes(context.Background(), client, false, false /*yes*/))

	for _, k := range allFixtureKeys() {
		assert.Truef(t, mr.Exists(k), "abort must not delete %s", k)
	}
}

func TestInvalidateAllTypes_CustomNamespaceScopesDeletesAndChannels(t *testing.T) {
	mrClient, mr := newTestCacheClient(t)
	_ = mrClient // default-namespace client unused; we build a custom one below

	ctx := context.Background()
	// Note: the namespace carries only a base prefix now; every segment below it
	// is a constant in transport/redis, so there is nothing else to spell out.
	customNS := config.DefaultRedisNamespaceConfig()
	customNS.BasePrefix = "custom"
	cli, err := transportredis.NewClient(ctx, transportredis.ClientConfig{
		URL:       fmt.Sprintf("redis://%s", mr.Addr()),
		Namespace: customNS,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	logger := logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "text", Async: false})
	client := &DebugRedisClient{Client: cli, Logger: logger}

	// Custom-namespace deployment keys (must be cleaned)...
	require.NoError(t, mr.Set("custom:cache:application:app1", "x"))
	require.NoError(t, mr.Set("custom:supplier:pokt1contam", `{"staked":true,"status":"active","operator_address":"pokt1contam","services":[]}`))
	// ...and a neighboring default-namespace deployment sharing the Redis
	// (must NOT be touched — cross-tenant protection).
	require.NoError(t, mr.Set("ha:cache:application:other", "y"))
	require.NoError(t, mr.Set("ha:supplier:pokt1other", `{"staked":true,"status":"active","operator_address":"pokt1other","services":[]}`))

	scope := newCleanupScope(client)
	assert.Equal(t, "custom:cache:*", scope.cachePattern)
	assert.Equal(t, "custom:supplier:*", scope.supplierPattern)
	for _, ch := range scope.channels {
		assert.Containsf(t, ch, "custom:", "channel %s must live in the custom namespace", ch)
	}

	require.NoError(t, invalidateAllTypes(ctx, client, false, true))

	assert.False(t, mr.Exists("custom:cache:application:app1"), "custom-namespace cache key must be deleted")
	assert.False(t, mr.Exists("custom:supplier:pokt1contam"), "custom-namespace contaminated supplier must be deleted")
	assert.True(t, mr.Exists("ha:cache:application:other"), "foreign-namespace cache key must survive")
	assert.True(t, mr.Exists("ha:supplier:pokt1other"), "foreign-namespace supplier must survive")
}

func TestInvalidationPayload_TypedFields(t *testing.T) {
	cases := []struct {
		cacheType string
		wantField string
	}{
		{"application", "address"},
		{"account", "address"},
		{"service", "service_id"},
		{"supplier", "operator_address"},
	}
	for _, tc := range cases {
		t.Run(tc.cacheType, func(t *testing.T) {
			payload := invalidationPayload(tc.cacheType, "some-key")
			var decoded map[string]string
			require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
			assert.Equal(t, "some-key", decoded["key"], "legacy field kept for compat")
			assert.Equalf(t, "some-key", decoded[tc.wantField],
				"handler for %s parses %q — payload must carry it or remote L1s never clear", tc.cacheType, tc.wantField)
		})
	}

	t.Run("params singleton falls back to key-only", func(t *testing.T) {
		payload := invalidationPayload("shared_params", "k")
		var decoded map[string]string
		require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
		assert.Equal(t, map[string]string{"key": "k"}, decoded)
	})
}
