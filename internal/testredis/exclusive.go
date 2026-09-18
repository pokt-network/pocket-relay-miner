//go:build test

package testredis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ExclusiveMaxmemoryBytes caps the container, so a test that fills it cannot
// take the machine with it. It is sized from the largest filler in the tree --
// 64 MiB of stream plus hash in the backlog test -- with room for Redis's own
// overhead and for the headroom those tests leave above `used`. A test that
// needs more should raise this and say why, not work around it.
//
// It is exported because a test that lowers maxmemory has to RAISE IT BACK to
// a valid value, and zero is not one: a store reporting maxmemory 0 is
// misconfigured, and the health gate closes on it permanently rather than
// reopening.
const ExclusiveMaxmemoryBytes = 512 << 20

// exclusiveEvictionPolicy is the policy the production code demands and these
// tests start from. It is the literal rather than transport/redis's
// storeEvictionPolicy because that constant is unexported and importing the
// package under test from here would invert the dependency.
const exclusiveEvictionPolicy = "noeviction"

// ExclusiveURL starts a Redis that belongs to THIS TEST ALONE and returns its
// URL. The container is torn down when the test ends.
//
// Use it, instead of Client, for a test that configures the SERVER: maxmemory,
// maxmemory-policy, FLUSHALL, CLIENT KILL. Those are not keys, so the prefix
// isolation the shared server relies on does not contain them -- a test that
// lowers maxmemory makes every OTHER package's writes fail with "OOM command
// not allowed", and that failure surfaces as a bug in whatever code happened to
// be writing at the time. Measured 2026-09-18: one such test held the shared
// server for 133 ms and a consumer test in another package died inside that
// window.
//
// One instance per TEST rather than per package, deliberately. Two tests in one
// file both configure the server, so per-file would let them collide the day
// one of them calls t.Parallel(); per-test cannot. It also means nothing needs
// restoring afterwards -- the container dies with the test -- which REMOVES
// rather than fixes the asymmetry the callers used to carry: their cleanups
// reset maxmemory and never maxmemory-policy.
//
// It SKIPS when Docker is unreachable, which is the one case a contributor can
// hit through no fault of their own. That skip is only safe because the gate
// counts how many of these ran and goes red on zero; without that counter a
// missing Docker would read exactly like a pass, which is the shape of failure
// this package exists to prevent.
func ExclusiveURL(t testing.TB) string {
	t.Helper()
	requireDocker(t)

	// The generic container rather than the redis module: the module is a
	// separate Go module, and one dependency is enough for this.
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:8-alpine",
			ExposedPorts: []string{"6379/tcp"},
			Cmd: []string{
				"redis-server",
				"--maxmemory", fmt.Sprintf("%d", ExclusiveMaxmemoryBytes),
				"--maxmemory-policy", exclusiveEvictionPolicy,
			},
			// Wait on the port, not on a log line: a log message is a string
			// the image can change, while a port that accepts a connection is
			// the thing the test is about to use.
			WaitingFor: wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("could not start an exclusive Redis: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			// Not fatal: Ryuk removes it when the session ends, which is the
			// reason it stays enabled. A test binary killed with SIGKILL never
			// reaches this cleanup at all, and this machine has killed two runs.
			t.Logf("could not terminate the exclusive Redis (the reaper will): %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("exclusive Redis started but has no host: %v", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("exclusive Redis started but its port is not published: %v", err)
	}
	return fmt.Sprintf("redis://%s:%s", host, port.Port())
}

// Exclusive is ExclusiveURL plus the client, for callers that do not build one
// of their own.
func Exclusive(t testing.TB) *redis.Client {
	t.Helper()

	opt, err := redis.ParseURL(ExclusiveURL(t))
	if err != nil {
		t.Fatalf("exclusive Redis URL is not a valid Redis URL: %v", err)
	}
	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// requireDocker separates "Docker is not installed" from "Docker is installed
// and refused us", because they are different repairs: install it, versus join
// the docker group or start the daemon. The shared-server helper can afford a
// bare Fatalf -- the gate starts its server -- but this one is the first thing
// a contributor meets on a fresh machine.
func requireDocker(t testing.TB) {
	t.Helper()

	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "true" {
		t.Fatal("TESTCONTAINERS_RYUK_DISABLED is set: the reaper is what removes these containers " +
			"when a test binary is killed, and a killed binary never runs its cleanup")
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed: this test configures a Redis server and needs one of its own")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Skipf("docker did not answer in 10s: %s", out)
		}
		t.Skipf("docker is installed but not usable -- daemon stopped, or this user is not in the docker group: %s", out)
	}
}
