//go:build test

// Package testredis hands tests a REAL Redis 8 instead of miniredis.
//
// miniredis is not Redis where the interesting behaviour lives. It answers a
// blocking XREADGROUP immediately rather than blocking, it does not age PEL
// entries, and its expiry and eviction are approximations. That gap is not
// theoretical here: the stream consumer parked on XREADGROUP BLOCK 0 and could
// not be shut down, and the entire suite stayed green because the fake never
// blocked. A test asserting on those semantics against miniredis is not
// evidence.
//
// The server is supplied by the environment, not started per test:
// scripts/gates/redis.sh brings ONE container up for the whole run and exports
// REDIS_TEST_URL. Starting a container per package instead is what the first
// attempt did, and `go test ./...` runs package binaries in parallel, so the
// container-plus-reaper storm timed the gate out.
//
// It is deliberately not the localnet's Redis either: locally that is the
// running Tilt fleet holding live relay traffic, and a test writing there
// competes with the thing it is measuring.
//
// Isolation is by KEY PREFIX, never by flushing. A shared server means a
// FLUSHDB from one package would delete another package's keys mid-test, and
// the failure would look like a bug in the code under test.
package testredis

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// defaultURL is where scripts/gates/redis.sh publishes the gate's server. It is
// NOT 6379: that is the localnet's Redis on a developer machine.
const defaultURL = "redis://127.0.0.1:6399"

// prefixSeq keeps prefixes unique within a test binary; the test name and the
// process start time keep them unique across binaries.
var prefixSeq atomic.Uint64

// URL returns the server these tests must use.
func URL() string {
	if u := os.Getenv("REDIS_TEST_URL"); u != "" {
		return u
	}
	return defaultURL
}

// Client returns a client on the shared real Redis.
//
// It FAILS rather than skips when nothing answers. This package exists because
// a fake produced false greens, and a silent "not covered" would reproduce that
// in another shape: run `eval "$(scripts/gates/redis.sh up)"` first, or let
// `make gate` do it.
func Client(t testing.TB) *redis.Client {
	t.Helper()

	opt, err := redis.ParseURL(URL())
	if err != nil {
		t.Fatalf("REDIS_TEST_URL %q is not a valid Redis URL: %v", URL(), err)
	}
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("no real Redis at %s: %v\n"+
			"These tests assert semantics miniredis does not reproduce. "+
			"Start one with: eval \"$(scripts/gates/redis.sh up)\"", URL(), err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// Prefix returns a key prefix unique to this test, and deletes everything under
// it afterwards. Every key a test creates must sit beneath it, SEPARATED BY A
// COLON: the server is shared with the other packages running in parallel.
//
// The colon is not cosmetic. The prefix ends in a sequence number, so "t:X:N:1"
// is a textual prefix of "t:X:N:11"; matching prefix+"*" would let one test's
// cleanup delete a sibling's keys. Only the nanosecond timestamp keeps those
// apart today, which is a coincidence and not a guarantee. Matching
// prefix+":*" makes the boundary structural.
func Prefix(t testing.TB) string {
	t.Helper()

	// Redis glob metacharacters are replaced too, not only the separators:
	// the cleanup below deletes by SCAN MATCH prefix+"*", so a subtest name
	// carrying "[", "?" or "*" would make that pattern match a DIFFERENT set
	// of keys -- either none (this test's keys leak onto the shared server)
	// or a wider one (another package's keys deleted mid-run). That is the
	// exact damage isolation-by-prefix exists to prevent.
	safe := strings.NewReplacer(
		"/", "_", " ", "_", ":", "_",
		"*", "_", "?", "_", "[", "_", "]", "_", "^", "_", `\`, "_",
	).Replace(t.Name())
	prefix := fmt.Sprintf("t:%s:%d:%d", safe, time.Now().UnixNano(), prefixSeq.Add(1))

	t.Cleanup(func() {
		client := Client(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, prefix+":*", 500).Result()
			if err != nil {
				t.Logf("could not clean up test keys under %q: %v", prefix, err)
				return
			}
			if len(keys) > 0 {
				if err := client.Del(ctx, keys...).Err(); err != nil {
					t.Logf("could not delete test keys under %q: %v", prefix, err)
					return
				}
			}
			if next == 0 {
				return
			}
			cursor = next
		}
	})
	return prefix
}
