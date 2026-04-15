//go:build test

package redis

import (
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// buildPooledTestMessage serializes a MinedRelayMessage into a Redis
// XMessage the way the publisher does, so parseMessage can exercise the
// real unmarshal + pool path.
func buildPooledTestMessage(t *testing.T, sessionID, serviceID string, cu uint64) redis.XMessage {
	t.Helper()
	relay := &transport.MinedRelayMessage{
		RelayHash:               []byte{0x01, 0x02, 0x03, 0x04},
		RelayBytes:              []byte("relay-payload-bytes"),
		ComputeUnitsPerRelay:    cu,
		SessionId:               sessionID,
		SessionEndHeight:        200,
		SupplierOperatorAddress: "pokt1supplier",
		ServiceId:               serviceID,
		ApplicationAddress:      "pokt1app",
		ArrivalBlockHeight:      150,
		PublishedAtUnixNano:     1776206000,
		SessionStartHeight:      100,
	}
	buf, err := relay.Marshal()
	require.NoError(t, err)
	return redis.XMessage{
		ID:     "1-0",
		Values: map[string]interface{}{"data": string(buf)},
	}
}

// newTestConsumer returns a StreamsConsumer stub that has just enough
// state to call parseMessage directly. The unit under test is the
// parseMessage method itself, not the Consume loop.
func newTestConsumer() *StreamsConsumer {
	return &StreamsConsumer{streamName: "ha:relays:pokt1test"}
}

// TestParseMessage_ReturnsPooledMessage verifies parseMessage pulls a
// MinedRelayMessage from the transport pool and returns a correctly
// populated StreamMessage. Releasing it back to the pool must scrub
// every field so a subsequent Acquire sees no stale data.
func TestParseMessage_ReturnsPooledMessage(t *testing.T) {
	c := newTestConsumer()
	xmsg := buildPooledTestMessage(t, "sess-1", "develop-http", 1000)

	msg, err := c.parseMessage(xmsg, "ha:relays:pokt1test")
	require.NoError(t, err)
	require.NotNil(t, msg.Message)

	assert.Equal(t, "sess-1", msg.Message.SessionId)
	assert.Equal(t, "develop-http", msg.Message.ServiceId)
	assert.Equal(t, uint64(1000), msg.Message.ComputeUnitsPerRelay)
	assert.Equal(t, []byte("relay-payload-bytes"), msg.Message.RelayBytes)

	// Release and confirm scrub.
	released := msg.Message
	transport.ReleaseMinedRelayMessage(released)
	assert.Empty(t, released.SessionId)
	assert.Empty(t, released.ServiceId)
	assert.Zero(t, released.ComputeUnitsPerRelay)
	assert.Empty(t, released.RelayBytes)
}

// TestParseMessage_NoCrossRelayLeak writes two different relays through
// parseMessage+Release sequentially and proves the second never sees
// fields from the first. This is the end-to-end correctness contract
// of the pool on the real hot path.
func TestParseMessage_NoCrossRelayLeak(t *testing.T) {
	c := newTestConsumer()

	first := buildPooledTestMessage(t, "sess-first", "svc-A", 500)
	msg1, err := c.parseMessage(first, "ha:relays:pokt1test")
	require.NoError(t, err)
	assert.Equal(t, "sess-first", msg1.Message.SessionId)
	assert.Equal(t, "svc-A", msg1.Message.ServiceId)
	transport.ReleaseMinedRelayMessage(msg1.Message)

	second := buildPooledTestMessage(t, "sess-second", "svc-B", 2000)
	msg2, err := c.parseMessage(second, "ha:relays:pokt1test")
	require.NoError(t, err)
	require.NotNil(t, msg2.Message)
	assert.Equal(t, "sess-second", msg2.Message.SessionId)
	assert.Equal(t, "svc-B", msg2.Message.ServiceId)
	assert.Equal(t, uint64(2000), msg2.Message.ComputeUnitsPerRelay)
	transport.ReleaseMinedRelayMessage(msg2.Message)
}

// TestParseMessage_InvalidPayloadReleasesPooledMessage guards the error
// path: when Unmarshal fails, parseMessage must still return the pooled
// message to avoid draining the pool over time.
func TestParseMessage_InvalidPayloadReleasesPooledMessage(t *testing.T) {
	c := newTestConsumer()
	bad := redis.XMessage{
		ID:     "bad-0",
		Values: map[string]interface{}{"data": string([]byte{0xff, 0xff, 0xff, 0xff})},
	}

	msg, err := c.parseMessage(bad, "ha:relays:pokt1test")
	require.Error(t, err)
	assert.Nil(t, msg.Message)

	// Pool must still produce a clean message afterwards — if the error
	// path leaked a dirty object, this would fail.
	clean := transport.AcquireMinedRelayMessage()
	assert.Empty(t, clean.SessionId)
	assert.Empty(t, clean.RelayBytes)
	transport.ReleaseMinedRelayMessage(clean)
}
