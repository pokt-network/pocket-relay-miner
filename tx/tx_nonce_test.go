//go:build test

package tx

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestTxNonceOffsetKeepsTheUnorderedNonceUnique is the test whose absence let a
// live money defect stand: tx/tx_timeout_test.go pins the anchor and the clamp,
// and nothing asserted that two transactions built in the SAME block get
// DIFFERENT timeout timestamps.
//
// A cosmos-sdk unordered transaction is keyed by (timeout.UnixNano(), sender).
// The anchor is the chain's latest_block_time, constant across a block, so
// without an offset every transaction one process builds for one supplier
// inside one block carries the same nonce and all but the first are rejected in
// CheckTx. Measured live 2026-09-03: three sessions in claim_tx_error, one
// EXPIRED claim, one slashing event.
//
// BOTH assertions are required and neither is redundant. Uniqueness alone
// passes for an offset large enough to break the ante handler's 10 minute
// ceiling; the ceiling alone passes for the defect itself, which was perfectly
// within bounds and perfectly duplicated.
func TestTxNonceOffsetKeepsTheUnorderedNonceUnique(t *testing.T) {
	// One frozen block time: the whole point is that the anchor does not move.
	const blockTime = 1_788_487_516_000_000_000
	anchor := time.Unix(0, blockTime)

	// The clamped duration a real call would use, at its maximum.
	timeoutDuration := DefaultTxTimeoutMax

	const n = 100_000
	seen := make(map[int64]struct{}, n)

	// The hard ceiling the ante handler enforces against ctx.BlockTime.
	const hardCeiling = 10 * time.Minute

	for i := 0; i < n; i++ {
		ts := anchor.Add(timeoutDuration).Add(nextTxNonceOffset())

		nano := ts.UnixNano()
		if _, dup := seen[nano]; dup {
			t.Fatalf("duplicate unordered nonce after %d transactions: %d -- "+
				"the chain rejects this with \"already used timeout\"", i, nano)
		}
		seen[nano] = struct{}{}

		require.Truef(t, ts.After(anchor),
			"a timeout at or before the block time is rejected as already passed")
		require.Lessf(t, ts.Sub(anchor), hardCeiling,
			"timeout is %v past the block time, over the ante handler's %v ceiling "+
				"(\"unordered tx ttl exceeds 10m0s\")", ts.Sub(anchor), hardCeiling)
	}

	require.Len(t, seen, n, "every transaction must carry its own nonce")
}

// TestTxNonceOffsetStaysUnderTheCeilingWithTheConfiguredMax pins the arithmetic
// the offset depends on, so that raising DefaultTxTimeoutMax or txNonceSpread
// cannot silently eat the headroom.
func TestTxNonceOffsetStaysUnderTheCeilingWithTheConfiguredMax(t *testing.T) {
	const hardCeiling = 10 * time.Minute
	worst := DefaultTxTimeoutMax + txNonceSpread
	require.Lessf(t, worst, hardCeiling,
		"DefaultTxTimeoutMax (%v) plus the whole nonce spread (%v) is %v, which the "+
			"ante handler rejects at %v", DefaultTxTimeoutMax, txNonceSpread, worst, hardCeiling)
}
