//go:build test

package miner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// cmdCounter counts, by command name, every Redis command a client ATTEMPTS.
// Registered first so it wraps the outside of the hook chain: a command that a
// deeper hook fails is still counted, which is what the "does not fall back"
// test needs -- it asserts that a second command was never even issued.
type cmdCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCmdCounter() *cmdCounter { return &cmdCounter{counts: make(map[string]int)} }

func (c *cmdCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *cmdCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.mu.Lock()
		c.counts[strings.ToLower(cmd.Name())]++
		c.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (c *cmdCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (c *cmdCounter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

func (c *cmdCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.counts {
		n += v
	}
	return n
}

// failNamedCmd fails one command by name with a chosen error, leaving every
// other command alone.
type failNamedCmd struct {
	name string
	err  error
}

func (h failNamedCmd) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h failNamedCmd) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if strings.ToLower(cmd.Name()) == h.name {
			cmd.SetErr(h.err)
			return h.err
		}
		return next(ctx, cmd)
	}
}

func (h failNamedCmd) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestGet_HashReadCostsOneCommand pins the point of the change: reading a
// session that lives in the hash layout is a single HGETALL. The TYPE this
// replaced ran once per relay on a serial per-supplier consumer.
func TestGet_HashReadCostsOneCommand(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-hash", SessionStateActive, 0, 0)

	counter := newCmdCounter()
	client.AddHook(counter)

	snapshot, err := store.Get(ctx, "sess-hash")
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.Equal(t, "sess-hash", snapshot.SessionID)
	require.Equal(t, SessionStateActive, snapshot.State)

	require.Equal(t, 1, counter.count("hgetall"), "a hash read is one HGETALL")
	require.Equal(t, 0, counter.count("type"), "the TYPE probe is gone")
	require.Equal(t, 1, counter.total(), "no other command runs on this path")
}

// TestGet_MissingKeyCostsOneCommandAndReturnsNil covers what the TYPE probe
// used to answer with its "none" branch. HGETALL on a missing key is an empty
// map and no error, and decodeSnapshot turns that into (nil, nil).
func TestGet_MissingKeyCostsOneCommandAndReturnsNil(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	counter := newCmdCounter()
	client.AddHook(counter)

	snapshot, err := store.Get(ctx, "sess-does-not-exist")
	require.NoError(t, err, "a missing session is not an error")
	require.Nil(t, snapshot)

	require.Equal(t, 1, counter.count("hgetall"))
	require.Equal(t, 1, counter.total(), "a missing key costs one command, not two")
}

// TestGet_LegacyJSONStringCostsTwoCommands proves the rolling-upgrade branch
// still works without the TYPE probe: the WRONGTYPE that HGETALL raises
// against a string key is what routes the read to the legacy decoder.
func TestGet_LegacyJSONStringCostsTwoCommands(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	legacy := SessionSnapshot{
		SessionID:               "sess-legacy",
		SupplierOperatorAddress: "pokt1test",
		ServiceID:               "svc-test",
		ApplicationAddress:      "pokt1app",
		SessionStartHeight:      100,
		SessionEndHeight:        110,
		State:                   SessionStateActive,
	}
	blob, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, store.sessionKey("sess-legacy"), blob, 0).Err())

	counter := newCmdCounter()
	client.AddHook(counter)

	snapshot, err := store.Get(ctx, "sess-legacy")
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.Equal(t, "sess-legacy", snapshot.SessionID)
	require.Equal(t, SessionStateActive, snapshot.State)

	require.Equal(t, 1, counter.count("hgetall"), "the hash read is attempted first")
	require.Equal(t, 1, counter.count("get"), "and WRONGTYPE routes it to the legacy read")
	require.Equal(t, 2, counter.total())
}

// TestGet_NonWrongTypeErrorDoesNotFallBackToLegacy is the one that matters when
// Redis is unhealthy: a failure that is NOT a WRONGTYPE must surface as itself.
// Falling back would issue a second command, and report ITS error instead of
// the real one -- turning "the connection is gone" into "no such session".
func TestGet_NonWrongTypeErrorDoesNotFallBackToLegacy(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	saveTestSession(t, store, "sess-broken", SessionStateActive, 0, 0)

	counter := newCmdCounter()
	client.AddHook(counter) // outermost: counts attempts, including failed ones

	boom := errors.New("connection reset by peer")
	client.AddHook(failNamedCmd{name: "hgetall", err: boom})

	snapshot, err := store.Get(ctx, "sess-broken")
	require.Error(t, err)
	require.Nil(t, snapshot)
	require.ErrorIs(t, err, boom, "the HGETALL error is what surfaces")

	require.Equal(t, 1, counter.count("hgetall"))
	require.Equal(t, 0, counter.count("get"), "a non-WRONGTYPE error must not try the legacy read")
	require.Equal(t, 1, counter.total())
}

// TestGet_ErrorNamesTheKey keeps the one thing the TYPE probe's error message
// gave an operator: which key was wrong. The type is no longer knowable without
// spending the round trip this change removed; the location still is.
func TestGet_ErrorNamesTheKey(t *testing.T) {
	store, client := setupTestSessionStore(t)
	ctx := context.Background()

	client.AddHook(failNamedCmd{name: "hgetall", err: errors.New("connection reset by peer")})

	_, err := store.Get(ctx, "sess-named")
	require.Error(t, err)
	require.Contains(t, err.Error(), store.sessionKey("sess-named"))
}
