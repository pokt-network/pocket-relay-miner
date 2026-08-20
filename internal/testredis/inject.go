//go:build test

package testredis

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Keys returns every key under prefix, sorted.
//
// It replaces miniredis's Keys(), which enumerated the WHOLE server. That is
// not available here and must not be: the server is shared with the packages
// running in parallel, so an unscoped listing would return their keys and a
// "no key was created" assertion would fail on somebody else's traffic.
func Keys(t testing.TB, client redis.UniversalClient, prefix string) []string {
	t.Helper()
	return scan(t, client, prefix+"*")
}

// KeysMatching returns every key matching a glob pattern, sorted.
//
// Unlike Keys it is NOT bounded to one namespace, so it may only be used with a
// pattern carrying a token unique to the running test -- never to delete, and
// never with a pattern another package could match. It exists for the one
// question a namespaced scan cannot answer: did the code under test write
// OUTSIDE the namespace it was configured with?
func KeysMatching(t testing.TB, client redis.UniversalClient, pattern string) []string {
	t.Helper()
	return scan(t, client, pattern)
}

// scan walks the keyspace and returns the distinct matches, sorted.
//
// The de-duplication is not defensive tidiness. SCAN guarantees a key present
// throughout the iteration is returned AT LEAST once, not exactly once: a
// rehash mid-iteration can hand the same key back twice. On a server shared by
// every package that is ordinary, and callers compare the result with
// require.Equal / ElementsMatch, so a duplicate would read as a spurious
// second key -- a flake in the helper the whole migration rests on.
func scan(t testing.TB, client redis.UniversalClient, pattern string) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	seen := map[string]struct{}{}
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			t.Fatalf("scanning test keys matching %q: %v", pattern, err)
		}
		for _, k := range keys {
			seen[k] = struct{}{}
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FailSwitch makes every command a client sends through Process or a pipeline
// fail, and lets it recover.
//
// NOT PubSub: Subscribe builds its own connection (redis.go:1206-1269) and its
// receive path goes through neither hook, so a subscriber keeps delivering
// through a simulated outage. An outage test written against Subscribe would
// pass for the wrong reason, which is the fault this package exists to remove
// -- so it needs a different mechanism, not this one.
//
// It replaces miniredis's SetError. Reproducing an unreachable Redis by
// CLOSING the server is what these tests must not do: closing frees the port,
// and another package's test binary can bind it mid-probe, at which point the
// reachability check succeeds against a foreign server and the test passes for
// the wrong reason. Failing at the client keeps the connection and breaks the
// commands, which is the condition under test.
//
// Single commands need no help: Client.Process assigns whatever the hook chain
// returns onto the Cmd (redis.go:1119-1122), and callers read cmd.Err().
// PIPELINES do not -- Pipeline.Exec returns the hook chain's error but nothing
// walks the batch (pipeline.go:104-113), so a short-circuited pipeline would
// leave every cmd.Err() nil while Exec reported failure. The pipeline hook
// therefore sets each command itself. Both statements were read out of
// go-redis v9.17.2, after an injection showed the single-command SetErr this
// helper first carried was dead code.
type FailSwitch struct {
	err atomic.Pointer[error]
}

// NewFailSwitch installs the switch on client. It starts open (commands pass).
// A go-redis hook cannot be removed once added, so the switch is a permanent
// pass-through until Fail is called.
func NewFailSwitch(client redis.UniversalClient) *FailSwitch {
	f := &FailSwitch{}
	client.AddHook(f)
	return f
}

// Fail makes every subsequent command return an error carrying msg.
func (f *FailSwitch) Fail(msg string) {
	err := errors.New(msg)
	f.err.Store(&err)
}

// Clear lets commands through again.
func (f *FailSwitch) Clear() { f.err.Store(nil) }

func (f *FailSwitch) current() error {
	if p := f.err.Load(); p != nil {
		return *p
	}
	return nil
}

// DialHook passes through: the connection itself stays healthy on purpose.
func (f *FailSwitch) DialHook(next redis.DialHook) redis.DialHook { return next }

func (f *FailSwitch) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := f.current(); err != nil {
			return err // Client.Process puts this on the Cmd.
		}
		return next(ctx, cmd)
	}
}

func (f *FailSwitch) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if err := f.current(); err != nil {
			// Exec returns this, but nothing propagates it to the batch.
			for _, cmd := range cmds {
				cmd.SetErr(err)
			}
			return err
		}
		return next(ctx, cmds)
	}
}

// DeletePrefix removes every key under prefix.
//
// It replaces miniredis's FlushAll, which suites and benchmarks called between
// iterations. FLUSHALL and FLUSHDB are forbidden here and always will be: the
// server is shared with every other package, so flushing it would delete their
// keys mid-test and the failure would surface as a bug in their code. Deleting
// one subtree is the same intent expressed in a way that cannot reach anybody
// else's data.
func DeletePrefix(t testing.TB, client redis.UniversalClient, prefix string) {
	t.Helper()

	keys := scan(t, client, prefix+"*")
	if len(keys) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// In batches: one DEL with tens of thousands of arguments is a long
	// single-threaded command on a server other packages are waiting on.
	const batch = 500
	for start := 0; start < len(keys); start += batch {
		end := min(start+batch, len(keys))
		if err := client.Del(ctx, keys[start:end]...).Err(); err != nil {
			t.Fatalf("deleting test keys under %q: %v", prefix, err)
		}
	}
}
