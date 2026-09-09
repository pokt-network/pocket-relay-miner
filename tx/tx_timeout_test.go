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
	defer func() { _ = km.Close() }()

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
	defer func() { _ = tc.Close() }()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-anchor"),
	}
	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.NoError(t, err)

	ts := decodeBroadcastTxTimeoutTimestamp(t, testServer.getLastTxBytes())

	// Invariant #1: the anchor was block time, not wall clock. The
	// delta between ts and laggedBlockTime must equal the configured
	// timeout duration (default path → DefaultTxTimeoutMax).
	require.WithinDuration(t, laggedBlockTime.Add(DefaultTxTimeoutMax), ts, 2*time.Second,
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
	// delta of roughly (DefaultTxTimeoutMax + 108s). Prove we're
	// nowhere near that — this is what protects us.
	require.Less(t, delta, DefaultTxTimeoutMax+txNonceSpread,
		"delta must track the configured timeout, not the block-time lag. The bound is "+
			"the timeout plus the nonce spread, named rather than a round second: the "+
			"offset is added after the clamp, and writing a literal here would silently "+
			"stop bounding anything if the spread ever grew past it")
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
	defer func() { _ = km.Close() }()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.000001upokt"),
		// BlockTimeProvider intentionally nil.
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer func() { _ = tc.Close() }()

	before := time.Now()
	_, err = tc.CreateClaims(context.Background(), supplierAddr, 1000,
		[]*prooftypes.MsgCreateClaim{generateTestClaim(t, supplierAddr, "session-nil")})
	require.NoError(t, err)
	after := time.Now()

	ts := decodeBroadcastTxTimeoutTimestamp(t, testServer.getLastTxBytes())

	// With nil provider, anchor should be wall clock at broadcast
	// time (sampled between `before` and `after`), plus the default
	// timeout duration.
	require.GreaterOrEqual(t, ts, before.Add(DefaultTxTimeoutMax).Add(-2*time.Second))
	require.LessOrEqual(t, ts, after.Add(DefaultTxTimeoutMax).Add(2*time.Second))
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
	defer func() { _ = km.Close() }()

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
	defer func() { _ = tc.Close() }()

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
	require.GreaterOrEqual(t, ts, before.Add(DefaultTxTimeoutMax).Add(-2*time.Second),
		"zero block time must trigger wall-clock fallback, not anchor on year-0001")
	require.LessOrEqual(t, ts, after.Add(DefaultTxTimeoutMax).Add(2*time.Second))
}
