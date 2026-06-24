//go:build test

package cache

import (
	"sort"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/require"
)

// TestMergeKnownFromRedis is the regression guard for the dead known-services
// discovery + the updateKnown*Set Del+SAdd clobber. The leader must union the
// Redis known-set (which recordDiscovered populates from relay traffic on any
// replica) into its in-memory set, so a service/app first seen by a non-leader
// replica is force-refreshed + pub/sub-invalidated instead of only following the
// consumer's local TTL.
func TestMergeKnownFromRedis(t *testing.T) {
	o := &CacheOrchestrator{}

	known := xsync.NewMap[string, struct{}]()
	// Pre-existing in-memory entry (e.g. a config-warmed app/service).
	known.Store("develop-http", struct{}{})

	// Redis carries an entry discovered by another replica's relay traffic that the
	// leader's in-memory set has never seen.
	merged := o.mergeKnownFromRedis(known, []string{"develop-grpc", "develop-http"})

	sort.Strings(merged)
	require.Equal(t, []string{"develop-grpc", "develop-http"}, merged,
		"merge must return the union of in-memory and Redis known-sets (deduped)")

	// The newly discovered entry must now be in the in-memory set so the next
	// refresh force-refreshes it (this is what populates L2 + publishes invalidation).
	_, ok := known.Load("develop-grpc")
	require.True(t, ok, "a Redis-discovered entry must be promoted into the in-memory known-set")
}

// TestMergeKnownFromRedis_EmptyRedis verifies the no-discovery path: an empty
// Redis set must not drop existing in-memory entries (no clobber).
func TestMergeKnownFromRedis_EmptyRedis(t *testing.T) {
	o := &CacheOrchestrator{}

	known := xsync.NewMap[string, struct{}]()
	known.Store("develop-http", struct{}{})

	merged := o.mergeKnownFromRedis(known, nil)
	require.Equal(t, []string{"develop-http"}, merged,
		"an empty Redis set must preserve in-memory entries (no clobber)")
}
