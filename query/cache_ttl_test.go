//go:build test

package query

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// These tests are the regression guard for the cache-freshness audit + the
// "every cache must have a TTL" mandate. Each newly TTL-bounded query-client
// cache must (a) serve the cached value while the TTL is large and (b) re-query
// the chain and reflect an on-chain change once the TTL is shrunk to 0. The
// var-to-0 pattern keeps these deterministic without sleeping. Each test sets
// its TTL var large first, then 0 — mirroring TestGetService_TTLRefreshesCUPR.

func newTTLTestClients(t *testing.T, address string) *Clients {
	t.Helper()
	qc, err := NewQueryClients(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })
	return qc
}

// --- Frozen param-client regressions (supplier + application + service were the
// three GetParams caches missing a TTL; supplier was the headline FROZEN_BUG). ---

func TestSupplierParams_RefreshAfterTTL(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	old := generateTestSupplierParams() // MinStake = 1_000_000 upokt
	mock.supplierParams = old

	prev := liveParamsCacheTTL
	liveParamsCacheTTL = time.Hour
	t.Cleanup(func() { liveParamsCacheTTL = prev })

	qc := newTTLTestClients(t, address)
	ctx := context.Background()

	p1, err := qc.Supplier().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1_000_000), p1.MinStake.Amount.Int64())

	// Governance MinStake change. Within the TTL the cached (old) value is served —
	// this is the exact frozen-cache regression: before the fix it would be served
	// FOREVER, not just within the TTL window.
	bumped := generateTestSupplierParams()
	newStake := cosmostypes.NewInt64Coin("upokt", 2_000_000)
	bumped.MinStake = &newStake
	mock.supplierParams = bumped

	p2, err := qc.Supplier().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1_000_000), p2.MinStake.Amount.Int64(), "within TTL the cached value is served")

	liveParamsCacheTTL = 0
	p3, err := qc.Supplier().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2_000_000), p3.MinStake.Amount.Int64(),
		"after TTL expiry supplier params must reflect the on-chain MinStake change (frozen-cache regression)")
}

func TestApplicationParams_RefreshAfterTTL(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	old := generateTestApplicationParams()
	old.MaxDelegatedGateways = 7
	mock.applicationParams = old

	prev := liveParamsCacheTTL
	liveParamsCacheTTL = time.Hour
	t.Cleanup(func() { liveParamsCacheTTL = prev })

	qc := newTTLTestClients(t, address)
	ctx := context.Background()

	p1, err := qc.Application().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(7), p1.MaxDelegatedGateways)

	bumped := generateTestApplicationParams()
	bumped.MaxDelegatedGateways = 11
	mock.applicationParams = bumped

	p2, err := qc.Application().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(7), p2.MaxDelegatedGateways, "within TTL the cached value is served")

	liveParamsCacheTTL = 0
	p3, err := qc.Application().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(11), p3.MaxDelegatedGateways,
		"after TTL expiry application params must reflect the on-chain change (frozen-cache regression)")
}

func TestServiceParams_RefreshAfterTTL(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	old := generateTestServiceParams()
	old.TargetNumRelays = 100
	mock.serviceParams = old

	prev := liveParamsCacheTTL
	liveParamsCacheTTL = time.Hour
	t.Cleanup(func() { liveParamsCacheTTL = prev })

	qc := newTTLTestClients(t, address)
	ctx := context.Background()

	p1, err := qc.Service().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(100), p1.TargetNumRelays)

	bumped := generateTestServiceParams()
	bumped.TargetNumRelays = 250
	mock.serviceParams = bumped

	p2, err := qc.Service().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(100), p2.TargetNumRelays, "within TTL the cached value is served")

	liveParamsCacheTTL = 0
	p3, err := qc.Service().GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(250), p3.TargetNumRelays,
		"after TTL expiry service params must reflect the on-chain change (frozen-cache regression)")
}

// --- Entity caches (latest semantics: liveEntityCacheTTL). ---

