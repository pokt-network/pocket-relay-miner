//go:build test

package tx

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// frozenBlockTime is a BlockTimeProvider that never moves, which is the whole
// point: a cosmos-sdk unordered transaction is keyed by (timeout.UnixNano(),
// sender), the anchor is the chain's latest_block_time, and that value is
// CONSTANT for a whole block. Freezing it here reproduces one block exactly.
type frozenBlockTime struct{ t time.Time }

func (f frozenBlockTime) LatestBlockTime() time.Time { return f.t }

// broadcastTimeouts drives n real CreateClaims calls through the mock and
// returns the timeout_timestamp each one actually put on the wire.
//
// It decodes the BROADCAST BYTES rather than recomputing the arithmetic. The
// first version of this test called nextTxNonceOffset() itself and rebuilt
// `anchor.Add(duration).Add(offset)` inside the assertion, so it re-implemented
// the production line instead of exercising it: deleting the offset from
// signAndBroadcast left the test GREEN. That is the defect this shape exists to
// prevent, and it is why the bytes are read after EVERY call -- the mock keeps
// only the most recent ones.
func broadcastTimeouts(t *testing.T, n int) []time.Time {
	t.Helper()

	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	km := setupTestKeyManager(t, supplierAddr)
	defer func() { _ = km.Close() }()

	tc, err := NewTxClient(logging.NewLoggerFromConfig(logging.DefaultConfig()), km, TxClientConfig{
		GRPCEndpoint:      testServer.address,
		ChainID:           "test-chain",
		GasLimit:          100000,
		GasPrice:          parseGasPrice(t, "0.001upokt"),
		BlockTimeProvider: frozenBlockTime{t: time.Unix(1_788_487_516, 0)},
	})
	require.NoError(t, err)
	defer func() { _ = tc.Close() }()

	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		_, _, err := tc.CreateClaims(context.Background(), supplierAddr, 1000,
			[]*prooftypes.MsgCreateClaim{generateTestClaim(t, supplierAddr, "session-1")})
		require.NoError(t, err)
		out = append(out, decodeBroadcastTxTimeoutTimestamp(t, testServer.getLastTxBytes()))
	}
	return out
}

// TestBroadcastGivesEveryTxItsOwnNonce is the assertion whose absence let a live
// money defect stand. tx_timeout_test.go pins the anchor and the clamp; nothing
// asserted that two transactions built against the SAME block time reach the
// chain with DIFFERENT timeout timestamps.
//
// Both halves are required and neither is redundant. Uniqueness alone passes
// for an offset big enough to break the ante handler's ceiling; the ceiling
// alone passes for the defect itself, which was perfectly within bounds and
// perfectly duplicated.
func TestBroadcastGivesEveryTxItsOwnNonce(t *testing.T) {
	anchor := time.Unix(1_788_487_516, 0)
	// No window is injected into the context, so signAndBroadcast falls back to
	// TxTimeoutDefault. base is therefore the deadline the tx would carry with
	// NO offset at all, which is what makes the direction of the offset
	// assertable rather than merely its existence.
	base := anchor.Add(DefaultTxTimeoutMax)
	stamps := broadcastTimeouts(t, 64)

	seen := make(map[int64]struct{}, len(stamps))
	for i, ts := range stamps {
		if _, dup := seen[ts.UnixNano()]; dup {
			t.Fatalf("transaction %d reused an unordered nonce (%d): the chain rejects "+
				"this with \"sender ... has already used timeout\"", i, ts.UnixNano())
		}
		seen[ts.UnixNano()] = struct{}{}

		// The offset must land in [base, base+spread) -- NOT merely "after the
		// anchor". That weaker form is what the first version of this test
		// asserted, and it does not bite: with a 2 minute deadline and a 10ms
		// offset, SUBTRACTING the offset still leaves the timestamp comfortably
		// after the anchor, so the test stayed green while the code did the one
		// thing the comment said it must never do. Measured here by injection.
		require.Falsef(t, ts.Before(base),
			"tx %d landed %v BEFORE the un-offset deadline: the offset was subtracted, "+
				"and an offset that moves the deadline earlier is what can trip the ante "+
				"handler's already-passed check", i, base.Sub(ts))
		require.Truef(t, ts.Before(base.Add(txNonceSpread)),
			"tx %d landed %v past the un-offset deadline, outside the %v spread",
			i, ts.Sub(base), txNonceSpread)
		require.Lessf(t, ts.Sub(anchor), txTimeoutHardCeiling,
			"tx %d is %v past the block time, over the ante handler's %v ceiling",
			i, ts.Sub(anchor), txTimeoutHardCeiling)
	}
	require.Len(t, seen, len(stamps))
}

