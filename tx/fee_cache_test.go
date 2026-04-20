//go:build test

package tx

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestFeeCache_InvalidateOnSuccessfulClaim verifies that the cached
// estimated claim+proof fee is dropped after a successful claim broadcast.
//
// Rationale: the cache stores a chain-observed fee that, during a transient
// spike, can be much higher than the fee the supplier actually paid on the
// following submission. Holding on to the spike for the full cache TTL
// drives economic-viability to skip sessions whose reward sits between the
// real and the cached value — lost claims despite healthy relay inflow.
//
// The fix invalidates the cache on every successful CreateClaims so the
// next GetEstimatedFeeUpokt re-queries the chain.
func TestFeeCache_InvalidateOnSuccessfulClaim(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	client := &HASupplierClient{
		operatorAddr: "pokt1test",
		logger:       logger,
	}

	// Seed the cache with an arbitrarily high "stale spike" value.
	client.feeCacheMu.Lock()
	client.feeCacheUpokt = 9999
	client.feeCacheTime = time.Now()
	client.feeCacheMu.Unlock()

	// Sanity: cache is populated.
	client.feeCacheMu.RLock()
	require.Equal(t, uint64(9999), client.feeCacheUpokt)
	require.False(t, client.feeCacheTime.IsZero())
	client.feeCacheMu.RUnlock()

	// Simulate the post-submit invalidation path.
	client.InvalidateFeeCache()

	// The cache must now be empty so the next GetEstimatedFeeUpokt
	// re-queries the chain instead of returning the stale spike.
	client.feeCacheMu.RLock()
	require.Equal(t, uint64(0), client.feeCacheUpokt, "fee cache must be cleared after successful submit")
	require.True(t, client.feeCacheTime.IsZero(), "fee cache timestamp must be cleared")
	client.feeCacheMu.RUnlock()
}

// TestFeeCache_InvalidateIsIdempotent asserts repeated invalidation is safe
// (no panic, stays cleared). Clean-up paths on retry loops can call this
// more than once per session.
func TestFeeCache_InvalidateIsIdempotent(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	client := &HASupplierClient{
		operatorAddr: "pokt1test",
		logger:       logger,
	}

	client.InvalidateFeeCache()
	client.InvalidateFeeCache()

	client.feeCacheMu.RLock()
	require.Equal(t, uint64(0), client.feeCacheUpokt)
	require.True(t, client.feeCacheTime.IsZero())
	client.feeCacheMu.RUnlock()
}