func TestGetApplication_RefreshAfterTTL(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	queryCount := 0
	stakeAmt := int64(1_000_000)
	mock.getApplicationFunc = func(_ context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
		queryCount++
		app := generateTestApplication(req.Address)
		stake := cosmostypes.NewInt64Coin("upokt", stakeAmt)
		app.Stake = &stake
		return &apptypes.QueryGetApplicationResponse{Application: *app}, nil
	}

	prev := liveEntityCacheTTL
	liveEntityCacheTTL = time.Hour
	t.Cleanup(func() { liveEntityCacheTTL = prev })

	qc := newTTLTestClients(t, address)
	ctx := context.Background()

	a1, err := qc.Application().GetApplication(ctx, "pokt1app")
	require.NoError(t, err)
	require.Equal(t, int64(1_000_000), a1.Stake.Amount.Int64())
	require.Equal(t, 1, queryCount)

	// Stake decreases on-chain; within TTL the cache hides it.
	stakeAmt = 250_000
	a2, err := qc.Application().GetApplication(ctx, "pokt1app")
	require.NoError(t, err)
	require.Equal(t, int64(1_000_000), a2.Stake.Amount.Int64(), "within TTL the cached stake is served")
	require.Equal(t, 1, queryCount, "within TTL must not re-query")

	liveEntityCacheTTL = 0
	a3, err := qc.Application().GetApplication(ctx, "pokt1app")
	require.NoError(t, err)
	require.Equal(t, int64(250_000), a3.Stake.Amount.Int64(),
		"after TTL expiry GetApplication must reflect the on-chain stake change")
	require.Equal(t, 2, queryCount)
}

func TestGetSupplier_RefreshAfterTTL(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	queryCount := 0
	services := []string{"develop"}
	mock.getSupplierFunc = func(_ context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
		queryCount++
		sup := generateTestSupplier(req.OperatorAddress)
		cfgs := make([]*sharedtypes.SupplierServiceConfig, 0, len(services))
		for _, s := range services {
			cfgs = append(cfgs, &sharedtypes.SupplierServiceConfig{ServiceId: s})
		}
		sup.Services = cfgs
		return &suppliertypes.QueryGetSupplierResponse{Supplier: *sup}, nil
	}

	prev := liveEntityCacheTTL
	liveEntityCacheTTL = time.Hour
	t.Cleanup(func() { liveEntityCacheTTL = prev })

	qc := newTTLTestClients(t, address)
	ctx := context.Background()

	s1, err := qc.Supplier().GetSupplier(ctx, "pokt1sup")
	require.NoError(t, err)
	require.Len(t, s1.Services, 1)
	require.Equal(t, 1, queryCount)

	// Supplier adds a service on-chain; within TTL it stays hidden.
	services = []string{"develop", "ethereum"}
	s2, err := qc.Supplier().GetSupplier(ctx, "pokt1sup")
	require.NoError(t, err)
	require.Len(t, s2.Services, 1, "within TTL the cached service set is served")
	require.Equal(t, 1, queryCount)

	liveEntityCacheTTL = 0
	s3, err := qc.Supplier().GetSupplier(ctx, "pokt1sup")
	require.NoError(t, err)
	require.Len(t, s3.Services, 2, "after TTL expiry GetSupplier must reflect the new service set")
	require.Equal(t, 2, queryCount)
}

func TestGetClaim_RefreshAfterTTL(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	queryCount := 0
	rootHash := []byte("root_v1")
	mock.getClaimFunc = func(_ context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		queryCount++
		claim := generateTestClaim(req.SupplierOperatorAddress, req.SessionId)
		claim.RootHash = append([]byte(nil), rootHash...)
		return &prooftypes.QueryGetClaimResponse{Claim: *claim}, nil
	}

	prev := liveEntityCacheTTL
	liveEntityCacheTTL = time.Hour
	t.Cleanup(func() { liveEntityCacheTTL = prev })

	qc := newTTLTestClients(t, address)
	ctx := context.Background()

	c1, err := qc.Proof().GetClaim(ctx, "pokt1sup", "sess1")
	require.NoError(t, err)
	require.Equal(t, "root_v1", string(c1.GetRootHash()))
	require.Equal(t, 1, queryCount)

	rootHash = []byte("root_v2")
	c2, err := qc.Proof().GetClaim(ctx, "pokt1sup", "sess1")
	require.NoError(t, err)
	require.Equal(t, "root_v1", string(c2.GetRootHash()), "within TTL the cached claim is served")
	require.Equal(t, 1, queryCount)

	liveEntityCacheTTL = 0
	c3, err := qc.Proof().GetClaim(ctx, "pokt1sup", "sess1")
	require.NoError(t, err)
	require.Equal(t, "root_v2", string(c3.GetRootHash()),
		"after TTL expiry GetClaim must reflect the updated on-chain claim")
	require.Equal(t, 2, queryCount)
}

