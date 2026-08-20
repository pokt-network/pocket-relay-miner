//go:build test

// Package testredis hands tests a REAL Redis 8 server instead of miniredis.
//
// miniredis is not Redis where the interesting behaviour lives. It answers a
// blocking XREADGROUP immediately rather than blocking, it does not age PEL
// entries, and its eviction and expiry are approximations. Every one of those
// gaps has already cost this repository: the consumer parked on BLOCK 0 and
// could not be shut down, and the whole suite stayed green because the fake
// never blocked. A test that asserts on those semantics against miniredis is
// not evidence.
//
// The container is started ONCE per test binary and shared, because the cost is
// the startup, not the connection: paying it per test would add seconds to
// every case. Isolation comes from flushing the database when a test acquires
// it, serialised so two tests never hold it at the same time.
//
// This is deliberately NOT the localnet's Redis. Locally that is the running
// Tilt fleet, holding live relay traffic, and a test that writes there competes
// with the thing it is meant to be measuring.
package testredis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// image is pinned to the major version production runs. Bumping it is a
// deliberate edit, not a silent "latest" drift that changes semantics under a
// suite whose whole point is matching production.
const image = "redis:8-alpine"

var (
	startOnce sync.Once
	sharedURL string
	startErr  error

	// exclusive serialises tests over the shared server: each holds it for the
	// duration of its case, so a flush never lands under another test.
	exclusive sync.Mutex
)

// start brings the container up once per test binary.
func start() (string, error) {
	startOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		container, err := tcredis.Run(ctx, image)
		if err != nil {
			startErr = err
			return
		}
		// Deliberately NOT registered with testcontainers.CleanupContainer: that
		// takes a *testing.T and would tie the shared container's life to
		// whichever test happened to start it. Removal is the reaper's job --
		// testcontainers starts Ryuk alongside and it kills the container when
		// the test session ends, including on a panic or a SIGKILL.
		sharedURL, startErr = container.ConnectionString(ctx)
	})
	return sharedURL, startErr
}

// Client returns a client on a freshly flushed real Redis, held exclusively for
// the duration of the test.
//
// It FAILS rather than skips when the container cannot start: this package
// exists because a fake produced false greens, and silently falling back to
// "not covered" would reproduce that in a different shape. A machine without
// Docker cannot run these tests, and should be told so.
func Client(t *testing.T) *redis.Client {
	t.Helper()

	url, err := start()
	if err != nil {
		t.Fatalf("could not start a real Redis (%s) for this test: %v\n"+
			"These tests assert semantics miniredis does not reproduce; Docker is required.", image, err)
	}

	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("testcontainers returned an unusable Redis URL %q: %v", url, err)
	}

	exclusive.Lock()
	t.Cleanup(exclusive.Unlock)

	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("could not flush the shared test Redis: %v", err)
	}
	return client
}

// URL returns the connection string for the shared server, for the few callers
// that build their own client (a hook, a different pool size). The database is
// NOT flushed and the server is NOT held exclusively; prefer Client.
func URL(t *testing.T) string {
	t.Helper()
	url, err := start()
	if err != nil {
		t.Fatalf("could not start a real Redis (%s): %v", image, err)
	}
	return url
}
