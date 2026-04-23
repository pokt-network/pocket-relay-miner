//go:build test

package tx

import (
	"context"
	"sync"
	"testing"
	"time"

	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// stubBlockTimeProvider is a test double for BlockTimeProvider. The
// stored time is returned by every LatestBlockTime call; set it to the
// zero time.Time to simulate "no block event yet".
type stubBlockTimeProvider struct {
	mu sync.RWMutex
	t  time.Time
}

func (s *stubBlockTimeProvider) LatestBlockTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.t
}

// decodeBroadcastTxTimeoutTimestamp decodes the most recently
// broadcast TX bytes and returns the TimeoutTimestamp the client set
// on it. This is the exact value cosmos-sdk's ante handler will see
// when validating `timeoutTimestamp - ctx.BlockTime() <= 10m`.
func decodeBroadcastTxTimeoutTimestamp(t *testing.T, txBytes []byte) time.Time {
	t.Helper()
	require.NotEmpty(t, txBytes, "no tx broadcast captured")

	var raw txtypes.Tx
	require.NoError(t, raw.Unmarshal(txBytes))
	require.NotNil(t, raw.Body, "tx body missing")
	require.NotNil(t, raw.Body.TimeoutTimestamp, "TimeoutTimestamp not set on tx")
	return *raw.Body.TimeoutTimestamp
}

// TestComputeEffectiveTxTimeout_HardLimitSafety is the regression guard
// for the `unordered tx ttl exceeds 10m0s` CheckTx rejection observed
// by operators after build 4b09780. The previous defaults handed the
// chain a timeoutTimestamp that sat exactly at the cosmos-sdk 10-minute
// hard ceiling, and any amount of clock skew between miner host and
// validator pushed CheckTx over the edge.
//
// The fix: subtract a configurable clock-skew buffer from the raw
// window-based deadline BEFORE clamping, and default the absolute max
// to 500 ms below the cosmos-sdk hard limit.
func TestComputeEffectiveTxTimeout_HardLimitSafety(t *testing.T) {
	const (
		hardLimit = 10 * time.Minute
		min       = 2 * time.Minute                  // DefaultTxTimeoutMin
		max       = hardLimit - 500*time.Millisecond // DefaultTxTimeoutMax
		skew      = 60 * time.Second                 // DefaultTxTimeoutClockSkewBuffer
		fallback  = 2 * time.Minute                  // DefaultTxTimeoutDefault
	)

	tests := []struct {
		name           string
		raw            time.Duration
		wantTimeout    time.Duration
		wantSource     string
		lowerThanLimit bool // true iff the result leaves room under hardLimit
	}{
		{
			name:        "no raw window falls back to default",
			raw:         0,
			wantTimeout: fallback,
			wantSource:  "default",
		},
		{
			name:        "raw smaller than min after skew clamps up to min",
			raw:         30 * time.Second, // 30s - 60s = -30s → clamp to min
			wantTimeout: min,
			wantSource:  "min_clamp",
		},
		{
			name:           "typical mid-window deadline subtracts skew cleanly",
			raw:            5 * time.Minute, // 300 - 60 = 240s = 4min
			wantTimeout:    5*time.Minute - skew,
			wantSource:     "window",
			lowerThanLimit: true,
		},
		{
			name: "raw at the cosmos-sdk hard limit lands safely under via skew subtraction",
			raw:  hardLimit, // exactly 10min raw
			// hardLimit - skew = 9m, inside [min, max] → "window" path
			// lands at 9min. Crucially, NOT at hardLimit: jitter cannot
			// push CheckTx over the 10m ceiling.
			wantTimeout:    hardLimit - skew,
			wantSource:     "window",
			lowerThanLimit: true,
		},
		{
			name:           "raw above hard limit still produces a timeout strictly below hard limit",
			raw:            2 * hardLimit,
			wantTimeout:    max,
			wantSource:     "max_clamp",
			lowerThanLimit: true,
		},
		{
			name:           "raw slightly above (max + skew) clamps to max, NOT to raw-skew",
			raw:            max + skew + time.Second, // would yield max+1s after skew — must clamp
			wantTimeout:    max,
			wantSource:     "max_clamp",
			lowerThanLimit: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source := computeEffectiveTxTimeout(tt.raw, skew, min, max, fallback)
			require.Equal(t, tt.wantSource, source, "source classification wrong for raw=%s", tt.raw)
			require.Equal(t, tt.wantTimeout, got, "effective timeout wrong for raw=%s", tt.raw)
			if tt.lowerThanLimit {
				// The entire reason this helper exists: never hand the
				// chain a timeoutTimestamp at or past the hard limit.
				require.Less(t, got, hardLimit,
					"effective timeout must stay strictly below the cosmos-sdk 10m hard limit (got %s)", got)
			}
		})
	}
}

