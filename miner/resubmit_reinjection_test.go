//go:build test

package miner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// The decision that stands between re-injecting and signing again.
//
// Getting it wrong is not symmetric. Signing when we could have re-injected
// costs a signature AND puts a second live transaction for one claim into the
// gossip -- behind a load balancer each attempt can reach a node that saw none
// of the others, so the network's own de-duplication, which is free, is exactly
// what re-signing throws away. Re-injecting bytes that have EXPIRED costs the
// attempt outright: the ante handler refuses them, and the resend that could
// have landed did not happen. So every row below is a different way of not
// knowing, and the default in each is to sign.
func TestReusable(t *testing.T) {
	deadline := time.Unix(1_700_000_600, 0)
	alive := tx.SignedTxPayload{Bytes: []byte("signed"), Hash: "H", TimeoutAt: deadline, TimeoutHeight: 4321}

	for _, tt := range []struct {
		name     string
		cached   tx.SignedTxPayload
		chainNow time.Time
		want     bool
	}{
		{
			name:   "alive: the chain clock is still before the sealed deadline",
			cached: alive, chainNow: deadline.Add(-time.Minute), want: true,
		},
		{
			// The ordinary end of a window rather than an edge case: the derived
			// budget sits just under the SDK ceiling while the window is barely
			// longer, so bytes expire BEFORE their window closes.
			name:   "expired: the deadline is sealed in the bytes and cannot be moved",
			cached: alive, chainNow: deadline.Add(time.Second), want: false,
		},
		{
			name:   "exactly at the deadline is NOT alive",
			cached: alive, chainNow: deadline, want: false,
		},
		{
			// Nobody can say whether these bytes are still alive, and guessing
			// spends the attempt. The opposite mistake costs one signature.
			name:   "unknown chain clock: sign rather than guess",
			cached: alive, chainNow: time.Time{}, want: false,
		},
		{
			name:   "no bytes: an entry written before this existed, or by an older binary",
			cached: tx.SignedTxPayload{Hash: "H", TimeoutAt: deadline}, chainNow: deadline.Add(-time.Minute), want: false,
		},
		{
			// Bytes with no deadline cannot be checked, and an unchecked
			// re-injection is the one failure this must never produce.
			name:   "bytes without a deadline are not usable",
			cached: tx.SignedTxPayload{Bytes: []byte("signed")}, chainNow: deadline.Add(-time.Minute), want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, reusable(tt.cached, tt.chainNow))
		})
	}
}

// Bytes and NO hash is a legal, meaningful state -- not a contradiction.
//
// It is what a submission whose broadcast never answered leaves behind: the
// transaction was built and signed, so the bytes exist; nothing confirmed it,
// so there is no hash. That pair is the case the whole mechanism exists for,
// and a resend must re-inject rather than sign, because signing would produce a
// second live transaction for a send that may well have arrived.
func TestReusable_BytesWithoutAHashAreStillUsable(t *testing.T) {
	deadline := time.Unix(1_700_000_600, 0)
	neverConfirmed := tx.SignedTxPayload{Bytes: []byte("signed"), TimeoutAt: deadline, TimeoutHeight: 4321}

	require.True(t, reusable(neverConfirmed, deadline.Add(-time.Minute)),
		"a transaction we never got an answer for is exactly the one worth "+
			"re-injecting: the hash is missing because nothing confirmed it, not "+
			"because the bytes are unusable")
}
