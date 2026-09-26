//go:build test

package testredis

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// usedMemoryOf reads used_memory out of INFO memory, which is how the callers
// of this helper size their own maxmemory cut.
func usedMemoryOf(t *testing.T, ctx context.Context, client *redis.Client) uint64 {
	t.Helper()
	info, err := client.Info(ctx, "memory").Result()
	require.NoError(t, err)
	for _, line := range strings.Split(info, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "used_memory:")
		if !ok {
			continue
		}
		used, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		require.NoError(t, err)
		return used
	}
	t.Fatal("INFO memory carried no used_memory")
	return 0
}

// captureMemoryConfig reads the server's memory settings and returns a func
// that puts them back. Both settings, not just maxmemory: no cleanup in this
// tree restores maxmemory-policy, which is harmless only because every server
// happens to start at noeviction -- an assumption nothing checks.
func captureMemoryConfig(t *testing.T, ctx context.Context, client *redis.Client) func() {
	t.Helper()
	maxmemory, err := client.ConfigGet(ctx, "maxmemory").Result()
	require.NoError(t, err)
	policy, err := client.ConfigGet(ctx, "maxmemory-policy").Result()
	require.NoError(t, err)
	return func() {
		bg := context.Background()
		_ = client.ConfigSet(bg, "maxmemory", maxmemory["maxmemory"]).Err()
		_ = client.ConfigSet(bg, "maxmemory-policy", policy["maxmemory-policy"]).Err()
	}
}

// TestExclusive_ConfiguringItCannotReachTheSharedServer is the whole point of
// the helper, stated as a test: a test that lowers maxmemory on ITS server
// must leave the shared one able to write.
//
// It goes red without the helper, because that is what the tree did until
// today -- both clients pointed at the same server, so the write below failed
// with "OOM command not allowed" and the failure landed on whichever package
// happened to be writing at the time.
//
// LINK: exclusive-does-not-reach-the-shared-server
func TestExclusive_ConfiguringItCannotReachTheSharedServer(t *testing.T) {
	ctx := context.Background()

	mine := Exclusive(t)
	shared := Client(t)
	prefix := Prefix(t)

	// Put back WHAT WAS THERE, read before touching anything.
	//
	// The cleanup is not for the happy path -- this server is disposable and
	// dies with the test. It is for the day ExclusiveURL hands back something
	// else: a bug, a bad REDIS_TEST_URL, or an injected fault while proving
	// this very test bites. Measured 2026-09-18: a tooth that made
	// ExclusiveURL return the SHARED server left it at maxmemory 2.8 MB and
	// every other package's writes began failing with OOM -- the defect this
	// file exists to prevent, reproduced by the check that verifies the
	// prevention.
	//
	// Restoring a CONSTANT instead would assume which server this is. The
	// shared one runs with maxmemory 0 (scripts/gates/redis.sh starts it with
	// no memory flags), and putting 512 MiB there breaks
	// TestStoreHealth_RealMaxmemory... in the other direction: it asserts the
	// preflight REFUSES a store reporting 0. Reading the value first is
	// correct on either server without knowing which one it got.
	restore := captureMemoryConfig(t, ctx, mine)
	t.Cleanup(restore)

	// Fill my own server to its limit, the way the callers of this helper do.
	used := usedMemoryOf(t, ctx, mine)
	require.NoError(t, mine.ConfigSet(ctx, "maxmemory", strconv.FormatUint(used, 10)).Err())

	require.Error(t, mine.Set(ctx, "canary", strings.Repeat("v", 1<<20), 0).Err(),
		"precondition: my own server must now refuse writes, or this test proves nothing")

	require.NoError(t, shared.Set(ctx, prefix+":canary", "v", 0).Err(),
		"LINK exclusive-does-not-reach-the-shared-server: the shared server must be untouched by my maxmemory")
}

// TestExclusive_StartsCappedAndWithoutEviction pins the two settings the
// production code demands, so a caller does not have to set them before it can
// measure anything.
//
// LINK: exclusive-starts-configured
func TestExclusive_StartsCappedAndWithoutEviction(t *testing.T) {
	ctx := context.Background()
	client := Exclusive(t)

	maxmemory, err := client.ConfigGet(ctx, "maxmemory").Result()
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(ExclusiveMaxmemoryBytes), maxmemory["maxmemory"],
		"LINK exclusive-starts-configured: the container is capped, so a test that fills it cannot take the machine")

	policy, err := client.ConfigGet(ctx, "maxmemory-policy").Result()
	require.NoError(t, err)
	require.Equal(t, exclusiveEvictionPolicy, policy["maxmemory-policy"],
		"LINK exclusive-starts-configured: noeviction, or Redis drops the nodes a claim is proved from")
}

// TestExclusive_TwoTestsGetDifferentServers is the property per-file
// granularity would not have: two tests configuring the server at once cannot
// see each other.
//
// LINK: exclusive-is-per-test
func TestExclusive_TwoTestsGetDifferentServers(t *testing.T) {
	ctx := context.Background()
	first := Exclusive(t)
	second := Exclusive(t)

	require.NoError(t, first.Set(ctx, "only-in-first", "v", 0).Err())

	err := second.Get(ctx, "only-in-first").Err()
	require.ErrorIs(t, err, redis.Nil,
		"LINK exclusive-is-per-test: two exclusive servers must not be the same server")
}
