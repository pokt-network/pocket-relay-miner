//go:build test

package miner

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHandleRelay_RedeliveryStillCreatesTheSession is the regression test for
// an unpaid-work path.
//
// The sequence, all of it ordinary:
//
//  1. Consumer A takes a relay, updates the SMST, and MarkProcessed adds the
//     hash to the dedup set (SADD -> 1).
//  2. A dies there — before the stream ACK, and before it tells the session
//     coordinator anything.
//  3. B reclaims the message. The SMST update is idempotent, so the tree is
//     fine. MarkProcessed now returns added=false, because A's entry is there.
//
// The dedup result gates the coordinator call, which is right for the RELAY
// COUNTER: counting the same relay twice inflates the claim. But that same
// call also CREATES the session when it does not exist yet
// (OnRelayProcessed -> OnSessionCreated -> CreateIfAbsent). Skipping it on a
// redelivery therefore skips the creation too — and if that relay was the
// session's first, nothing ever records the session. The SMST holds the
// relays, no snapshot exists, the claim is never submitted, and the work is
// unpaid.
//
// Creation is first-write-wins and idempotent, so running it on every delivery
// costs one Redis round-trip and risks nothing. The counter stays gated.
func TestHandleRelay_RedeliveryStillCreatesTheSession(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1redelivery")
	const sessionID = "sess-redelivered"

	msg := newStreamMessage(f.supplierAddr, sessionID, "relay-redelivered", 100)

	// Consumer A got this far and then died: the hash is in the dedup set and
	// the session was never created.
	added, err := f.dedup.MarkProcessed(f.ctx, msg.Message.RelayHash, sessionID)
	require.NoError(t, err)
	require.True(t, added, "the fixture must start from a fresh dedup entry")

	before, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, before, "the session must not exist yet — that is the whole premise")

	// B reclaims and handles the same relay.
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msg),
		"a redelivered relay must still ACK")

	snap, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, snap,
		"the session MUST exist after the redelivery: without a snapshot nothing "+
			"claims the SMST the relay is already in, and the work goes unpaid")
	require.Equal(t, f.supplierAddr, snap.SupplierOperatorAddress)
	require.Equal(t, "svc-1", snap.ServiceID)
	require.Equal(t, int64(10), snap.SessionEndHeight,
		"the claim window is derived from this height; a wrong one is a missed claim")
}

// TestHandleRelay_RedeliveryDoesNotDoubleCount pins the other half of the
// contract, so the fix above cannot be made by simply removing the gate: the
// relay counter feeds the claim, and counting one relay twice inflates it.
func TestHandleRelay_RedeliveryDoesNotDoubleCount(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1nodouble")
	const sessionID = "sess-no-double"

	// A fresh message object per delivery, not the same one twice: handleRelay
	// nils RelayHash and RelayBytes after the SMST update to let the GC have
	// them, so a reused object arrives the second time with no hash, takes the
	// no-deduplicator branch, and double-counts for a reason production never
	// has. A redelivery reads the bytes off the stream again.
	deliver := func() { //nolint:contextcheck // f.ctx is the fixture's context
		msg := newStreamMessage(f.supplierAddr, sessionID, "relay-once", 100)
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msg))
	}

	deliver()

	first, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, int64(1), first.RelayCount)

	// The SAME relay arrives again — the original copy landing after a reclaim.
	deliver()

	second, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, int64(1), second.RelayCount,
		"a redelivered relay must not be counted twice — the count feeds the claim")
	require.Equal(t, first.TotalComputeUnits, second.TotalComputeUnits,
		"nor may its compute units be added twice")
}