// TestTxTimeoutMaxLeavesRoomForTheNonceSpread pins the arithmetic the offset
// depends on: the spread is added AFTER the clamp, so the deadline ceiling has
// to have been reduced by it. Raising either number without the other silently
// eats the drift budget that keeps CheckTx from rejecting us outright.
func TestTxTimeoutMaxLeavesRoomForTheNonceSpread(t *testing.T) {
	require.Equal(t, txTimeoutHardCeiling-txTimeoutSafetyMargin-txNonceSpread, DefaultTxTimeoutMax,
		"DefaultTxTimeoutMax must be derived from the ceiling, not written out by hand")
	require.Less(t, DefaultTxTimeoutMax+txNonceSpread, txTimeoutHardCeiling)
	require.GreaterOrEqual(t, txTimeoutHardCeiling-(DefaultTxTimeoutMax+txNonceSpread), txTimeoutSafetyMargin,
		"the whole safety margin must survive the spread: it is the only budget against "+
			"drift between our anchor and the validator's block time")
}

// TestNonceOffsetAppliesUnderWallClockAnchoring: the offset must not be
// conditional on which anchor won. The signer does not get to know who called
// it, and the wall-clock branch is the one that runs at startup, before the
// block subscriber has seen its first event.
func TestNonceOffsetAppliesUnderWallClockAnchoring(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	km := setupTestKeyManager(t, supplierAddr)
	defer func() { _ = km.Close() }()

	// No BlockTimeProvider: signAndBroadcast falls back to wall clock.
	tc, err := NewTxClient(logging.NewLoggerFromConfig(logging.DefaultConfig()), km, TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.001upokt"),
	})
	require.NoError(t, err)
	defer func() { _ = tc.Close() }()

	before := txNonceCounter.Load()
	_, _, err = tc.CreateClaims(context.Background(), supplierAddr, 1000,
		[]*prooftypes.MsgCreateClaim{generateTestClaim(t, supplierAddr, "session-1")})
	require.NoError(t, err)

	require.Greater(t, txNonceCounter.Load(), before,
		"the offset must be drawn on the wall-clock branch too: making it conditional "+
			"on the anchor is how one path silently keeps the collision")
}

// TestNonceOffsetIsExactWithinAProcessAndSeparatesProcesses states the property
// the first version's comment got wrong. Within a process the counter is
// monotonic and the modulo is taken of consecutive values, so uniqueness is
// EXACT, not probabilistic. Only the inter-process part is a matter of chance,
// and each process walks a CONTIGUOUS run rather than sampling independently.
func TestNonceOffsetIsExactWithinAProcessAndSeparatesProcesses(t *testing.T) {
	const n = 200_000
	seen := make(map[time.Duration]struct{}, n)
	for i := 0; i < n; i++ {
		off := nextTxNonceOffset()
		require.GreaterOrEqual(t, off, time.Duration(0))
		require.Less(t, off, txNonceSpread, "the offset must stay inside the spread")
		seen[off] = struct{}{}
	}
	require.Len(t, seen, n,
		"uniqueness within a process is exact for a run shorter than the spread, not probabilistic")

	// Two processes: different bases must not walk the same sequence. A seed
	// that degrades to a constant -- which the crypto/rand fallback did -- makes
	// two replicas start identically and collide with certainty.
	require.NotEqual(t, offsetsFrom(7, 5), offsetsFrom(999_331, 5),
		"two processes seeded differently must not produce the same offsets")
}

// offsetsFrom reproduces nextTxNonceOffset's arithmetic for an explicit base,
// which is the one thing a test cannot get by calling production: the base is
// drawn once per process.
func offsetsFrom(base uint64, n int) []time.Duration {
	out := make([]time.Duration, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, time.Duration((base+uint64(i))%uint64(txNonceSpread)))
	}
	return out
}
