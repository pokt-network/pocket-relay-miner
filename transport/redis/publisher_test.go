//go:build test

package redis

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// newTestPublisher builds a publisher writing under the test's own key prefix.
func newTestPublisher(t *testing.T, prefix string) (*StreamsPublisher, string) {
	t.Helper()
	client := testredis.Client(t)
	streamPrefix := prefix + ":relays"
	return NewStreamsPublisher(zerolog.Nop(), client, streamPrefix), streamPrefix
}

func testMessage(supplier string) *transport.MinedRelayMessage {
	return &transport.MinedRelayMessage{
		SupplierOperatorAddress: supplier,
		ServiceId:               "svc-a",
		SessionId:               "sess-1",
		SessionEndHeight:        100,
	}
}

// TestPublishSetsNoStreamTTL is the regression test for the defect that made a
// supplier's relay stream disappear mid-session.
//
// The publisher used to issue EXPIRE once per (process, stream) and memoise the
// fact. That produced an absolute deadline anchored to the process's first
// publish, unrelated to any session boundary: when it fired, Redis deleted the
// whole key -- entries not yet consumed, the consumer group and its pending
// entries list along with it -- and emitted nothing, because key expiry is
// silent. A supplier's stream is a permanent lane spanning every session that
// supplier serves, so no clock is the right instrument for ending it.
//
// The assertion is on the observable a human would check with redis-cli: TTL of
// the stream key. -1 means "exists, no expiry set", which is what we want; any
// non-negative value means an expiry was armed and the defect is back.
func TestPublishSetsNoStreamTTL(t *testing.T) {
	ctx := context.Background()
	prefix := testredis.Prefix(t)
	client := testredis.Client(t)
	pub, streamPrefix := newTestPublisher(t, prefix)

	const supplier = "pokt1supplier_ttl"
	streamKey := transport.SupplierStreamName(streamPrefix, supplier)

	require.NoError(t, pub.Publish(ctx, testMessage(supplier)))

	ttl, err := client.TTL(ctx, streamKey).Result()
	require.NoError(t, err)
	require.Equal(t, time.Duration(-1), ttl,
		"the relay stream must carry NO expiry: -1 is Redis' answer for a key with no TTL. "+
			"A non-negative TTL here means an EXPIRE was armed again, and when it fires it deletes "+
			"un-consumed relays and the pending-entries list with the key, silently")

	// Guard against the assertion passing for the wrong reason: -1 is also what
	// TTL returns for a key that does not exist... no, that is -2. Prove the key
	// really is there and really holds the relay.
	length, err := client.XLen(ctx, streamKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), length, "the stream must actually contain the published relay")
}

// TestPublishDoesNotArmTTLAcrossManyPublishes pins the property over repeated
// publishes, not just the first.
//
// The old code only ever called EXPIRE on the FIRST publish of a stream, so a
// test that published once and checked would have caught the first-publish case
// only. Re-checking after several publishes closes the door on a "refresh the
// TTL on every write" fix being introduced later without a decision: refreshing
// would still leave an idle stream to be deleted with its pending entries.
func TestPublishDoesNotArmTTLAcrossManyPublishes(t *testing.T) {
	ctx := context.Background()
	prefix := testredis.Prefix(t)
	client := testredis.Client(t)
	pub, streamPrefix := newTestPublisher(t, prefix)

	const supplier = "pokt1supplier_ttl_many"
	streamKey := transport.SupplierStreamName(streamPrefix, supplier)

	for i := 0; i < 5; i++ {
		require.NoError(t, pub.Publish(ctx, testMessage(supplier)))
		ttl, err := client.TTL(ctx, streamKey).Result()
		require.NoError(t, err)
		require.Equal(t, time.Duration(-1), ttl, "no expiry may be armed on publish %d", i+1)
	}

	length, err := client.XLen(ctx, streamKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(5), length)
}

// TestPublishRejectsMessagesMissingSessionFields covers the validation the
// publisher performs before writing, which nothing exercised before.
func TestPublishRejectsMessagesMissingSessionFields(t *testing.T) {
	ctx := context.Background()
	prefix := testredis.Prefix(t)
	pub, _ := newTestPublisher(t, prefix)

	tests := []struct {
		name   string
		mutate func(*transport.MinedRelayMessage)
	}{
		{"empty session id", func(m *transport.MinedRelayMessage) { m.SessionId = "" }},
		{"zero session end height", func(m *transport.MinedRelayMessage) { m.SessionEndHeight = 0 }},
		{"negative session end height", func(m *transport.MinedRelayMessage) { m.SessionEndHeight = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := testMessage("pokt1supplier_invalid")
			tt.mutate(msg)
			require.Error(t, pub.Publish(ctx, msg))
		})
	}

	t.Run("nil message", func(t *testing.T) {
		require.Error(t, pub.Publish(ctx, nil))
	})

	t.Run("closed publisher", func(t *testing.T) {
		require.NoError(t, pub.Close())
		require.Error(t, pub.Publish(ctx, testMessage("pokt1supplier_closed")))
	})
}
