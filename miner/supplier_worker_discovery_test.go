//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestRecordDiscovered_WritesToKnownSets proves relay-traffic discovery persists
// app/service addresses to the shared Redis known-sets the leader's orchestrator
// reads. Before the fix, discovery was gated on a never-wired cacheOrchestrator
// (always nil), so apps seen only via live traffic were never refreshed.
func TestRecordDiscovered_WritesToKnownSets(t *testing.T) {

	ctx := context.Background()
	rc, _ := newTestRedis(t)
	defer func() { _ = rc.Close() }()

	w := NewSupplierWorker(SupplierWorkerConfig{
		Logger:      logging.NewLoggerFromConfig(logging.DefaultConfig()),
		RedisClient: rc,
	})

	// Same entities seen multiple times (local dedup must keep the set correct).
	w.recordDiscovered(ctx, "pokt1app", "svc-x")
	w.recordDiscovered(ctx, "pokt1app", "svc-x")
	w.recordDiscovered(ctx, "pokt1app2", "")

	apps, err := rc.SMembers(ctx, rc.KB().CacheKnownKey("applications")).Result()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"pokt1app", "pokt1app2"}, apps)

	svcs, err := rc.SMembers(ctx, rc.KB().CacheKnownKey("services")).Result()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"svc-x"}, svcs)
}