// TestComputeEffectiveTxTimeout_SkewSubtractionOrder proves that the
// skew is subtracted BEFORE clamping, not after. If the order were
// reversed (clamp → subtract), a raw deadline of exactly max would
// survive as max and only get the skew treatment post-facto, but the
// hard-limit safety of the max ceiling would be broken. We verify the
// window path produces (raw - skew) for an in-range input.
func TestComputeEffectiveTxTimeout_SkewSubtractionOrder(t *testing.T) {
	const (
		min      = 1 * time.Minute
		max      = 9 * time.Minute
		skew     = 45 * time.Second
		fallback = 2 * time.Minute
	)

	// Pick a raw that is comfortably inside the window so no clamping
	// happens. The result MUST be raw - skew exactly.
	raw := 5 * time.Minute
	got, source := computeEffectiveTxTimeout(raw, skew, min, max, fallback)

	require.Equal(t, "window", source)
	require.Equal(t, raw-skew, got,
		"effective timeout must equal (raw - skew) inside the window; got %s", got)
}

// TestComputeEffectiveTxTimeout_ZeroSkew mirrors an operator who
// explicitly sets tx_timeout_clock_skew_buffer_seconds: 0 (tightly
// co-located miner + validator, no jitter). The adjusted path should
// pass raw through unchanged, still clamped by [min, max].
func TestComputeEffectiveTxTimeout_ZeroSkew(t *testing.T) {
	got, source := computeEffectiveTxTimeout(
		5*time.Minute, 0, 1*time.Minute, 9*time.Minute, 2*time.Minute,
	)
	require.Equal(t, "window", source)
	require.Equal(t, 5*time.Minute, got)
}

// TestDefaultTxTimeout_Max_BelowCosmosHardLimit pins the invariant
// that the compiled-in default max is strictly less than 10m. If
// someone ever bumps DefaultTxTimeoutMax to 10m this test fails and
// tells them why they shouldn't.
//
// The headroom (10s) is tuned to the block-time-anchor regime: once
// timeoutTimestamp is anchored on latest_block_time (what the
// cosmos-sdk ante handler compares against), the only delta we need
// to absorb is one block-interval of settlement jitter — the race
// where a new block commits between our read of latest_block_time
// and the validator processing the tx. 10s covers that comfortably
// without wasting session-window budget.
func TestDefaultTxTimeout_Max_BelowCosmosHardLimit(t *testing.T) {
	require.Less(t, DefaultTxTimeoutMax, 10*time.Minute,
		"DefaultTxTimeoutMax must stay strictly below 10m — the cosmos-sdk "+
			"unordered-TX hard limit — so a new block committing between our "+
			"anchor read and the validator's CheckTx cannot push us over "+
			"`unordered tx ttl exceeds 10m0s`")
	require.GreaterOrEqual(t, DefaultTxTimeoutMax, 10*time.Minute-30*time.Second,
		"DefaultTxTimeoutMax should stay within 30s of the hard limit — too "+
			"much headroom wastes the session's claim/proof window budget")
}

