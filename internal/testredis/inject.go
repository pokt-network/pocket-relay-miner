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
func Keys(t *testing.T, client redis.UniversalClient, prefix string) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		out    []string
		cursor uint64
	)
	for {
		keys, next, err := client.Scan(ctx, cursor, prefix+"*", 500).Result()
		if err != nil {
			t.Fatalf("scanning test keys under %q: %v", prefix, err)
		}
		out = append(out, keys...)
		if next == 0 {
			break
		}
		cursor = next
	}
	sort.Strings(out)
	return out
}

// FailSwitch makes every command on a client fail, and lets it recover.
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
