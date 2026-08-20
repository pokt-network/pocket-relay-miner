//go:build test

package cache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newTestRedis returns a redisutil.Client on the shared REAL Redis 8, with a
// namespace of its own.
//
// Isolation is by BasePrefix, never by flushing: the server is shared with the
// other packages `go test ./...` runs in parallel, so a FLUSHDB here would
// delete another package's keys mid-test and the failure would read as a bug
// in the code under test.
//
// Moving the base prefix is enough to move the WHOLE namespace because every
// key and channel in this package is built through client.KB() — cache keys,
// lock keys, known-sets and the invalidation channels alike. A test that
// writes a key some other way must build it from the SAME client's KeyBuilder,
// or it will write outside the namespace the cache reads.
func newTestRedis(t *testing.T) *redisutil.Client {
	t.Helper()

	// Fail fast with testredis's "start one with ..." message rather than
	// NewClient's bare dial error.
	testredis.Client(t)

	client, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL:       testredis.URL(),
		Namespace: config.RedisNamespaceConfig{BasePrefix: testredis.Prefix(t)},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}
