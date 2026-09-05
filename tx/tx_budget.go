package tx

import (
	"context"
	"time"
)

// DefaultTxRPCTimeout bounds ONE broadcast attempt's network work.
//
// It exists because the window budget is the wrong clock for an RPC.
// DefaultTxTimeoutMin is 2 minutes and the minimum clamp is unconditional, so
// the effective window timeout is never under two minutes -- while BroadcastTx
// runs in BROADCAST_MODE_SYNC and returns after CheckTx, in milliseconds. Two
// minutes on an RPC that healthily takes milliseconds is not a bound in any
// useful sense: it measures how long the transaction is worth something
// on-chain, not how long a healthy node may take to answer.
//
// The consequence is not academic: a broadcast that cannot finish keeps a
// transition-subpool worker for the whole window, and that subpool is per
// supplier with a minimum of ten.
//
// WHY 30s, AND IT IS A JUDGEMENT: an order of magnitude above the 5s the
// operator already declares for chain queries (miner.Config.GetQueryTimeout),
// leaving room for a Simulate of a batched claim -- Simulate EXECUTES the
// messages, so it is legitimately heavier than a read -- and an order of
// magnitude below the two-minute floor it replaces. Nobody measured it.
// The evidence to move it is already wired: ha_tx_broadcast_latency_seconds is
// this exact path's histogram, per supplier, so an operator reads their own p99
// and chooses.
const DefaultTxRPCTimeout = 30 * time.Second

// effectiveTxTimeout resolves the window budget for this broadcast and returns
// the window it came from, so the caller can build an absolute deadline.
func (tc *TxClient) effectiveTxTimeout(ctx context.Context) (time.Duration, string, txWindow) {
	window, _ := ctx.Value(txWindowTimeoutKey{}).(txWindow)
	timeout, source := computeEffectiveTxTimeout(
		window.raw,
		tc.config.TxTimeoutClockSkewBuffer,
		tc.config.TxTimeoutMin,
		tc.config.TxTimeoutMax,
		tc.config.TxTimeoutDefault,
	)
	return timeout, source, window
}

// withBroadcastDeadline puts a clock on the whole broadcast: the account
// lookup, the simulation and the broadcast itself.
//
// TWO CAPS, cutting from different sides, and they are not redundant:
//
//   - the WINDOW cap is absolute and SHARED across retries -- computedAt plus
//     the effective timeout. The caller builds one context and reuses it for
//     every attempt, so without an absolute instant N attempts would each start
//     a fresh full budget and together spend far more window than exists.
//   - the RPC cap is per attempt -- now plus txRPCTimeout. A node that has
//     stopped answering should cost one attempt's worth of seconds, not the
//     whole window.
//
// The earliest of the two wins. When the context carries no window (computedAt
// is the zero time) only the RPC cap applies, which is the right default for a
// caller that never declared a window.
func (tc *TxClient) withBroadcastDeadline(
	ctx context.Context,
	timeout time.Duration,
	window txWindow,
) (context.Context, context.CancelFunc) {
	rpcDeadline := time.Now().Add(tc.rpcTimeout())

	deadline := rpcDeadline
	if !window.computedAt.IsZero() {
		if windowDeadline := window.computedAt.Add(timeout); windowDeadline.Before(deadline) {
			deadline = windowDeadline
		}
	}

	return context.WithDeadline(ctx, deadline)
}

// rpcTimeout is the configured per-attempt cap, or the default when unset.
func (tc *TxClient) rpcTimeout() time.Duration {
	if tc.config.TxRPCTimeout > 0 {
		return tc.config.TxRPCTimeout
	}
	return DefaultTxRPCTimeout
}
