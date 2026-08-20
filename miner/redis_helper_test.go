//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newTestRedis returns a redisutil.Client on the shared REAL Redis 8, with a
// namespace of its own, and that namespace's prefix.
//
// Isolation is by BasePrefix, never by flushing: the server is shared with the
// other packages `go test ./...` runs in parallel, so a FLUSHDB here would
// delete another package's keys mid-test and the failure would read as a bug
// in the code under test.
//
// Moving the base prefix moves the WHOLE namespace because the session store,
// the SMST stores, the deduplicator and the submission tracker all build their
// keys through client.KB(). A test that writes a key some other way must build
// it from the SAME client's KeyBuilder, or it writes where nothing reads.
func newTestRedis(t testing.TB) (*redisutil.Client, string) {
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

// expireNow makes Redis drop key immediately, replacing miniredis's
// FastForward.
//
// A real server has no clock to wind forward, so a test cannot ask "what
// happens once this TTL elapses" by pretending time passed. It can ask Redis to
// expire the key for real, which is what PEXPIRE 0 does — the key is gone on
// return, no sleep, no fake clock. Paired with an assertion on the TTL that was
// actually set (client.TTL), the two together cover what FastForward covered:
// the right expiry was configured, and the system behaves once it fires.
func expireNow(t *testing.T, client *redisutil.Client, keys ...string) {
	t.Helper()
	ctx := context.Background()
	for _, key := range keys {
		require.NoError(t, client.PExpire(ctx, key, 0).Err())
		n, err := client.Exists(ctx, key).Result()
		require.NoError(t, err)
		require.Zero(t, n, "key %s must be gone after PEXPIRE 0", key)
	}
}

// keyExists reports whether key is present, replacing miniredis's Exists.
func keyExists(t *testing.T, client *redisutil.Client, key string) bool {
	t.Helper()
	n, err := client.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return n == 1
}

// requireTTLNear asserts a key's remaining TTL is want, within a second.
//
// miniredis froze time, so an exact equality held. A real server starts
// counting the moment the expiry is set, and the tolerance is what that costs;
// it is small enough that a wrong TTL — a different configured window, or none
// at all — still fails.
func requireTTLNear(t *testing.T, client *redisutil.Client, key string, want time.Duration) {
	t.Helper()
	got, err := client.PTTL(context.Background(), key).Result()
	require.NoError(t, err)
	require.InDelta(t, want.Seconds(), got.Seconds(), 1.0,
		"TTL on %s: want ~%s, got %s", key, want, got)
}

// ageKeyTo leaves key with exactly remaining time to live, replacing
// miniredis's FastForward.
//
// A real server has no clock to wind forward, but the REMAINING TTL is the
// observable these tests are about, so setting it directly produces the same
// state winding the clock would have — with no sleep and no fake clock.
func ageKeyTo(t *testing.T, client *redisutil.Client, key string, remaining time.Duration) {
	t.Helper()
	ok, err := client.PExpire(context.Background(), key, remaining).Result()
	require.NoError(t, err)
	require.Truef(t, ok, "%s must exist for its TTL to be aged", key)
}