// --- Height-keyed caches: past the floor a final entry is re-validated, never dropped. ---

// TestGetServiceRelayDifficultyAtHeight_RevalidatesAfterFloor: a final entry
// older than the floor keeps being served while the chain is asked again, and
// the answer replaces it. A different answer is counted: the chain rewrote a
// value already final (an upgrade handler can).
func TestGetServiceRelayDifficultyAtHeight_RevalidatesAfterFloor(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	var queries atomic.Int64
	var fill atomic.Int32
	fill.Store(1)
	mock.getRelayMiningDifficultyAtHeightFunc = func(_ context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		queries.Add(1)
		d := generateTestRelayMiningDifficulty(req.ServiceId)
		d.TargetHash = bytes.Repeat([]byte{byte(fill.Load())}, 32)
		return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{RelayMiningDifficulty: *d}, nil
	}

	qc := newTTLTestClients(t, address)
	cache := qc.serviceClient.difficultyAtHeight
	cache.floor = time.Hour
	ctx := context.Background()
	changedBefore := testutil.ToFloat64(queryCacheFinalAnswerChanged.WithLabelValues("service", "difficulty"))

	d, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte{1}, 32), d.TargetHash)

	// Within the floor the entry is served without asking the chain.
	fill.Store(2)
	d, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte{1}, 32), d.TargetHash, "within the floor the cached entry is served")
	require.Equal(t, int64(1), queries.Load())

	// Past the floor the old value is still served while the chain is asked again.
	cache.floor = 0
	d, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 100)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte{1}, 32), d.TargetHash, "the final value is served while it is re-validated")
	require.Eventually(t, func() bool { return queries.Load() == 2 }, 5*time.Second, time.Millisecond,
		"the entry past the floor must be asked again")
	cache.floor = time.Hour

	require.Eventually(t, func() bool {
		d, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, "develop", 100)
		return err == nil && bytes.Equal(d.TargetHash, bytes.Repeat([]byte{2}, 32))
	}, 5*time.Second, time.Millisecond, "the re-validated answer must replace the entry")
	require.Equal(t, changedBefore+1,
		testutil.ToFloat64(queryCacheFinalAnswerChanged.WithLabelValues("service", "difficulty")),
		"a different answer at a final height must be counted")
}

