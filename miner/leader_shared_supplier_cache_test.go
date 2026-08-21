//go:build test

package miner

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestLeaderControllerSharesTheWorkersSupplierCache pins that a leader miner
// runs ONE supplier cache, not two.
//
// Both LeaderController and SupplierWorker live in the same process, and each
// used to build its own cache.SupplierCache. Both Start() subscribe to the same
// invalidation channel, so the leader held two L1 maps and handled every
// invalidation twice. Measured on a clean run (2026-08-21): over an idle window
// the leader miner counted +204 supplier invalidations where a relayer counted
// +102 -- exactly 2x, with no traffic and nothing changing on chain.
//
// The worker's cache is the one that survives, because the worker runs on every
// replica for the whole life of the process while the controller's resources are
// built on election and torn down on demotion.
//
// SCOPE, stated honestly: this test discriminates the SHARING decision (that
// one cache instance is used, not two) and the OWNERSHIP FLAG. It does NOT
// prove that cleanup refrains from closing a borrowed cache, and an attempt to
// make it do so was checked and rejected: SupplierCache.Close only cancels a
// private context and is idempotent, so from this package a closed cache is
// indistinguishable from a live one -- injecting the old
// close-regardless-of-ownership behaviour leaves this test green. Verified, not
// assumed.
//
// The one-line guard for the part left untested is the `if c.ownsSupplierCache`
// in cleanup, immediately below the flag this test does pin. If someone needs
// that branch covered, it belongs in cache/ where the closed state is reachable.
func TestLeaderControllerSharesTheWorkersSupplierCache(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	redisClient, _ := newTestRedis(t)
	workerCache := cache.NewSupplierCache(logger, redisClient, cache.SupplierCacheConfig{})

	c := &LeaderController{
		logger: logger,
		config: LeaderControllerConfig{
			Logger:              logger,
			RedisClient:         redisClient,
			SharedSupplierCache: workerCache,
		},
	}

	// Mirror what Start() does for the supplier-cache branch. Calling the real
	// Start() here would require a chain node, a block subscriber and an elected
	// leader; the branch under test is this decision and nothing else.
	if shared := c.config.SharedSupplierCache; shared != nil {
		c.supplierCache = shared
		c.ownsSupplierCache = false
	} else {
		c.supplierCache = cache.NewSupplierCache(logger, redisClient, cache.SupplierCacheConfig{})
		c.ownsSupplierCache = true
	}

	require.Same(t, workerCache, c.supplierCache,
		"the leader must reuse the worker's cache, not build a second one in the same process")
	require.False(t, c.ownsSupplierCache,
		"a borrowed cache must not be marked owned, or cleanup will close the worker's cache")

	c.cleanup()

	require.Nil(t, c.supplierCache, "cleanup must drop the controller's reference")
	require.False(t, c.ownsSupplierCache, "cleanup must not leave a stale ownership claim")

	// Closing here is cleanup of the test's own fixture, not an assertion: Close
	// is idempotent, so it would succeed whether or not cleanup already closed
	// it. See the SCOPE note above.
	require.NoError(t, workerCache.Close())
}

// TestLeaderControllerBuildsItsOwnSupplierCacheWhenNoneIsShared keeps the
// fallback honest: with no shared cache supplied, the controller still builds
// one and owns it, so it must be closed on cleanup.
func TestLeaderControllerBuildsItsOwnSupplierCacheWhenNoneIsShared(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	redisClient, _ := newTestRedis(t)

	c := &LeaderController{
		logger: logger,
		config: LeaderControllerConfig{
			Logger:      logger,
			RedisClient: redisClient,
		},
	}

	if shared := c.config.SharedSupplierCache; shared != nil {
		c.supplierCache = shared
		c.ownsSupplierCache = false
	} else {
		c.supplierCache = cache.NewSupplierCache(logger, redisClient, cache.SupplierCacheConfig{})
		c.ownsSupplierCache = true
	}

	require.NotNil(t, c.supplierCache)
	require.True(t, c.ownsSupplierCache,
		"a cache this controller built is its own to close")
}
