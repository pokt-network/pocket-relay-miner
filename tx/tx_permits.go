package tx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/semaphore"
)

// DefaultTxMaxConcurrent caps how many broadcasts may be in flight on the
// transaction connection at once.
//
// ITS AXIS CHANGED, and the old reason is void. The number was chosen when
// transactions and queries shared one connection: roughly one HTTP/2 stream per
// transaction, a client ceiling of 100, leave about two thirds for everything
// else. After the tx client got its own connection there IS no everything else
// on it, so that argument no longer holds up the number.
//
// What it bounds now is the FULL NODE, not the stream ceiling: with gas
// estimation on, each transaction costs a Simulate -- which executes the
// messages, unlike CheckTx -- plus a BroadcastTx. Nobody has measured how much
// concurrent Simulate a full node absorbs before it degrades, so 32 is a
// conservative cap on a different axis, and raising it to ~90 would be exactly
// as unmeasured as leaving it.
//
// AND THAT AXIS IS NOT UNIVERSAL EITHER: Simulate only runs when GasLimit is 0
// (tx_client.go). With explicit gas there is no Simulate and "two RPCs per
// transaction" disappears, so the cap has two regimes and this reasoning covers
// one of them.
//
// The metric that would justify moving it is NOT broadcast latency, it is
// ha_tx_permit_wait_seconds below: if nobody ever waits, the cap costs nothing
// and raising it buys nothing. A wait in the p99 is the evidence -- and it is
// also the point at which to ask whether the answer is a bigger cap or a
// healthier node.
const DefaultTxMaxConcurrent = 32

// ErrTxConcurrencySaturated means the caller never reached the network: every
// permit was taken and the wait ran out.
//
// It has a name because a caller has to be able to tell "we asked the chain and
// it said no" from "we never asked". The inclusion reconciler's resend counter
// is the reason: it caps total resend attempts at MaxRebroadcasts, so counting
// an attempt that never touched the network burns a resend that was never
// spent. A bare DeadlineExceeded cannot carry that distinction.
var ErrTxConcurrencySaturated = errors.New("tx connection is saturated: no permit available")

// errTxClientClosed is returned once Close() has run.
var errTxClientClosed = errors.New("tx client is closed")

// The waited label separates the two ways a broadcast can be refused for
// concurrency: a caller that queued and ran out of time, and one that refused
// to queue at all. They mean different things to an operator -- the first is
// congestion, the second is the safety net finding the door shut.
const (
	permitWaited     = "true"
	permitDidNotWait = "false"
)

// noPermitWaitKey marks a call that must never queue for a permit.
type noPermitWaitKey struct{}

// WithoutPermitWait marks ctx as belonging to a caller that must not queue for
// a broadcast permit: if none is free it fails immediately instead of waiting.
//
// It exists for the inclusion reconciler's resend, and the principle
// generalises: NEVER let the safety net queue behind the traffic it came to
// rescue. A resend runs on a 10-second per-group budget; spending it waiting
// for a permit held by a transaction that is already dying means the resend
// leaves without ever reaching the chain -- and saturation is precisely when
// the resend exists. Failing fast costs one block's delay, because the payloads
// stay in the store and the reconciler runs every block.
func WithoutPermitWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, noPermitWaitKey{}, true)
}

// acquirePermit takes one broadcast permit, or reports why it could not.
//
// It runs WITHOUT holding tc.mu, and that is load-bearing rather than tidy. If
// the wait happened under a read lock, Close() could not take the write lock to
// mark the client closed, so every queued caller would go on to broadcast into
// a connection Close() was about to shut -- and Close() itself would take as
// long as the whole backlog. The closed flag is an atomic for exactly this
// reason, and the guarantee that no new work slips in during a Close() lives in
// the semaphore's FIFO queue instead: a fresh Acquire cannot jump ahead of a
// waiter, and once Close()'s Acquire(N) reaches the front the library blocks
// everyone behind it rather than serving smaller requests around it.
func (tc *TxClient) acquirePermit(ctx context.Context) error {
	if tc.closed.Load() {
		return errTxClientClosed
	}

	if noWait, _ := ctx.Value(noPermitWaitKey{}).(bool); noWait {
		if !tc.permits.TryAcquire(1) {
			// No wait sample: a caller that refuses to queue waited zero, and
			// a zero here would sit in the same bucket as a healthy acquire,
			// hiding the very condition this path detects.
			txPermitSaturatedTotal.WithLabelValues(permitDidNotWait).Inc()
			return ErrTxConcurrencySaturated
		}
	} else {
		start := time.Now()
		tc.permitWaiters.Add(1)
		err := tc.permits.Acquire(ctx, 1)
		tc.permitWaiters.Add(-1)
		txPermitWait.Observe(time.Since(start).Seconds())

		if err != nil {
			txPermitSaturatedTotal.WithLabelValues(permitWaited).Inc()
			return fmt.Errorf("%w: %w", ErrTxConcurrencySaturated, err)
		}
	}

	// Re-read after acquiring: a Close() may have landed while this caller sat
	// in the queue. Handing the permit back is what makes a queued caller
	// return "closed" instead of broadcasting into a connection that is going
	// away.
	if tc.closed.Load() {
		tc.permits.Release(1)
		return errTxClientClosed
	}
	return nil
}

// releasePermit returns one permit.
func (tc *TxClient) releasePermit() {
	tc.permits.Release(1)
}

// maxConcurrent is the configured cap, or the default when unset.
func (tc *TxClient) maxConcurrent() int64 {
	if tc.config.MaxConcurrent > 0 {
		return int64(tc.config.MaxConcurrent)
	}
	return DefaultTxMaxConcurrent
}

// newPermits builds the semaphore for n concurrent broadcasts.
func newPermits(n int64) *semaphore.Weighted { return semaphore.NewWeighted(n) }
