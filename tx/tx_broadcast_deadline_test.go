//go:build test

package tx

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Broadcasting with no deadline on the context is refused, loudly.
//
// The budget belongs to the caller: the original submission derives it from the
// window, and a re-injection inherits the reconciler's per-group timeout. That
// used to be a sentence in a comment, and a contract in prose is one a caller
// can forget without anything noticing -- the failure is not an error but a
// WAIT, so a resend that forgot it would sit in a queue until the window closed
// and then report nothing unusual. Late and silent is the shape this whole
// mechanism exists to avoid.
//
// The check cannot fire on today's paths, and that is the point of testing it
// rather than trusting it: signAndBroadcast always sets a deadline, so nothing
// in production reaches this branch, and a change that deleted the guard would
// go unnoticed until the first caller that needed it. This test is what stands
// between "the machine checks" and "somebody remembered".
func TestBroadcastRaw_RefusesAContextWithNoDeadline(t *testing.T) {
	const supplierAddr = "pokt1supplier-deadline"
	_, tc := newTimeoutHeightTestClient(t, supplierAddr)

	_, err := tc.broadcastRaw(context.Background(), supplierAddr, "claim", signedTx{
		bytes: []byte("irrelevant: the guard runs before anything is sent"),
		hash:  "IRRELEVANT",
	})

	require.Error(t, err, "a broadcast with no budget must fail fast, not wait")
	require.Contains(t, err.Error(), "no deadline",
		"and it must say WHY, naming the missing budget rather than surfacing as "+
			"some later transport error the reader has to trace back")
}

// The control: with a deadline the guard is inert and the call proceeds to the
// transport.
//
// Without it, a guard that refused EVERY context would satisfy the case above
// and break every submission -- and since the test harness would then fail for
// its own reasons, the cause would be read as anything but this line.
func TestBroadcastRaw_ProceedsWhenTheCallerBroughtABudget(t *testing.T) {
	const supplierAddr = "pokt1supplier-deadline-ok"
	_, tc := newTimeoutHeightTestClient(t, supplierAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tc.broadcastRaw(ctx, supplierAddr, "claim", signedTx{
		bytes: []byte("not a valid transaction"),
		hash:  "IRRELEVANT",
	})

	// Whatever the mock answers, it must NOT be the guard: getting past it is
	// the whole assertion.
	if err != nil {
		require.NotContains(t, err.Error(), "no deadline",
			"a context that carries a budget must pass the guard")
	}
}
