//go:build test

package redis

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// The read loop used to park on XREADGROUP with BLOCK 0 and a comment claiming
// context cancellation would interrupt it. It does not. Verified against
// go-redis v9.17.2: for a blocking command cmdTimeout returns 0 (redis.go:751),
// the context handed to the reader is context.Background() unless
// ContextTimeoutEnabled is set and it defaults to false (redis.go:764), and
// deadline(Background, 0) returns noDeadline (internal/pool/conn.go). No read
// deadline is set at all, so Close() cancelled the context and then sat in
// wg.Wait() until a relay happened to arrive -- on an idle supplier, until
// Kubernetes ran out of grace and killed the pod.
//
// These run against a REAL Redis 8 (internal/testredis). Under miniredis the
// suite stayed green while production hung, because the fake answers a blocking
// read immediately -- asserting blocking semantics against it proves nothing,
// which is the whole reason this bug survived.

// blockArgRecorder captures the BLOCK argument of every XREADGROUP the client
// issues, which is the value whose being zero caused the hang.
type blockArgRecorder struct {
	mu     sync.Mutex
	blocks []string
	first  chan struct{}
	once   sync.Once
}

func (r *blockArgRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *blockArgRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (r *blockArgRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if len(args) > 0 && strings.EqualFold(fmt.Sprint(args[0]), "xreadgroup") {
			for i, a := range args {
				if strings.EqualFold(fmt.Sprint(a), "block") && i+1 < len(args) {
					r.mu.Lock()
					r.blocks = append(r.blocks, fmt.Sprint(args[i+1]))
					r.mu.Unlock()
					r.once.Do(func() { close(r.first) })
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (r *blockArgRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.blocks...)
}

func TestConsumeSendsAFiniteBlockOnTheWire(t *testing.T) {
	restore := blockInterval
	blockInterval = 200 * time.Millisecond
	t.Cleanup(func() { blockInterval = restore })

	// Namespaced: the server is shared with the packages running alongside, and
	// production-shaped keys here would land in whatever Redis REDIS_TEST_URL
	// points at.
	testredis.Client(t) // fail fast with the "start one with..." message
	prefix := testredis.Prefix(t)

	// A client of our own so the hook sees only this test's traffic.
	opt, err := redis.ParseURL(testredis.URL())
	require.NoError(t, err)
	rec := &blockArgRecorder{first: make(chan struct{})}
	client := redis.NewClient(opt)
	client.AddHook(rec)
	t.Cleanup(func() { _ = client.Close() })

	consumer, err := NewStreamsConsumer(
		logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "json"}),
		client,
		transport.ConsumerConfig{
			StreamPrefix:            prefix,
			SupplierOperatorAddress: "pokt1block_arg",
			ConsumerGroup:           prefix + ":ha-miners",
			ConsumerName:            "c1",
			BatchSize:               10,
			ClaimIdleTimeout:        30000,
		},
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	ch := consumer.Consume(ctx)

	select {
	case <-rec.first:
	case <-time.After(10 * time.Second):
		t.Fatal("no XREADGROUP reached the wire")
	}

	cancel()

	// Bounded, because the failure this file guards against is a read loop that
	// never notices the cancel: an unbounded drain would hang the whole binary
	// to the global test timeout instead of reporting here.
	drained := make(chan error, 1)
	go func() {
		for range ch { //nolint:revive // drain until the coordinator closes it
		}
		drained <- consumer.Close()
	}()
	select {
	case err := <-drained:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("the delivery channel never closed: the read loop cannot see a cancelled context")
	}

	for _, b := range rec.seen() {
		require.NotEqual(t, "0", b,
			"BLOCK 0 sets no read deadline at all, so a cancelled context cannot end the read and Close() hangs")
	}
}

// TestCloseReturnsWhileTheStreamIsIdle is the test that would have caught the
// bug. Against a real server the read genuinely parks on an idle stream, so
// Close() has to be what ends it. Injecting the defect (Block: 0 in the read)
// makes this time out rather than fail an assertion -- which is what the bug
// looked like in production too.
func TestCloseReturnsWhileTheStreamIsIdle(t *testing.T) {
	client := testredis.Client(t)
	prefix := testredis.Prefix(t)

	restore := blockInterval
	blockInterval = 200 * time.Millisecond
	t.Cleanup(func() { blockInterval = restore })

	consumer, err := NewStreamsConsumer(
		logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "json"}),
		client,
		transport.ConsumerConfig{
			StreamPrefix:            prefix,
			SupplierOperatorAddress: "pokt1idle_close",
			ConsumerGroup:           prefix + ":ha-miners",
			ConsumerName:            "c1",
			BatchSize:               10,
			ClaimIdleTimeout:        30000,
		},
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	ch := consumer.Consume(ctx)

	// Nothing is ever written to this stream: the read loop is parked in
	// XREADGROUP, which is the state the old code could not leave.
	cancel()

	done := make(chan error, 1)
	go func() {
		for range ch { //nolint:revive // drain until the coordinator closes it
		}
		done <- consumer.Close()
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close() did not return on an idle stream: the read loop cannot see a cancelled context")
	}
}

// TestBlockIntervalIsBounded pins the two properties the value has to keep. It
// is deliberately not a golden number: the point is that it is neither infinite
// nor so long that shutdown outlives a termination grace period.
func TestBlockIntervalIsBounded(t *testing.T) {
	require.Greater(t, blockInterval, time.Duration(0),
		"zero means no read deadline, which is the hang this constant exists to prevent")
	require.LessOrEqual(t, blockInterval, 15*time.Second,
		"this bounds how long Close() waits for the read loop; it must sit well inside a termination grace period")
}
