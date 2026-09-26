//go:build test

package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// xnackFailer fails exactly the XNACK command with a fixed error and lets every
// other command through to the real server. It is the hook pattern the package
// already uses in consumer_block_test.go.
type xnackFailer struct{ err error }

func (xnackFailer) DialHook(next redis.DialHook) redis.DialHook { return next }

func (xnackFailer) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f xnackFailer) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if len(args) > 0 && strings.EqualFold(fmt.Sprint(args[0]), "xnack") {
			cmd.SetErr(f.err)
			return f.err
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

// newReleaseFixture wires a consumer against a real Redis stream. A non-nil
// xnackErr makes XNACK alone fail with it.
func newReleaseFixture(t *testing.T, xnackErr error) *releaseFixture {
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
	if xnackErr != nil {
		client.AddHook(xnackFailer{err: xnackErr})
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
				// 60s: longer than the entry's real age, so the reclaim below
				// can only take it back because XNACK reset its delivery time.
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

// TestReleaseMessageLeavesTheEntryUnownedAndReclaimable runs the real XNACK on
// the real server: the entry loses its owner and THIS consumer's own reclaim,
// which skips entries it still owns, takes it back. On a single-miner fleet no
// other consumer exists to rescue it, so this round trip is the property.
func TestReleaseMessageLeavesTheEntryUnownedAndReclaimable(t *testing.T) {
	f := newReleaseFixture(t, nil)
	ctx := context.Background()

	require.Equal(t, releaseConsumerName, f.ownerOf(t, f.msg.ID),
		"premise: the entry starts owned by this consumer")

	require.NoError(t, f.consumer.ReleaseMessage(ctx, f.msg))
	require.Empty(t, f.ownerOf(t, f.msg.ID), "XNACK SILENT leaves the entry with no owner")

	msgs, _, err := f.consumer.claimIdleFromOtherConsumers(ctx, "0-0")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "and it is claimable again by this very consumer")
	require.Equal(t, f.msg.ID, msgs[0].ID)
}

// A failed XNACK is reported and changes nothing: there is no second path that
// moves the entry, so the caller counts it as not released and it stays pending
// under this consumer. A NOPERM is named as an ACL decision.
func TestReleaseMessageReportsAFailedXNackAndLeavesTheEntry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		quote string
	}{
		{name: "unknown command", err: errors.New("ERR unknown command 'XNACK', with args beginning with: "), quote: "failed to release message"},
		{name: "acl", err: errors.New("NOPERM User relayminer has no permissions to run the 'xnack' command"), quote: "forbidden by ACL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseFixture(t, tc.err)
			err := f.consumer.ReleaseMessage(context.Background(), f.msg)
			require.ErrorIs(t, err, tc.err)
			require.ErrorContains(t, err, tc.quote)
			require.Equal(t, releaseConsumerName, f.ownerOf(t, f.msg.ID),
				"LINK release-xnack-only: a failed XNACK must not move the entry")
		})
	}
}

// TestReleaseMessageRefusesAfterClose guards the contract the caller relies on:
// a closed consumer must not report that it handed anything over. The drain
// counts a release failure as abandoned, which leaves the entry pending -- the
// safe outcome.
func TestReleaseMessageRefusesAfterClose(t *testing.T) {
	f := newReleaseFixture(t, nil)
	f.consumer.closed = true

	require.Error(t, f.consumer.ReleaseMessage(context.Background(), f.msg))
	require.Equal(t, releaseConsumerName, f.ownerOf(t, f.msg.ID),
		"and it must not have touched the entry")
}
