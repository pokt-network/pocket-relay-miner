//go:build test

package relayer

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
// Moving the base prefix moves the WHOLE namespace because the meter and the
// simulation verifier build every key through client.KB().
//
// The prefix comes back with the client because miniredis's Keys() listed the
// whole server and several tests here relied on that. The replacement is
// testredis.Keys(t, client, prefix): a test may enumerate its own subtree, and
// only its own.
func newTestRedis(t *testing.T) (*redisutil.Client, string) {
	t.Helper()

	// Fail fast with testredis's "start one with ..." message rather than
	// NewClient's bare dial error.
	testredis.Client(t)

	prefix := testredis.Prefix(t)
	client, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL:       testredis.URL(),
		Namespace: config.RedisNamespaceConfig{BasePrefix: prefix},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client, prefix
}