// TestGetServiceRelayDifficultyAtHeight_FailedRevalidationKeepsServing: a
// re-validation that fails, or that a node behind the height answers, must not
// drop or replace the final entry, and is retried only after another floor.
func TestGetServiceRelayDifficultyAtHeight_FailedRevalidationKeepsServing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		breakNode func(mock *mockQueryServer, fail *atomic.Bool)
	}{
		{"the node fails", func(_ *mockQueryServer, fail *atomic.Bool) { fail.Store(true) }},
		{"the node answers from below the height", func(mock *mockQueryServer, _ *atomic.Bool) { mock.answeredHeight.Store(50) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, address, cleanup, mock := setupMockQueryServer(t)
			defer cleanup()

			var queries atomic.Int64
			var fail atomic.Bool
			mock.getRelayMiningDifficultyAtHeightFunc = func(_ context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
				n := queries.Add(1)
				if fail.Load() {
					return nil, status.Error(codes.Unavailable, "node down")
				}
				d := generateTestRelayMiningDifficulty(req.ServiceId)
				d.TargetHash = bytes.Repeat([]byte{byte(n)}, 32)
				return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{RelayMiningDifficulty: *d}, nil
			}

			qc := newTTLTestClients(t, address)
			cache := qc.serviceClient.difficultyAtHeight
			cache.floor = time.Hour
			ctx := context.Background()
			key := heightKey{id: "develop", height: 100}

			_, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, key.id, key.height)
			require.NoError(t, err)
			want := bytes.Repeat([]byte{1}, 32)
			first, ok := cache.final.Load(key)
			require.True(t, ok)

			tc.breakNode(mock, &fail)
			cache.floor = 0
			d, err := qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, key.id, key.height)
			require.NoError(t, err)
			require.Equal(t, want, d.TargetHash, "the final value is served while it is re-validated")
			cache.floor = time.Hour
			restamped, ok := cache.final.Load(key)
			require.True(t, ok)
			require.True(t, restamped.fetchedAt.After(first.fetchedAt),
				"the lookup that starts the re-validation restamps the entry, so the next try waits a floor")
			require.Eventually(t, func() bool { return queries.Load() == 2 }, 5*time.Second, time.Millisecond)

			// Neither a failure nor a lagging node changes the entry.
			d, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(ctx, key.id, key.height)
			require.NoError(t, err)
			require.Equal(t, want, d.TargetHash)
			e, ok := cache.final.Load(key)
			require.True(t, ok, "the final entry must stay")
			require.Equal(t, want, e.value.TargetHash)
		})
	}
}

// TestGetParamsAtHeight_RevalidatesAfterFloor is the shared-params twin.
func TestGetParamsAtHeight_RevalidatesAfterFloor(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	old := generateTestSharedParams()
	old.NumBlocksPerSession = 4
	mock.sharedParamsAtHeight = old

	qc := newTTLTestClients(t, address)
	cache := qc.sharedClient.paramsAtHeight
	cache.floor = time.Hour
	ctx := context.Background()

	p, err := qc.Shared().GetParamsAtHeight(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(4), p.NumBlocksPerSession)

	changed := generateTestSharedParams()
	changed.NumBlocksPerSession = 8
	mock.sharedParamsAtHeight = changed

	p, err = qc.Shared().GetParamsAtHeight(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(4), p.NumBlocksPerSession, "within the floor the height-keyed params are served")

	cache.floor = 0
	p, err = qc.Shared().GetParamsAtHeight(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(4), p.NumBlocksPerSession, "the final value is served while it is re-validated")
	cache.floor = time.Hour
	require.Eventually(t, func() bool {
		p, err := qc.Shared().GetParamsAtHeight(ctx, 100)
		return err == nil && p.NumBlocksPerSession == 8
	}, 5*time.Second, time.Millisecond, "the re-validated params must replace the entry")
}

// TestSessionCache_BoundedEviction is the regression guard for unbounded session
// cache growth: evictOldSessionsLocked must drop entries whose session-start
// height is far below the newest once the map exceeds maxSessionCacheEntries.
func TestSessionCache_BoundedEviction(t *testing.T) {
	c := &sessionQueryClient{sessionCache: make(map[string]sessionCacheEntry)}

	// Fill past the cap with one entry per height.
	newest := int64(maxSessionCacheEntries + 200)
	for h := int64(1); h <= newest; h++ {
		c.sessionCache[fmt.Sprintf("app/svc/%d", h)] = sessionCacheEntry{height: h, cachedAt: time.Now()}
	}
	require.Greater(t, len(c.sessionCache), maxSessionCacheEntries)

	c.evictOldSessionsLocked(newest)

	require.LessOrEqual(t, len(c.sessionCache), maxSessionCacheEntries,
		"eviction must bring the map back under the cap")
	// Heights within the keep window survive; far-older ones are gone.
	_, recent := c.sessionCache[fmt.Sprintf("app/svc/%d", newest)]
	require.True(t, recent, "the newest session must be retained")
	_, old := c.sessionCache[fmt.Sprintf("app/svc/%d", int64(1))]
	require.False(t, old, "a session far below the keep window must be evicted")
}
