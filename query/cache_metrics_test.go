//go:build test

package query

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// TestQueryCacheHitMissMetrics verifies that queryCacheHits increments on a
// cache hit and queryCacheMisses increments on a cache miss (chain query) for
// the query-client L1 caches. Two sub-tests cover shared/params and
// session/session — representative of both the params and entity cache_type labels.
//
// This satisfies the >=2-source verifiability requirement: the counter increments
// proven here are the first source; the debug log wired at the same call sites is
// the second source.
func TestQueryCacheHitMissMetrics(t *testing.T) {
	t.Run("shared/params miss then hit", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()

		mock.sharedParams = generateTestSharedParams()

		logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
		qc, err := NewQueryClients(logger, ClientConfig{
			GRPCEndpoint: address,
			QueryTimeout: 5 * time.Second,
		})
		require.NoError(t, err)
		defer qc.Close()
		ctx := context.Background()

		// First call: cache cold → miss.
		missBefore := testutil.ToFloat64(queryCacheMisses.WithLabelValues("shared", "params"))
		hitBefore := testutil.ToFloat64(queryCacheHits.WithLabelValues("shared", "params"))

		_, err = qc.Shared().GetParams(ctx)
		require.NoError(t, err)

		require.Equal(t, missBefore+1,
			testutil.ToFloat64(queryCacheMisses.WithLabelValues("shared", "params")),
			"first (cold) GetParams must increment queryCacheMisses")
		require.Equal(t, hitBefore,
			testutil.ToFloat64(queryCacheHits.WithLabelValues("shared", "params")),
			"first (cold) GetParams must NOT increment queryCacheHits")

		// Second call: cache warm → hit.
		_, err = qc.Shared().GetParams(ctx)
		require.NoError(t, err)

		require.Equal(t, missBefore+1,
			testutil.ToFloat64(queryCacheMisses.WithLabelValues("shared", "params")),
			"second (warm) GetParams must NOT increment queryCacheMisses")
		require.Equal(t, hitBefore+1,
			testutil.ToFloat64(queryCacheHits.WithLabelValues("shared", "params")),
			"second (warm) GetParams must increment queryCacheHits")
	})

	t.Run("shared/params cache size gauge reflects populated state", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()

		mock.sharedParams = generateTestSharedParams()

		logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
		qc, err := NewQueryClients(logger, ClientConfig{
			GRPCEndpoint: address,
			QueryTimeout: 5 * time.Second,
		})
		require.NoError(t, err)
		defer qc.Close()
		ctx := context.Background()

		// shared/params is a single-slot cache (not a map), so the gauge is Set(1)
		// when populated and Set(0) when empty. Assert it reads 1 after a cold call.
		_, err = qc.Shared().GetParams(ctx)
		require.NoError(t, err)

		require.Equal(t, float64(1),
			testutil.ToFloat64(queryCacheSize.WithLabelValues("shared", "params")),
			"after first cache store, queryCacheSize must be 1 (slot populated)")

		// Repeated calls must not change the size (slot already counted).
		_, err = qc.Shared().GetParams(ctx)
		require.NoError(t, err)

		require.Equal(t, float64(1),
			testutil.ToFloat64(queryCacheSize.WithLabelValues("shared", "params")),
			"cache hit must leave queryCacheSize at 1")
	})

	t.Run("session/session miss then hit", func(t *testing.T) {
		_, address, cleanup, mock := setupMockQueryServer(t)
		defer cleanup()

		mock.sharedParams = generateTestSharedParams()
		session := generateTestSession("pokt1app1", "develop", 1)
		mock.getSessionFunc = func(_ context.Context, _ *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
			return &sessiontypes.QueryGetSessionResponse{Session: session}, nil
		}

		logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
		qc, err := NewQueryClients(logger, ClientConfig{
			GRPCEndpoint: address,
			QueryTimeout: 5 * time.Second,
		})
		require.NoError(t, err)
		defer qc.Close()
		ctx := context.Background()

		missBefore := testutil.ToFloat64(queryCacheMisses.WithLabelValues("session", "session"))
		hitBefore := testutil.ToFloat64(queryCacheHits.WithLabelValues("session", "session"))

		// First call: cold miss.
		_, err = qc.Session().GetSession(ctx, "pokt1app1", "develop", 5)
		require.NoError(t, err)

		require.Equal(t, missBefore+1,
			testutil.ToFloat64(queryCacheMisses.WithLabelValues("session", "session")),
			"first (cold) GetSession must increment queryCacheMisses")
		require.Equal(t, hitBefore,
			testutil.ToFloat64(queryCacheHits.WithLabelValues("session", "session")),
			"first (cold) GetSession must NOT increment queryCacheHits")

		// Second call for the same (app, service, sessionStartHeight) → hit.
		_, err = qc.Session().GetSession(ctx, "pokt1app1", "develop", 5)
		require.NoError(t, err)

		require.Equal(t, missBefore+1,
			testutil.ToFloat64(queryCacheMisses.WithLabelValues("session", "session")),
			"second (warm) GetSession must NOT increment queryCacheMisses")
		require.Equal(t, hitBefore+1,
			testutil.ToFloat64(queryCacheHits.WithLabelValues("session", "session")),
			"second (warm) GetSession must increment queryCacheHits")
	})
}
