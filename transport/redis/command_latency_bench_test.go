//go:build test

package redis

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// BenchmarkCommandWithoutHook and BenchmarkCommandWithHook measure what the
// latency hook costs on the hot path: the SAME command, against the SAME real
// Redis, with the hook absent and present.
//
// It exists because "the cost is negligible" is a claim about the world, and
// this repository does not let one into a commit message unmeasured. A hook
// runs on every Redis command of a binary aiming at thousands of relays a
// second, so "negligible" has to be a number somebody can re-derive.
//
// READ THE ISOLATED ONE. BenchmarkCommandWithHook and ...WithoutHook both issue
// SET against a real server, and the round trip dominates so heavily that its
// run-to-run spread is LARGER than the effect being measured: the no-hook case
// alone ranged 438-665 microseconds across three runs, against a hook cost of
// well under a microsecond. Those two benchmarks can therefore show only that
// the hook adds nothing of the round trip's order -- they cannot resolve its
// actual cost, and reporting their difference as the overhead would be
// reporting noise.
//
// BenchmarkHookObserveOnly is the one with an answer: a no-op next leaves the
// timer and the histogram observation, which is exactly the work that scales
// with command rate.
func benchSet(b *testing.B, client *Client) {
	b.Helper()
	ctx := context.Background()
	key := testredis.Prefix(b) + ":bench"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.Set(ctx, key, "v", 0).Err(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchClient(b *testing.B, withHook bool) *Client {
	b.Helper()
	url := testRedisURLBench(b)
	client, err := NewClient(context.Background(), ClientConfig{URL: url})
	if err != nil {
		b.Skipf("no real Redis: %v", err)
	}
	b.Cleanup(func() { _ = client.Close() })
	if withHook {
		client.AddHook(NewCommandLatencyHook("bench"))
	}
	return client
}

func BenchmarkCommandWithoutHook(b *testing.B) { benchSet(b, benchClient(b, false)) }
func BenchmarkCommandWithHook(b *testing.B)    { benchSet(b, benchClient(b, true)) }

// BenchmarkHookObserveOnly isolates the hook's own work from the round trip: a
// no-op "next" leaves only the timer and the histogram observation, which is
// the number that scales with command rate.
func BenchmarkHookObserveOnly(b *testing.B) {
	h := NewCommandLatencyHook("bench-isolated")
	next := h.ProcessHook(func(context.Context, goredis.Cmder) error { return nil })
	cmd := goredis.NewStatusCmd(context.Background(), "set", "k", "v")
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = next(ctx, cmd)
	}
}
