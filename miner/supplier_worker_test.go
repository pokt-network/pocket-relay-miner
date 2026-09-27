//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
)

// TestDefaultSupplierReconcileIntervalMatchesTTLMarginAssumption pins the
// other half of cache.TestSupplierCacheTTLFromParams_MarginAboveReconcileInterval:
// that test hardcodes 60s as "the reconcile interval" because cache cannot
// import miner (miner already imports cache — a cycle). This test makes sure
// the two numbers cannot drift apart silently: if
// DefaultSupplierReconcileInterval ever changes, this fails and says to go
// re-check the TTL margin in cache/supplier_cache.go, instead of the margin
// quietly shrinking underneath an unrelated tuning change.
func TestDefaultSupplierReconcileIntervalMatchesTTLMarginAssumption(t *testing.T) {
	require.Equal(t, 60*time.Second, DefaultSupplierReconcileInterval,
		"cache.SupplierCacheTTLFromParams' margin test assumes this exact value — "+
			"changing DefaultSupplierReconcileInterval requires re-checking that the "+
			"supplier cache TTL still comfortably outlives it")
}

// TestRefineSupplierCacheTTLSurvivesWorkerQueryClientsGoingNil is a regression
// test for the race found in review 2026-08-21: the TTL-refinement task Start()
// dispatches on the master pool used to read w.queryClients.Shared() directly
// inside its closure. cleanup() sets w.queryClients = nil BEFORE calling
// masterPool.Stop(), and pond/v2's Stop() does not wait for queued tasks to
// finish (unlike StopAndWait(), used by leader_controller.go for the same
// reason) — so a task still queued when Close() runs (e.g. because a LATER
// startup step in cmd_miner.go failed and the deferred Close() fired) could
// run against a nil w.queryClients and panic on qc.Shared() dereferencing a
// nil receiver. pond's default panicRecovery swallows that silently.
//
// The fix moved the read behind an explicit qc parameter the caller captures
// BEFORE dispatch, mirroring the pre-existing advisory block. This test
// proves the method itself cannot read w.queryClients: it sets that field to
// nil, exactly simulating cleanup() having already run, and calls
// refineSupplierCacheTTL with a still-valid qc obtained separately.
func TestRefineSupplierCacheTTLSurvivesWorkerQueryClientsGoingNil(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	redisClient, _ := newTestRedis(t)
	supplierCache := cache.NewSupplierCache(logger, redisClient, cache.SupplierCacheConfig{})

	// A real *query.Clients pointed at a refused local port: construction
	// never dials (grpc.NewClient is lazy), and the GetParams call below
	// fails fast on connection refused rather than hanging out to the 10s
	// advisory timeout — the point of this test is the nil-receiver panic,
	// not what GetParams returns.
	qc, err := query.NewQueryClients(logger, query.ClientConfig{
		GRPCEndpoint: "127.0.0.1:1",
		QueryTimeout: time.Second,
	})
	require.NoError(t, err)
	defer qc.Close()

	w := &SupplierWorker{
		logger: logger,
		ctx:    context.Background(),
		config: SupplierWorkerConfig{Config: &Config{}},
	}
	// Simulate the exact race this regression guards against: by the time
	// the dispatched task runs, cleanup() already nil'd the worker's field.
	w.queryClients = nil

	require.NotPanics(t, func() {
		w.refineSupplierCacheTTL(qc, supplierCache)
	}, "refineSupplierCacheTTL must not read w.queryClients — it must use the qc parameter captured before dispatch")
}
