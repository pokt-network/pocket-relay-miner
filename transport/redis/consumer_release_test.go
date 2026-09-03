//go:build test

package redis

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// ReleaseMessage has two paths and the deployed cluster only ever takes the
// second one. Measured 2026-09-02: redis-standalone-0 reports 8.4.6 and
// COMMAND INFO XNACK comes back empty, while the Redis these tests run against
// is 8.10.0 and has XNACK. So a test written the obvious way returns at the
// `if err == nil` after XNACK succeeds and never reaches the fallback -- which
// is the only code the fix changed. It would pass without exercising anything.
//
// xnackKiller makes the fallback reachable by failing exactly one command with
// the error an older server sends, and letting every other command through to
// the real server. The XCLAIM the fallback then issues runs for real. This is
// the hook pattern the package already uses in consumer_block_test.go:51.
type xnackKiller struct{}

func (xnackKiller) DialHook(next redis.DialHook) redis.DialHook { return next }

func (xnackKiller) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (xnackKiller) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if len(args) > 0 && strings.EqualFold(fmt.Sprint(args[0]), "xnack") {
			// The shape Redis sends for a command it does not have. What
			// matters is the "unknown command" substring: ReleaseMessage
			// treats a NOPERM as a deliberate ACL decision and refuses to
			// degrade past it, so only this text reaches the fallback.
			return fmt.Errorf("ERR unknown command 'XNACK', with args beginning with: ")
		}
		return next(ctx, cmd)
	}
}

// releaseFixture is one delivered, unacknowledged entry owned by "me".
type releaseFixture struct {
	consumer      *StreamsConsumer
	client        redis.UniversalClient
	msg           transport.StreamMessage
	stream, group string
}

const releaseConsumerName = "me"

// newReleaseFixture wires a consumer against a real Redis stream. hookXNackOut
// asks for a client that behaves like a pre-8.8 server for XNACK alone.
func newReleaseFixture(t *testing.T, hookXNackOut bool) *releaseFixture {
	t.Helper()
	ctx := context.Background()

	testredis.Client(t) // fail fast with the "start one with..." message
	prefix := testredis.Prefix(t)
	stream, group := prefix+":relays", prefix+":group"

	// A client of our own, because AddHook cannot be removed and the shared
	// client is used by every other test in this package.
	opt, err := redis.ParseURL(testredis.URL())
	require.NoError(t, err)
	client := redis.NewClient(opt)
	if hookXNackOut {
		client.AddHook(xnackKiller{})
	}
	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	require.NoError(t, client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream, Values: map[string]any{"data": []byte("x")},
	}).Err())

	read, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: releaseConsumerName, Streams: []string{stream, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)
	require.Len(t, read[0].Messages, 1, "premise: the entry is delivered and unacked")

	return &releaseFixture{
		client: client,
		consumer: &StreamsConsumer{
			client:     client,
			streamName: stream,
			config: transport.ConsumerConfig{
				ConsumerGroup: group,
				ConsumerName:  releaseConsumerName,
				// 60s: longer than the entry's real age, shorter than the
				// releaseIdleMillis the fallback writes. Both filters below
				// therefore depend on the release having set IDLE.
				ClaimIdleTimeout: 60_000,
			},
		},
		msg:    transport.StreamMessage{ID: read[0].Messages[0].ID, StreamName: stream},
		stream: stream,
		group:  group,
	}
}

func (f *releaseFixture) ownerOf(t *testing.T, id string) string {
	t.Helper()
	pending, err := f.client.XPendingExt(context.Background(), &redis.XPendingExtArgs{
		Stream: f.stream, Group: f.group, Start: "-", End: "+", Count: 10,
	}).Result()
	require.NoError(t, err)
	for _, e := range pending {
		if e.ID == id {
			return e.Consumer
		}
	}
	return ""
}

// TestReleaseMessageOnPre88ServerHandsTheEntryBackToItsOwnReclaim is the
// regression test for a release that released nothing.
//
// The fallback used to XCLAIM the entry back to c.config.ConsumerName. The
// owner therefore never changed, and claimIdleFromOtherConsumers skips any
// entry it already owns ("our own in-flight delivery, not a stranded one",
// consumer.go:443-446) -- so on a single-miner fleet, where no other consumer
// exists to rescue it, a released relay stayed stranded until the process
// restarted and its name changed. If the claim window closed first, the relay
// had been served and was never billed.
//
// The assertion is deliberately NOT "the owner is the sentinel", which would
// only restate the fix. It is the round trip through the production filter:
// release it, then ask THIS consumer's own reclaim for it back.
func TestReleaseMessageOnPre88ServerHandsTheEntryBackToItsOwnReclaim(t *testing.T) {
	f := newReleaseFixture(t, true)
	ctx := context.Background()

	require.Equal(t, releaseConsumerName, f.ownerOf(t, f.msg.ID),
		"premise: the entry starts owned by this consumer")

	require.NoError(t, f.consumer.ReleaseMessage(ctx, f.msg))

	msgs, _, err := f.consumer.claimIdleFromOtherConsumers(ctx, "0-0")
	require.NoError(t, err)
	require.Len(t, msgs, 1,
		"the consumer that let go must be able to take it back: with the entry "+
			"claimed to itself the reclaim filter skips it and this returns nothing")
	require.Equal(t, f.msg.ID, msgs[0].ID)
}

// TestReleaseMessageOnPre88ServerParksUnderTheSentinel is the diagnostic half.
// It says WHY the test above passes, so a failure there is readable without
// reaching for a debugger.
func TestReleaseMessageOnPre88ServerParksUnderTheSentinel(t *testing.T) {
	f := newReleaseFixture(t, true)

	require.NoError(t, f.consumer.ReleaseMessage(context.Background(), f.msg))

	require.Equal(t, releasedConsumerName, f.ownerOf(t, f.msg.ID),
		"the fallback parks the entry under an owner that is nobody real")
}

// TestReleaseMessageOnModernServerLeavesTheEntryUnowned pins the other path.
// From 8.8.0 XNACK SILENT marks the entry unowned outright, so the sentinel is
// never needed -- and both paths converge on the property that matters: an
// entry this consumer can claim back.
func TestReleaseMessageOnModernServerLeavesTheEntryUnowned(t *testing.T) {
	f := newReleaseFixture(t, false)
	ctx := context.Background()

	if err := f.consumer.ReleaseMessage(ctx, f.msg); err != nil {
		require.Contains(t, err.Error(), "unknown command",
			"a server without XNACK is the only acceptable failure here")
		t.Skip("this Redis has no XNACK; the pre-8.8 path is covered by the tests above")
	}

	require.Empty(t, f.ownerOf(t, f.msg.ID), "XNACK SILENT leaves the entry with no owner")

	msgs, _, err := f.consumer.claimIdleFromOtherConsumers(ctx, "0-0")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "and it is claimable again by this very consumer")
	require.Equal(t, f.msg.ID, msgs[0].ID)
}

// TestReleaseMessageRefusesAfterClose guards the contract the caller relies on:
// a closed consumer must not report that it handed anything over. The drain
// counts a release failure as abandoned, which leaves the entry pending -- the
// safe outcome.
func TestReleaseMessageRefusesAfterClose(t *testing.T) {
	f := newReleaseFixture(t, true)
	f.consumer.closed = true

	require.Error(t, f.consumer.ReleaseMessage(context.Background(), f.msg))
	require.Equal(t, releaseConsumerName, f.ownerOf(t, f.msg.ID),
		"and it must not have touched the entry")
}