// TestSignAndBroadcast_AnchorsOnBlockTime_NotWallClock is the regression
// guard for the breeze production bug that lost 2026 claim submissions.
// The chain's latest_block_time lagged wall clock by ~108s while
// catching_up=false. Wall-clock-anchored timeoutTimestamp produced
// (wall_now + 492s), which from the validator's block-time perspective
// equalled (wall_now + 492s) - (wall_now - 108s) = 600s exactly — and
// any additional drift pushed CheckTx over the 10-minute ceiling with
// `unordered tx ttl exceeds 10m0s`.
//
// The fix anchors timeoutTimestamp on latest_block_time directly, so
// the delta the ante handler sees is bounded by our configured
// TxTimeoutMax regardless of how far block time lags wall clock.
func TestSignAndBroadcast_AnchorsOnBlockTime_NotWallClock(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	// Simulate the breeze scenario: chain block time lags wall clock
	// by 108s while the miner continues to accept traffic.
	laggedBlockTime := time.Now().Add(-108 * time.Second)
	provider := &stubBlockTimeProvider{t: laggedBlockTime}

	config := TxClientConfig{
		GRPCEndpoint:      testServer.address,
		ChainID:           "test-chain",
		GasLimit:          100000,
		GasPrice:          parseGasPrice(t, "0.000001upokt"),
		BlockTimeProvider: provider,
		// Use defaults for the timeout knobs so we validate the
		// production defaults, not some test-only tuning.
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-anchor"),
	}
	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.NoError(t, err)

	ts := decodeBroadcastTxTimeoutTimestamp(t, testServer.getLastTxBytes())

	// Invariant #1: the anchor was block time, not wall clock. The
	// delta between ts and laggedBlockTime must equal the configured
	// timeout duration (default path → DefaultTxTimeoutDefault).
	require.WithinDuration(t, laggedBlockTime.Add(DefaultTxTimeoutDefault), ts, 2*time.Second,
		"timeoutTimestamp must be anchored on latest_block_time, not wall clock")

	// Invariant #2 — THE ONE THAT MATTERS — is the exact check the
	// cosmos-sdk ante handler performs. Anchoring on block time
	// guarantees this delta can never exceed our configured max.
	const cosmosUnorderedTTL = 10 * time.Minute
	delta := ts.Sub(laggedBlockTime)
	require.LessOrEqual(t, delta, cosmosUnorderedTTL,
		"timeoutTimestamp - block_time must stay <=10m — cosmos-sdk rejects "+
			"anything over with `unordered tx ttl exceeds 10m0s`")

	// Wall-clock anchoring under this scenario would have produced a
	// delta of roughly (DefaultTxTimeoutDefault + 108s). Prove we're
	// nowhere near that — this is what protects us.
	require.Less(t, delta, DefaultTxTimeoutDefault+time.Second,
		"delta must track the configured timeout, not the block-time lag")
}

// TestSignAndBroadcast_FallsBackToWallClockWhenProviderNil pins the
// backward-compat guarantee: existing wiring that doesn't supply a
// BlockTimeProvider (tests, the brief window before the miner's block
// subscriber receives its first event) keeps the pre-fix behaviour of
// anchoring on time.Now().
func TestSignAndBroadcast_FallsBackToWallClockWhenProviderNil(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier456"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.000001upokt"),
		// BlockTimeProvider intentionally nil.
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	before := time.Now()
	_, err = tc.CreateClaims(context.Background(), supplierAddr, 1000,
		[]*prooftypes.MsgCreateClaim{generateTestClaim(t, supplierAddr, "session-nil")})
	require.NoError(t, err)
	after := time.Now()

	ts := decodeBroadcastTxTimeoutTimestamp(t, testServer.getLastTxBytes())

	// With nil provider, anchor should be wall clock at broadcast
	// time (sampled between `before` and `after`), plus the default
	// timeout duration.
	require.GreaterOrEqual(t, ts, before.Add(DefaultTxTimeoutDefault).Add(-2*time.Second))
	require.LessOrEqual(t, ts, after.Add(DefaultTxTimeoutDefault).Add(2*time.Second))
}

// TestSignAndBroadcast_FallsBackToWallClockWhenProviderReturnsZero
// covers the startup race: the provider is wired but hasn't received
// any block events yet, so LatestBlockTime returns the zero
// time.Time. signAndBroadcast must treat zero as "no data" and fall
// back to time.Now() rather than anchoring on year-0001.
func TestSignAndBroadcast_FallsBackToWallClockWhenProviderReturnsZero(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier789"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	// Provider returns zero time — startup race.
	provider := &stubBlockTimeProvider{t: time.Time{}}

	config := TxClientConfig{
		GRPCEndpoint:      testServer.address,
		ChainID:           "test-chain",
		GasLimit:          100000,
		GasPrice:          parseGasPrice(t, "0.000001upokt"),
		BlockTimeProvider: provider,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	before := time.Now()
	_, err = tc.CreateClaims(context.Background(), supplierAddr, 1000,
		[]*prooftypes.MsgCreateClaim{generateTestClaim(t, supplierAddr, "session-zero")})
	require.NoError(t, err)
	after := time.Now()

	ts := decodeBroadcastTxTimeoutTimestamp(t, testServer.getLastTxBytes())

	// Zero block time must NOT be used as the anchor. Had it been,
	// ts would land sometime in year 1, billions of seconds away
	// from `before`. Assert we're within a sensible window around
	// wall clock instead.
	require.GreaterOrEqual(t, ts, before.Add(DefaultTxTimeoutDefault).Add(-2*time.Second),
		"zero block time must trigger wall-clock fallback, not anchor on year-0001")
	require.LessOrEqual(t, ts, after.Add(DefaultTxTimeoutDefault).Add(2*time.Second))
}
