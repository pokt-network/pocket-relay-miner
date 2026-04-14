//go:build test

package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStreamMessage_IsReclaimDefaultsFalse pins the invariant that normal
// (non-reclaimed) StreamMessages must have IsReclaim == false. Regressions on
// this default would cause the miner's deduplicator check to run on every
// relay — the exact overhead the feature was introduced to avoid.
func TestStreamMessage_IsReclaimDefaultsFalse(t *testing.T) {
	m := StreamMessage{
		ID:         "1234567890123-0",
		StreamName: "ha:relays:pokt1test",
		Message:    &MinedRelayMessage{SessionId: "sess-1"},
	}
	assert.False(t, m.IsReclaim, "IsReclaim must default to false for XREADGROUP delivery")
}

// TestStreamMessage_IsReclaimSet verifies the flag is persisted when set
// explicitly by the consumer's reclaim path.
func TestStreamMessage_IsReclaimSet(t *testing.T) {
	m := StreamMessage{
		ID:         "1234567890123-0",
		StreamName: "ha:relays:pokt1test",
		Message:    &MinedRelayMessage{SessionId: "sess-1"},
		IsReclaim:  true,
	}
	assert.True(t, m.IsReclaim)

	// Copy-through-channel semantics: when the consumer sends `*msg` on the
	// channel the receiver gets a value copy that preserves the flag.
	ch := make(chan StreamMessage, 1)
	ch <- m
	recv := <-ch
	assert.True(t, recv.IsReclaim)
}
