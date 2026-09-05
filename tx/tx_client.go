package tx

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"google.golang.org/grpc"

	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport/grpcconn"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// TestConfig holds test mode flags read once at initialization.
// These environment variables are only for testing and should not be set in production.
type TestConfig struct {
	// ForceClaimTxError forces claim transaction submission to fail (for testing claim error path)
	ForceClaimTxError bool

	// ForceProofTxError forces proof transaction submission to fail (for testing proof error path)
	ForceProofTxError bool

	// FailOriginalProofSubmit fails ONLY the lifecycle's original proof submit
	// (the SubmitProofs wrapper), letting the inclusion reconciler's resend (which
	// calls SubmitProofsReturningHash directly) succeed. Used to validate the
	// never-broadcast self-heal end-to-end: original fails → entry persisted with
	// OrigTxHash="" → reconciler resends → proof lands → claim VALIDATED.
	FailOriginalProofSubmit bool
}

var (
	testConfig     TestConfig
	testConfigOnce sync.Once
)

// getTestConfig returns the test configuration, reading environment variables once.
func getTestConfig() TestConfig {
	testConfigOnce.Do(func() {
		testConfig.ForceClaimTxError = os.Getenv("TEST_FORCE_CLAIM_TX_ERROR") == "true"
		testConfig.ForceProofTxError = os.Getenv("TEST_FORCE_PROOF_TX_ERROR") == "true"
		testConfig.FailOriginalProofSubmit = os.Getenv("TEST_FAIL_ORIGINAL_PROOF_SUBMIT") == "true"
	})
	return testConfig
}

const (
	// DefaultGasPrice is the default gas price in upokt.
	DefaultGasPrice = "0.000001upokt"

	// DefaultGasAdjustment is the default multiplier for simulated gas.
	// Applied when GasLimit=0 (auto) to add safety margin: actual_gas = simulated_gas * adjustment
	DefaultGasAdjustment = 1.7

	// DefaultChainID for the pocket network.
	DefaultChainID = "pocket"

	// DefaultTxTimeoutMin is the minimum TX broadcast deadline. Reverted
	// to 2 minutes (the original pre-dynamic-timeout value) because the
	// intermediate 30s default starved real claims whose submission
	// window still had 5+ minutes left but small per-attempt time.
	DefaultTxTimeoutMin = 2 * time.Minute

	// DefaultTxTimeoutMax is the maximum TX broadcast deadline. It is
	// anchored to the chain's latest_block_time (what the cosmos-sdk
	// ante handler checks against via ctx.BlockTime), so the relevant
	// ceiling is the cosmos-sdk hard limit for unordered TXs (10 min)
	// minus a small safety margin for the worst-case race where a new
	// block commits between our read of latest_block_time and the
	// validator processing the tx.
	//
	// 10s is ample: block production on pocket targets ~60s intervals,
	// so a single missed-block slip can't account for more than one
	// block interval of drift. The previous 500ms margin was chosen
	// under the wrong anchor (wall clock) and had to absorb arbitrary
	// chain-lag; once the anchor is block time the margin only has to
	// cover one-block-worth of in-flight settlement jitter.
	// The nonce spread is subtracted too: it is added AFTER the clamp, so a
	// deadline sitting exactly at max would carry (max + spread) to the chain.
	// Deriving the constant is what keeps that arithmetic from having to be
	// re-checked by hand every time either number moves.
	DefaultTxTimeoutMax = txTimeoutHardCeiling - txTimeoutSafetyMargin - txNonceSpread
	// txTimeoutHardCeiling is the cosmos-sdk limit the ante handler enforces:
	// x/auth/ante/sigverify.go rejects with "unordered tx ttl exceeds 10m0s"
	// when timeoutTimestamp is further than this past ctx.BlockTime().
	txTimeoutHardCeiling = 10 * time.Minute

	// txTimeoutSafetyMargin is the drift budget between the block time we
	// anchor on and the one the validator judges the tx against. It is the
	// ONLY thing standing between us and that rejection, which is why the
	// nonce spread is taken out of the deadline rather than out of here.
	txTimeoutSafetyMargin = 10 * time.Second

	// DefaultTxTimeoutDefault is the fallback TX deadline when no window-based value is injected.
	DefaultTxTimeoutDefault = 2 * time.Minute

	// DefaultTxTimeoutClockSkewBuffer is subtracted from every
	// window-based raw deadline BEFORE clamping to [min, max]. It only
	// affects the window path (raw > 0 in computeEffectiveTxTimeout):
	// it trims a safety margin off a session-window-derived deadline so
	// that min/max clamping is applied to an already-conservative value.
	//
	// It is NOT what saves us from `unordered tx ttl exceeds 10m0s`:
	// that rejection is driven by the chain's latest_block_time drifting
	// from wall clock, not by host clock skew. The fix for that is
	// anchoring timeoutTimestamp on block time (see signAndBroadcast).
	// This buffer remains useful as a window-path cushion for operators
	// who want to leave extra headroom above the min clamp.
	DefaultTxTimeoutClockSkewBuffer = 60 * time.Second
)

// BlockTimeProvider returns the timestamp of the most recent block the
// caller has observed. It is used by signAndBroadcast to anchor the
// unordered-TX timeoutTimestamp on chain time rather than wall clock.
//
// Why: cosmos-sdk's x/auth/ante/sigverify.go compares the tx's
// timeoutTimestamp against ctx.BlockTime() (the latest committed
// block's header time) and rejects with `unordered tx ttl exceeds
// 10m0s` if the delta is greater than 10 minutes. When the chain
// produces blocks slower than target — on breeze we observed 108s of
// lag between wall clock and latest_block_time while the chain was
// reporting catching_up:false — a wall-clock-anchored deadline sails
// silently over the 600s ceiling and CheckTx drops the claim.
//
// Returning the zero time.Time is a valid signal of "no block observed
// yet" (startup race); signAndBroadcast falls back to time.Now() in
// that case, preserving the pre-fix behaviour for tests and the first
// few seconds of miner startup.
type BlockTimeProvider interface {
	LatestBlockTime() time.Time
}

// TxClientConfig contains configuration for the transaction client.
type TxClientConfig struct {
	// GRPCEndpoint is the gRPC endpoint for the full node.
	// Only used if GRPCConn is nil.
	GRPCEndpoint string

	// TxRPCTimeout bounds ONE attempt's network work. Zero uses
	// DefaultTxRPCTimeout. See tx_budget.go for why the window budget is the
	// wrong clock for an RPC.
	TxRPCTimeout time.Duration

	// ConnProbeInterval is how often to probe an owned connection. Zero uses
	// DefaultTxConnProbeInterval. Ignored when the connection is shared: the
	// invariant belongs to whoever owns the connection.
	ConnProbeInterval time.Duration

	// GRPCConn is an existing gRPC connection to reuse.
	// If provided, GRPCEndpoint and UseTLS are ignored.
	// The caller is responsible for closing this connection.
	GRPCConn *grpc.ClientConn

	// ChainID is the chain ID of the network.
	ChainID string

	// GasLimit is the gas limit for transactions.
	// Set to 0 for automatic gas estimation (simulation).
	// Set to a positive value for a fixed gas limit.
	// No default - must be explicitly configured (0 for auto, or explicit value)
	GasLimit uint64

	// GasPrice is the gas price for transactions.
	GasPrice cosmostypes.DecCoin

	// GasAdjustment is the multiplier applied to simulated gas to add safety margin.
	// Only used when GasLimit=0 (automatic simulation).
	// Actual gas = simulated_gas * GasAdjustment
	// Default: 1.7 (adds 70% safety margin)
	GasAdjustment float64

	// TxTimeoutMin is the floor for window-based TX broadcast deadlines.
	// Prevents a near-expired window from producing an unreasonably short deadline.
	// Default: 2min
	TxTimeoutMin time.Duration

	// TxTimeoutMax is the cap for window-based TX broadcast deadlines.
	// See DefaultTxTimeoutMax for why the margin below the cosmos-sdk
	// 10-minute unordered-TX ceiling is one block interval, not clock jitter.
	// Default: 10min - 10s
	TxTimeoutMax time.Duration

	// TxTimeoutDefault is used when no window-based deadline is injected via context.
	// Matches the pre-existing hardcoded behaviour.
	// Default: 2min
	TxTimeoutDefault time.Duration

	// TxTimeoutClockSkewBuffer is subtracted from the raw window-based
	// deadline BEFORE clamping to [TxTimeoutMin, TxTimeoutMax]. Tune
	// higher if the miner host's clock drifts from the validator,
	// lower if both hosts are tightly synced. Default: 60s.
	TxTimeoutClockSkewBuffer time.Duration

	// UseTLS enables TLS for the gRPC connection.
	// Set to true when connecting to endpoints on port 443 or with TLS enabled.
	// Only used if GRPCConn is nil.
	// Default: false (insecure connection)
	UseTLS bool

	// BlockTimeProvider, when non-nil, supplies the chain's latest
	// observed block time. signAndBroadcast uses it as the anchor for
	// timeoutTimestamp (so the 10-minute cosmos-sdk unordered-tx TTL is
	// measured against the same clock the validator uses). When nil, or
	// when LatestBlockTime returns the zero time.Time, signAndBroadcast
	// falls back to time.Now() — this preserves the pre-fix behaviour
	// for existing tests and the brief startup window before the first
	// block event lands.
	BlockTimeProvider BlockTimeProvider
}

// TxClient provides transaction submission capabilities for the HA system.
// It supports multi-supplier signing using private keys from the KeyManager.
type TxClient struct {
	logger     logging.Logger
	config     TxClientConfig
	keyManager keys.KeyManager
	grpcConn   *grpc.ClientConn
	ownsConn   bool // true if we created the connection and should close it

	// Codec for encoding/decoding transactions
	codec       codec.Codec
	txConfig    client.TxConfig
	authQuerier authtypes.QueryClient
	txClient    txtypes.ServiceClient

	// Per-supplier account info cache
	accountCache   map[string]*authtypes.BaseAccount
	accountCacheMu sync.RWMutex

	// lastConnOKUnixNano is when an RPC last completed on this connection.
	lastConnOKUnixNano atomic.Int64

	// Connection probe (only when this client owns its connection)
	probeCancel context.CancelFunc
	probeDone   chan struct{}

	// Lifecycle
	closed bool
	mu     sync.RWMutex

	// inFlight counts broadcasts that passed the closed check. Close() waits
	// for them before closing the connection: the check and the broadcast are
	// not atomic, so without this a Close() landing in between would pull the
	// connection out from under a claim already on its way.
	inFlight sync.WaitGroup
}

// NewTxClient creates a new transaction client.
func NewTxClient(
	logger logging.Logger,
	keyManager keys.KeyManager,
	config TxClientConfig,
) (*TxClient, error) {
	// Validate: either GRPCConn or GRPCEndpoint must be provided
	if config.GRPCConn == nil && config.GRPCEndpoint == "" {
		return nil, fmt.Errorf("either GRPCConn or GRPCEndpoint is required")
	}
	if config.ChainID == "" {
		config.ChainID = DefaultChainID
	}
	// GasLimit: No default applied - 0 means automatic (simulation), non-zero means explicit limit
	// Check Denom instead of IsZero() since zero-value DecCoin has nil internal state
	if config.GasPrice.Denom == "" {
		gasPrice, err := cosmostypes.ParseDecCoin(DefaultGasPrice)
		if err != nil {
			return nil, fmt.Errorf("failed to parse default gas price: %w", err)
		}
		config.GasPrice = gasPrice
	}
	if config.GasAdjustment == 0 {
		config.GasAdjustment = DefaultGasAdjustment
	}
	if config.TxTimeoutMin <= 0 {
		config.TxTimeoutMin = DefaultTxTimeoutMin
	}
	if config.TxTimeoutMax <= 0 {
		config.TxTimeoutMax = DefaultTxTimeoutMax
	}
	// An operator-supplied max is clamped too. The schema allows 599s and the
	// example suggests it, and the nonce spread is added AFTER this value, so
	// an unclamped 599s would leave the drift budget at a few milliseconds --
	// the budget that keeps CheckTx from rejecting the tx outright.
	if maxAllowed := txTimeoutHardCeiling - txTimeoutSafetyMargin - txNonceSpread; config.TxTimeoutMax > maxAllowed {
		config.TxTimeoutMax = maxAllowed
	}
	if config.TxTimeoutDefault <= 0 {
		config.TxTimeoutDefault = DefaultTxTimeoutDefault
	}
	// Zero means "no buffer" — but operators almost never want 0.
	// `< 0` is nonsensical (would extend the deadline past the raw window).
	// Treat zero/negative as "unset" and apply the default.
	if config.TxTimeoutClockSkewBuffer <= 0 {
		config.TxTimeoutClockSkewBuffer = DefaultTxTimeoutClockSkewBuffer
	}

	var grpcConn *grpc.ClientConn
	var ownsConn bool

	if config.GRPCConn != nil {
		// Use the provided connection (caller owns it)
		grpcConn = config.GRPCConn
		ownsConn = false
	} else {
		// Build our own, through the one constructor every outbound node
		// connection goes through. Before this the tx client dialled with
		// transport credentials and nothing else -- no keepalive, no windows,
		// no backoff, no stream observer -- and it was invisible only because
		// the miner handed it the query connection instead.
		var err error
		grpcConn, err = grpcconn.New(
			grpcconn.Target{Endpoint: config.GRPCEndpoint, UseTLS: config.UseTLS},
			grpcconn.RoleTx,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
		}
		ownsConn = true
	}

	// Create codec and tx config
	cdc, txConfig := createCodecAndTxConfig()

	tc := &TxClient{
		logger:       logging.ForComponent(logger, logging.ComponentTxClient),
		config:       config,
		keyManager:   keyManager,
		grpcConn:     grpcConn,
		ownsConn:     ownsConn,
		codec:        cdc,
		txConfig:     txConfig,
		authQuerier:  authtypes.NewQueryClient(grpcConn),
		txClient:     txtypes.NewServiceClient(grpcConn),
		accountCache: make(map[string]*authtypes.BaseAccount),
	}

	// The probe is the owner's job: a shared connection is somebody else's to
	// keep alive, and two probes on one connection is one too many.
	if ownsConn {
		tc.startConnProbe()
	}

	tc.logger.Info().
		Str("endpoint", config.GRPCEndpoint).
		Str("chain_id", config.ChainID).
		Bool("shared_conn", !ownsConn).
		Uint64("gas_limit", config.GasLimit).
		Str("gas_price", config.GasPrice.String()).
		Float64("gas_adjustment", config.GasAdjustment).
		Msg("transaction client initialized")

	return tc, nil
}

// createCodecAndTxConfig creates the codec and transaction config for signing.
func createCodecAndTxConfig() (codec.Codec, client.TxConfig) {
	registry := codectypes.NewInterfaceRegistry()

	// Register necessary interfaces
	authtypes.RegisterInterfaces(registry)
	cryptocodec.RegisterInterfaces(registry)
	prooftypes.RegisterInterfaces(registry)
	sessiontypes.RegisterInterfaces(registry)

	cdc := codec.NewProtoCodec(registry)
	txConfig := authtx.NewTxConfig(cdc, authtx.DefaultSignModes)

	return cdc, txConfig
}

// CreateClaims creates and submits claim transactions for a supplier.
// Returns the TX hash for deduplication tracking.
func (tc *TxClient) CreateClaims(
	ctx context.Context,
	supplierOperatorAddr string,
	timeoutHeight int64,
	claims []*prooftypes.MsgCreateClaim,
) (string, error) {
	if !tc.enter() {
		return "", fmt.Errorf("tx client is closed")
	}
	defer tc.inFlight.Done()

	if len(claims) == 0 {
		return "", nil
	}

	// Convert claims to Msg interface
	msgs := make([]cosmostypes.Msg, len(claims))
	for i, claim := range claims {
		msgs[i] = claim
	}

	txHash, err := tc.signAndBroadcast(ctx, supplierOperatorAddr, uint64(timeoutHeight), "claim", msgs...)
	if err != nil {
		txClaimErrors.WithLabelValues(supplierOperatorAddr).Inc()
		return "", fmt.Errorf("failed to broadcast claims: %w", err)
	}

	tc.logger.Info().
		Str("supplier", supplierOperatorAddr).
		Int("num_claims", len(claims)).
		Str("tx_hash", txHash).
		Msg("claims submitted")

	txClaimsSubmitted.WithLabelValues(supplierOperatorAddr).Add(float64(len(claims)))
	return txHash, nil
}

// SubmitProofs submits proof transactions for a supplier.
// Returns the TX hash for deduplication tracking.
func (tc *TxClient) SubmitProofs(
	ctx context.Context,
	supplierOperatorAddr string,
	timeoutHeight int64,
	proofs []*prooftypes.MsgSubmitProof,
) (string, error) {
	if !tc.enter() {
		return "", fmt.Errorf("tx client is closed")
	}
	defer tc.inFlight.Done()

	if len(proofs) == 0 {
		return "", nil
	}

	// Convert proofs to Msg interface
	msgs := make([]cosmostypes.Msg, len(proofs))
	for i, proof := range proofs {
		msgs[i] = proof
	}

	txHash, err := tc.signAndBroadcast(ctx, supplierOperatorAddr, uint64(timeoutHeight), "proof", msgs...)
	if err != nil {
		// Check if error is "proof not required" - this is benign (claim already settled without proof)
		if isProofNotRequiredError(err) {
			tc.logger.Info().
				Str("supplier", supplierOperatorAddr).
				Int("num_proofs", len(proofs)).
				Msg("proof submission skipped: blockchain indicates proof not required (claim already settled)")
			// Return empty hash to indicate success without submission
			return "", nil
		}
		txProofErrors.WithLabelValues(supplierOperatorAddr).Inc()
		return "", fmt.Errorf("failed to broadcast proofs: %w", err)
	}

	tc.logger.Info().
		Str("supplier", supplierOperatorAddr).
		Int("num_proofs", len(proofs)).
		Str("tx_hash", txHash).
		Msg("proofs submitted")

	txProofsSubmitted.WithLabelValues(supplierOperatorAddr).Add(float64(len(proofs)))
	return txHash, nil
}

// txWindowTimeoutKey is the context key used to carry a window-based TX deadline.
type txWindowTimeoutKey struct{}

// txWindow is the raw window duration plus the wall-clock instant it was
// computed at.
//
// computedAt is what makes the budget belong to the WINDOW rather than to each
// attempt: the caller builds this context once and reuses it across retries
// (miner/lifecycle_callback.go), so a per-attempt WithTimeout would hand every
// retry a fresh full budget and N attempts could spend N windows' worth of a
// window that lasts one.
//
// It is wall clock at the moment the height was read, NOT the chain's block
// time anchor -- that anchor can lag wall clock, and an absolute deadline built
// on it can already be in the past, which would refuse to even try.
type txWindow struct {
	raw        time.Duration
	computedAt time.Time
}

// WithTxWindowTimeout injects a raw window-based duration into ctx.
// signAndBroadcast reads it, subtracts TxTimeoutClockSkewBuffer, then
// clamps to [TxTimeoutMin, TxTimeoutMax]. If not set, signAndBroadcast
// falls back to TxTimeoutDefault.
func WithTxWindowTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, txWindowTimeoutKey{}, txWindow{raw: d, computedAt: time.Now()})
}

// computeEffectiveTxTimeout is the pure math of the deadline decision.
// Extracted so the skew→clamp pipeline can be tested directly without
// building a full TxClient. source is one of:
//
//	"default"   — no raw window in context; fallbackDefault used
//	"min_clamp" — raw - skew fell below min
//	"max_clamp" — raw - skew exceeded max
//	"window"    — raw - skew fit inside [min, max]
//
// skew MUST be subtracted BEFORE clamping: if we clamped first and then
// subtracted, a raw value close to max would end up between max and
// max-skew, but a raw value AT max would get clamped to max and then
// land at max-skew — fine — but then the edge case where raw sits
// exactly at the cosmos-sdk hard limit (600s) would hand the chain a
// timeoutTimestamp at (now + max) with zero jitter headroom. Subtract
// first so max stays an absolute ceiling.
// txNonceSpread bounds the offset added to every unordered transaction's
// timeout timestamp, and it is what keeps the nonce unique.
//
// WHY. A cosmos-sdk unordered transaction is identified by the pair
// (timeout.UnixNano(), sender) -- x/auth/keeper/keeper.go TryAddUnorderedNonce
// -- and reusing that pair is rejected in CheckTx with "sender %s has already
// used timeout %d". The anchor is the chain's latest_block_time, which does not
// move inside a block. Measured live 2026-09-03: three sessions in
// claim_tx_error, one EXPIRED claim (8 relays, PROOF_MISSING) and one slashing
// event, with relays lost on EVERY transport -- which is what places the cause
// here rather than in one transport's path.
//
// It separates transactions in TWO regimes, and both are real, because
// computeEffectiveTxTimeout returns a different shape in each:
//
//   - CLAMPED (min_clamp / max_clamp -- all of localnet, and mainnet late in a
//     window): the deadline is a FIXED duration, so anchor+timeout advances
//     with the anchor. Blocks are naturally separated; what collides is several
//     transactions inside ONE block -- two session-end groups, the retry loop,
//     a rebroadcast landing beside a retry.
//   - WINDOW (no clamp -- mainnet early in a window): raw is
//     remaining_blocks * configured_block_time, so between blocks the anchor
//     advances by the REAL interval while the duration shrinks by the
//     CONFIGURED one. anchor+timeout is then invariant -- it points at the
//     window close, which does not move -- so a RETRY IN A LATER BLOCK
//     recomputes the same nonce as its original. That is the case the 0/5/7
//     schedule produces on purpose.
//     NOT MEASURED: the cancellation is exact only where the real block
//     interval matches the configured one to the nanosecond, and real blocks
//     drift. Plausible path, not a guaranteed mechanism.
//
// WHY ADDING IS SAFE, AND SUBTRACTING IS NOT. The ante handler makes three
// checks and an offset that only moves the timestamp LATER can trip none of
// them: it cannot make the deadline look already-passed. Never subtract. The
// ceiling is handled by deriving DefaultTxTimeoutMax from it.
//
// THE SIZE. 10ms is 10^7 slots for a problem that needs ~10^4, and it costs
// 0.1% of the drift budget instead of the 10% a full second cost. It must stay
// well under the minimum block interval so the offset cannot create an overlap
// between adjacent blocks that the clamped regime otherwise separates -- three
// orders of magnitude of headroom against localnet's 10s.
const txNonceSpread = 10 * time.Millisecond

var (
	// txNonceCounter separates transactions built by THIS process. Because it
	// is monotonic and the modulo is applied to consecutive values, uniqueness
	// within a process is EXACT, not probabilistic, for 10^7 consecutive
	// transactions. Do not add a retry-on-collision here; there is nothing to
	// retry against.
	txNonceCounter atomic.Uint64

	// txNonceBase separates PROCESSES, and only that part is probabilistic:
	// each process walks a contiguous run, so two replicas emitting K1 and K2
	// transactions overlap with probability (K1+K2-1)/10^7, not the pairwise
	// figure.
	//
	// It matters far less than it looks. The nonce is keyed by SENDER, and the
	// sender is the SUPPLIER's operator address -- so two processes can only
	// collide while both are signing for the same supplier, which is the
	// split-brain window. The real defence there is the lease drain, not this
	// seed.
	//
	// math/rand/v2 rather than crypto/rand: the property needed is "two
	// processes start far apart", not unpredictability, and it is seeded per
	// process with no error to handle. crypto/rand.Read never returns an error
	// ("It never returns an error, and always fills b entirely"), so the
	// fallback the first version carried was unreachable -- and it degraded to
	// zero, which would have made two replicas start at the SAME base and
	// collide with certainty.
	txNonceBase = mathrand.Uint64()
)

// nextTxNonceOffset returns the offset to add to one transaction's timeout.
func nextTxNonceOffset() time.Duration {
	n := (txNonceBase + txNonceCounter.Add(1)) % uint64(txNonceSpread)
	return time.Duration(n) // #nosec G115 -- bounded by the modulo above
}

func computeEffectiveTxTimeout(
	raw, skewBuffer, min, max, fallbackDefault time.Duration,
) (timeout time.Duration, source string) {
	if raw <= 0 {
		return fallbackDefault, "default"
	}
	adjusted := raw - skewBuffer
	switch {
	case adjusted < min:
		return min, "min_clamp"
	case adjusted > max:
		return max, "max_clamp"
	default:
		return adjusted, "window"
	}
}

// signAndBroadcast signs and broadcasts a transaction.
// txType should be "claim" or "proof" for proper metrics labeling.
func (tc *TxClient) signAndBroadcast(
	ctx context.Context,
	signerAddr string,
	_ uint64,
	txType string,
	msgs ...cosmostypes.Msg,
) (string, error) {
	// NOTE: No global mutex needed - unordered transactions (SetUnordered(true))
	// use sequence=0, eliminating sequence number conflicts between concurrent TXs.

	startTime := time.Now()
	defer func() {
		txBroadcastLatency.WithLabelValues(signerAddr).Observe(time.Since(startTime).Seconds())
	}()

	// The clock goes on BEFORE the first network call, not after.
	//
	// computeEffectiveTxTimeout used to run 23 lines below getAccount, which
	// left the account lookup -- a real RPC, with a cache that is cold exactly
	// after a restart or a rebalance -- with no deadline at all. A hung lookup
	// there holds a transition-subpool worker, and that subpool is per supplier
	// with a minimum of 10: ten hangs and the supplier stops transitioning,
	// silently. The helper is pure, so moving it up costs nothing.
	timeoutDuration, timeoutSource, window := tc.effectiveTxTimeout(ctx)

	ctx, cancelDeadline := tc.withBroadcastDeadline(ctx, timeoutDuration, window)
	defer cancelDeadline()

	// Get signing key
	privKey, err := tc.keyManager.GetSigner(signerAddr)
	if err != nil {
		return "", fmt.Errorf("failed to get signing key: %w", err)
	}

	// Get account info
	account, err := tc.getAccount(ctx, signerAddr)
	if err != nil {
		return "", fmt.Errorf("failed to get account: %w", err)
	}

	// Build the transaction
	txBuilder := tc.txConfig.NewTxBuilder()
	if setMsgsErr := txBuilder.SetMsgs(msgs...); setMsgsErr != nil {
		return "", fmt.Errorf("failed to set messages: %w", setMsgsErr)
	}

	// Set memo (optional)
	txBuilder.SetMemo("HA RelayMiner")

	// Set unordered=true to eliminate account sequence issues
	// With unordered, TXs don't check sequence numbers and can be included in any order
	txBuilder.SetUnordered(true)

	// Anchor timeoutTimestamp on the chain's latest_block_time, not
	// wall clock. cosmos-sdk x/auth/ante/sigverify.go:441 checks
	// `timeoutTimestamp - ctx.BlockTime() > 10 * time.Minute` and
	// rejects with `unordered tx ttl exceeds 10m0s`. When the chain
	// produces blocks slower than target (observed on breeze at 108 s
	// of block-time-vs-wall-clock lag while catching_up=false),
	// wall-clock anchoring pushes us silently over the ceiling and
	// CheckTx drops the claim — permanent economic loss.
	//
	// Fallback to time.Now() when the provider is nil (legacy wiring,
	// tests) or returns the zero time.Time (startup race before the
	// first block event). The fallback preserves the previous
	// behaviour so nothing breaks; the new default behaviour kicks in
	// automatically once the miner wires a provider.
	anchor := time.Now()
	anchorSource := "wall_clock"
	if tc.config.BlockTimeProvider != nil {
		if bt := tc.config.BlockTimeProvider.LatestBlockTime(); !bt.IsZero() {
			anchor = bt
			anchorSource = "block_time"
		}
	}
	// The offset is what keeps the unordered nonce unique. Without it every
	// transaction this process builds for one supplier inside one block
	// carries the same (timeout, sender) pair -- and in the unclamped regime,
	// so does a retry in a LATER block. See txNonceSpread.
	timeoutTimestamp := anchor.Add(timeoutDuration).Add(nextTxNonceOffset())
	txBuilder.SetTimeoutTimestamp(timeoutTimestamp)

	// Determine gas limit and fees
	var gasLimit uint64
	var feeAmount cosmostypes.Coins

	if tc.config.GasLimit == 0 {
		// Automatic gas estimation: simulate transaction to estimate gas
		simGas, simErr := tc.simulateTx(ctx, txBuilder, privKey, account)
		if simErr != nil {
			// Simulation failed and no fallback gas limit configured
			return "", fmt.Errorf("gas simulation failed (gas_limit=0 requires successful simulation): %w", simErr)
		}

		// Apply gas adjustment for safety margin
		gasLimit = uint64(float64(simGas) * tc.config.GasAdjustment)
		tc.logger.Debug().
			Str("supplier", signerAddr).
			Uint64("simulated_gas", simGas).
			Float64("gas_adjustment", tc.config.GasAdjustment).
			Uint64("final_gas_limit", gasLimit).
			Msg("gas simulation succeeded")
		feeAmount = tc.calculateFeeForGas(gasLimit)
	} else {
		// Use explicit gas limit
		gasLimit = tc.config.GasLimit
		feeAmount = tc.calculateFee()
	}

	// Set gas limit and fees
	txBuilder.SetGasLimit(gasLimit)
	txBuilder.SetFeeAmount(feeAmount)

	// Sign the transaction (unordered=true means sequence=0)
	err = tc.signTx(ctx, txBuilder, privKey, account, true)
	if err != nil {
		return "", fmt.Errorf("failed to sign transaction: %w", err)
	}

	// Encode the transaction
	txBytes, err := tc.txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return "", fmt.Errorf("failed to encode transaction: %w", err)
	}

	// Broadcast in SYNC mode (returns after CheckTx, fast)
	// Using unordered eliminates sequence mismatch issues
	// Duplicate protection handled by caller via Redis tracking
	res, err := tc.txClient.BroadcastTx(ctx, &txtypes.BroadcastTxRequest{
		TxBytes: txBytes,
		Mode:    txtypes.BroadcastMode_BROADCAST_MODE_SYNC,
	})
	if err != nil {
		return "", fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	txHash := res.TxResponse.TxHash

	// Check result (SYNC mode returns CheckTx result only)
	if res.TxResponse.Code != 0 {
		// CheckTx failed
		if isInsufficientBalanceError(res.TxResponse.RawLog) {
			txInsufficientBalanceErrors.WithLabelValues(signerAddr).Inc()
		}

		if isSequenceMismatchError(res.TxResponse.RawLog) {
			// Should NOT happen with unordered=true, but handle anyway
			tc.logger.Warn().
				Str("supplier", signerAddr).
				Str("tx_type", txType).
				Str("error", res.TxResponse.RawLog).
				Msg("sequence mismatch with unordered TX (unexpected)")
			tc.InvalidateAccount(signerAddr)
		}

		txBroadcastRejections.WithLabelValues(
			txType,
			res.TxResponse.Codespace,
			strconv.FormatUint(uint64(res.TxResponse.Code), 10),
		).Inc()

		tc.logger.Warn().
			Str("supplier", signerAddr).
			Str("tx_type", txType).
			Str("tx_hash", txHash).
			Str("codespace", res.TxResponse.Codespace).
			Uint32("code", res.TxResponse.Code).
			Str("error", res.TxResponse.RawLog).
			Msg("transaction CheckTx failed")

		return txHash, fmt.Errorf("CheckTx failed (code %d): %s", res.TxResponse.Code, res.TxResponse.RawLog)
	}

	// CheckTx passed! TX accepted to mempool
	tc.logger.Info().
		Str("supplier", signerAddr).
		Str("tx_type", txType).
		Str("tx_hash", txHash).
		Str("timeout_source", timeoutSource).
		Str("anchor_source", anchorSource).
		Time("anchor", anchor).
		Dur("timeout_duration", timeoutDuration).
		Time("timeout_timestamp", timeoutTimestamp).
		Msg("transaction accepted to mempool (unordered)")

	// NOTE: We don't increment sequence for unordered TXs (they don't use sequence numbers)

	txBroadcastsTotal.WithLabelValues(signerAddr).Inc()
	// Real traffic counts as proof the connection is alive, so idle_seconds on
	// a probe failure measures silence and not merely time.
	tc.markConnOK()
	return txHash, nil
}

// calculateFee calculates the transaction fee based on configured gas limit.
// This is the MAXIMUM fee we're willing to pay (set before broadcast).
func (tc *TxClient) calculateFee() cosmostypes.Coins {
	return tc.calculateFeeForGas(tc.config.GasLimit)
}

// calculateFeeForGas calculates the transaction fee for a given gas limit.
func (tc *TxClient) calculateFeeForGas(gasLimit uint64) cosmostypes.Coins {
	gasLimitDec := math.LegacyNewDec(int64(gasLimit))
	feeAmount := tc.config.GasPrice.Amount.Mul(gasLimitDec)

	// Truncate and add 1 if there's a remainder to ensure we don't underpay
	feeInt := feeAmount.TruncateInt()
	if feeAmount.Sub(math.LegacyNewDecFromInt(feeInt)).IsPositive() {
		feeInt = feeInt.Add(math.OneInt())
	}

	return cosmostypes.NewCoins(cosmostypes.NewCoin(tc.config.GasPrice.Denom, feeInt))
}

// signTx signs a transaction with the given private key.
func (tc *TxClient) signTx(
	ctx context.Context,
	txBuilder client.TxBuilder,
	privKey cryptotypes.PrivKey,
	account *authtypes.BaseAccount,
	unordered bool,
) error {
	pubKey := privKey.PubKey()
	signMode := signing.SignMode_SIGN_MODE_DIRECT

	// For unordered transactions, sequence MUST be 0
	sequence := account.Sequence
	if unordered {
		sequence = 0
	}

	// Set signature info placeholder
	sigV2 := signing.SignatureV2{
		PubKey: pubKey,
		Data: &signing.SingleSignatureData{
			SignMode:  signMode,
			Signature: nil,
		},
		Sequence: sequence,
	}

	if err := txBuilder.SetSignatures(sigV2); err != nil {
		return fmt.Errorf("failed to set signature placeholder: %w", err)
	}

	// Build sign data
	signerData := authsigning.SignerData{
		ChainID:       tc.config.ChainID,
		AccountNumber: account.AccountNumber,
		Sequence:      sequence,
		PubKey:        pubKey,
		Address:       account.Address,
	}

	// Get bytes to sign using the sign mode handler
	bytesToSign, err := authsigning.GetSignBytesAdapter(
		ctx,
		tc.txConfig.SignModeHandler(),
		signMode,
		signerData,
		txBuilder.GetTx(),
	)
	if err != nil {
		return fmt.Errorf("failed to get sign bytes: %w", err)
	}

	// Sign
	signature, err := privKey.Sign(bytesToSign)
	if err != nil {
		return fmt.Errorf("failed to sign: %w", err)
	}

	// Set the actual signature
	sigV2.Data = &signing.SingleSignatureData{
		SignMode:  signMode,
		Signature: signature,
	}

	if err := txBuilder.SetSignatures(sigV2); err != nil {
		return fmt.Errorf("failed to set signature: %w", err)
	}

	return nil
}

// getAccount retrieves account info from chain or cache.
func (tc *TxClient) getAccount(ctx context.Context, addr string) (*authtypes.BaseAccount, error) {
	// Check cache first
	tc.accountCacheMu.RLock()
	if account, ok := tc.accountCache[addr]; ok {
		tc.accountCacheMu.RUnlock()
		return account, nil
	}
	tc.accountCacheMu.RUnlock()

	// Query the chain WITHOUT holding the lock.
	//
	// This used to take the write lock and defer its release across the RPC, so
	// every supplier's first signature -- a cold cache is the normal state
	// after a restart or a rebalance -- went through the chain ONE AT A TIME,
	// for suppliers that share nothing but this map. That serialization sits in
	// front of everything else on the signing path, so it is also the first
	// thing any measurement of connection concurrency would have measured:
	// a number that says "the connection is the bottleneck" while the real
	// bottleneck is this mutex.
	//
	// The cost of releasing it is that two callers can query the SAME address
	// at once and both write the result. That is harmless -- the value is the
	// same account and the map converges -- and it is the trade the previous
	// shape was avoiding at the price of serializing DIFFERENT addresses, which
	// is the case that actually happens.
	res, err := tc.authQuerier.Account(ctx, &authtypes.QueryAccountRequest{
		Address: addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query account: %w", err)
	}

	var account authtypes.BaseAccount
	if err := tc.codec.UnpackAny(res.Account, &account); err != nil {
		// Try unpacking as BaseAccount directly
		if err := account.Unmarshal(res.Account.Value); err != nil {
			return nil, fmt.Errorf("failed to unpack account: %w", err)
		}
	}

	tc.accountCacheMu.Lock()
	// Another caller may have stored it while this RPC was in flight. Keep the
	// stored pointer so concurrent callers share one instance rather than each
	// holding its own copy of the same account.
	if existing, ok := tc.accountCache[addr]; ok {
		tc.accountCacheMu.Unlock()
		return existing, nil
	}
	tc.accountCache[addr] = &account
	tc.accountCacheMu.Unlock()

	return &account, nil
}

// NOTE: incrementSequence() removed - not needed for unordered transactions in SYNC mode

// InvalidateAccount removes an account from the cache.
func (tc *TxClient) InvalidateAccount(addr string) {
	tc.accountCacheMu.Lock()
	defer tc.accountCacheMu.Unlock()
	delete(tc.accountCache, addr)
}

// NOTE: waitForTxCommit() removed - not needed in SYNC mode (doesn't wait for commit)

// queryLastTxFeeUpokt queries the chain for the most recent successful
// transaction whose body contains a message of the given type, and returns
// its fee in upokt. Used by the economic-viability check to calibrate fees
// against observed on-chain reality rather than a hardcoded constant.
//
// Returns (fee, nil) on success, (0, err) on query error or no result.
// Multi-denom fees are collapsed to the upokt component only.
func (tc *TxClient) queryLastTxFeeUpokt(ctx context.Context, msgTypeURL string) (uint64, error) {
	req := &txtypes.GetTxsEventRequest{
		Events:  []string{fmt.Sprintf("message.action='%s'", msgTypeURL)},
		OrderBy: txtypes.OrderBy_ORDER_BY_DESC,
		Limit:   1,
	}
	resp, err := tc.txClient.GetTxsEvent(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("GetTxsEvent for %s: %w", msgTypeURL, err)
	}
	if len(resp.Txs) == 0 {
		return 0, fmt.Errorf("no recent %s txs found", msgTypeURL)
	}
	return extractUpoktFeeFromTx(resp.Txs[0], msgTypeURL)
}

// extractUpoktFeeFromTx extracts the positive upokt fee amount from a cosmos
// transaction. Returns an error when the tx has no fee info, when no upokt
// denomination is present, or when the amount is zero/negative. Pure helper —
// split out of queryLastTxFeeUpokt so it can be unit-tested without a real
// cosmos Service client.
func extractUpoktFeeFromTx(tx *txtypes.Tx, msgTypeURL string) (uint64, error) {
	if tx == nil {
		return 0, fmt.Errorf("tx %s is nil", msgTypeURL)
	}
	fee := tx.GetAuthInfo().GetFee()
	if fee == nil {
		return 0, fmt.Errorf("tx %s has no fee info", msgTypeURL)
	}
	for _, coin := range fee.Amount {
		if coin.Denom == "upokt" {
			if !coin.Amount.IsPositive() {
				return 0, fmt.Errorf("tx %s fee is zero or negative", msgTypeURL)
			}
			return coin.Amount.Uint64(), nil
		}
	}
	return 0, fmt.Errorf("tx %s fee has no upokt component", msgTypeURL)
}

// simulateTx simulates a transaction to estimate gas usage.
func (tc *TxClient) simulateTx(
	ctx context.Context,
	txBuilder client.TxBuilder,
	privKey cryptotypes.PrivKey,
	account *authtypes.BaseAccount,
) (uint64, error) {
	// Sign with unordered=true to match actual broadcast (for accurate gas estimation)
	if err := tc.signTx(ctx, txBuilder, privKey, account, true); err != nil {
		return 0, fmt.Errorf("failed to sign transaction for simulation: %w", err)
	}

	// Encode the transaction
	txBytes, err := tc.txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return 0, fmt.Errorf("failed to encode transaction for simulation: %w", err)
	}

	// Simulate the transaction
	simRes, err := tc.txClient.Simulate(ctx, &txtypes.SimulateRequest{
		TxBytes: txBytes,
	})
	if err != nil {
		return 0, fmt.Errorf("simulation failed: %w", err)
	}

	if simRes.GasInfo == nil {
		return 0, fmt.Errorf("simulation returned nil gas info")
	}

	return simRes.GasInfo.GasUsed, nil
}

// NOTE: calculateActualFee() removed - not available in SYNC mode (only CheckTx, no execution result)

// isInsufficientBalanceError checks if the error message indicates insufficient balance.
func isInsufficientBalanceError(errorMsg string) bool {
	// Common error patterns from Cosmos SDK
	insufficientFundsPatterns := []string{
		"insufficient funds",
		"insufficient account balance",
		"spendable balance",
	}

	errorLower := strings.ToLower(errorMsg)
	for _, pattern := range insufficientFundsPatterns {
		if strings.Contains(errorLower, pattern) {
			return true
		}
	}
	return false
}

// isProofNotRequiredError checks if the error indicates proof is not required for the claim.
// This happens when:
// - The claim didn't meet the ProofRequestProbability threshold (probabilistic proof selection)
// - The claim was already settled without requiring proof
// - There's a timing race where the miner thinks proof is required but blockchain says it's not
// This is a benign condition - no proof submission needed, claim already settled.
func isProofNotRequiredError(err error) bool {
	if err == nil {
		return false
	}
	errorMsg := strings.ToLower(err.Error())
	return strings.Contains(errorMsg, "proof not required")
}

// isSequenceMismatchError checks if the error message indicates account sequence mismatch.
func isSequenceMismatchError(errorMsg string) bool {
	// Common error patterns from Cosmos SDK
	sequenceMismatchPatterns := []string{
		"account sequence mismatch",
		"incorrect account sequence",
		"sequence mismatch",
	}

	errorLower := strings.ToLower(errorMsg)
	for _, pattern := range sequenceMismatchPatterns {
		if strings.Contains(errorLower, pattern) {
			return true
		}
	}
	return false
}

// enter registers a broadcast as in flight, or reports that the client is
// closed. Every caller that gets true owes an inFlight.Done(), and Close()
// waits for all of them.
//
// The registration happens under the SAME read lock that reads closed, which
// is what makes it safe: Close() sets closed under the write lock, so no
// caller can register after that, and every caller that did register was
// already counted when Close() starts waiting.
func (tc *TxClient) enter() bool {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	if tc.closed {
		return false
	}
	tc.inFlight.Add(1)
	return true
}

// Close closes the transaction client.
// If the client was created with a shared gRPC connection, it will not be closed.
func (tc *TxClient) Close() error {
	tc.mu.Lock()
	if tc.closed {
		tc.mu.Unlock()
		return nil
	}
	tc.closed = true
	cancelProbe, probeDone := tc.probeCancel, tc.probeDone
	// Released before waiting: a caller blocked on the read lock would only be
	// delayed, but holding a lock across two waits is how the next change to
	// this function introduces a deadlock.
	tc.mu.Unlock()

	// Stop the probe and wait for the broadcasts that already passed the
	// closed check. Both must finish before the connection goes: closing it
	// underneath either is the failure this client is being taught to survive.
	if cancelProbe != nil {
		cancelProbe()
	}
	if probeDone != nil {
		<-probeDone
	}
	tc.inFlight.Wait()

	// Only close the connection if we created it ourselves
	if tc.ownsConn && tc.grpcConn != nil {
		if err := tc.grpcConn.Close(); err != nil {
			return fmt.Errorf("failed to close gRPC connection: %w", err)
		}
	}

	tc.logger.Info().Msg("transaction client closed")
	return nil
}

// =============================================================================
// SupplierClient wrapper for compatibility with pkg/client interfaces
// =============================================================================

// HASupplierClient wraps TxClient to implement the client.SupplierClient interface.
type HASupplierClient struct {
	txClient     *TxClient
	operatorAddr string
	logger       logging.Logger

	// lastClaimTxHash stores the TX hash of the last claim submission (for deduplication)
	lastClaimTxHash string
	lastClaimTxMu   sync.RWMutex

	// lastProofTxHash stores the TX hash of the last proof submission (for deduplication)
	lastProofTxHash string
	lastProofTxMu   sync.RWMutex

	// feeCacheUpokt is the cached sum of the most recently observed claim
	// tx fee + proof tx fee on chain. It is populated lazily by querying the
	// chain for the most recent successful MsgCreateClaim and MsgSubmitProof
	// transactions, and refreshed at most once per feeCacheTTL.
	feeCacheMu    sync.RWMutex
	feeCacheUpokt uint64
	feeCacheTime  time.Time
}

// feeCacheTTL is how long the observed claim+proof fee pair is reused before
// re-querying the chain. This is the fallback for the case where a supplier
// sits idle between claim windows and the cache never gets refreshed by a
// successful submission — most refreshes come from the post-submit
// InvalidateFeeCache path rather than from TTL expiry. Kept short (tens of
// seconds) so that a transient network fee spike auto-expires quickly
// instead of persisting for the full claim/proof cycle.
const feeCacheTTL = 30 * time.Second

// InvalidateFeeCache clears the cached claim+proof fee estimate so the next
// GetEstimatedFeeUpokt call re-queries the chain. Called after a successful
// claim or proof submission: we just paid the real fee, so any in-memory
// ceiling from a previous chain observation is now stale and (during a
// transient spike) can be much higher than reality. Holding on to that
// over-estimate would cause the economic-viability check to skip sessions
// whose legitimate reward sits below the stale ceiling but above the real
// fee — silently dropping claims with healthy relay counts.
func (c *HASupplierClient) InvalidateFeeCache() {
	c.feeCacheMu.Lock()
	defer c.feeCacheMu.Unlock()
	c.feeCacheUpokt = 0
	c.feeCacheTime = time.Time{}
}

// NewHASupplierClient creates a new supplier client for a specific operator.
func NewHASupplierClient(
	txClient *TxClient,
	operatorAddr string,
	logger logging.Logger,
) *HASupplierClient {
	supplierLogger := logger.With().Str("supplier", operatorAddr).Logger()

	// DEBUG/TEST: Log test mode environment variables at startup
	testCfg := getTestConfig()
	if testCfg.ForceClaimTxError {
		supplierLogger.Warn().Msg("TEST MODE: TEST_FORCE_CLAIM_TX_ERROR detected - will force claim TX errors")
	}
	if testCfg.ForceProofTxError {
		supplierLogger.Warn().Msg("TEST MODE: TEST_FORCE_PROOF_TX_ERROR detected - will force proof TX errors")
	}

	return &HASupplierClient{
		txClient:     txClient,
		operatorAddr: operatorAddr,
		logger:       supplierLogger,
	}
}

// minFeePerTxUpokt is the mathematical floor for any single tx fee in this
// system. Cosmos's fee computation is ceiling(gas_limit × gas_price), and
// with gas_price = 0.000001 upokt (config default) and any positive gas, the
// fraction always rounds up to at least 1 upokt. So:
//
//	minFeePerTxUpokt = ceiling(anything > 0 × 0.000001) = 1
//
// This is a protocol floor, not a hardcoded constant — you literally cannot
// pay less for a tx that consumes any gas at all.
const minFeePerTxUpokt uint64 = 1

// minClaimAndProofCostUpokt is the protocol floor for submitting a claim +
// proof pair: 2 × minFeePerTxUpokt = 2 upokt. The economic-viability check
// uses this as the lower bound and refines upward with on-chain observations.
const minClaimAndProofCostUpokt uint64 = 2 * minFeePerTxUpokt

// GetEstimatedFeeUpokt returns the expected combined cost (claim tx + proof
// tx) in upokt for the economic viability decision.
//
// Resolution order:
//  1. If the local cache is fresh, return it.
//  2. Otherwise query the chain for the most recent successful
//     MsgCreateClaim and MsgSubmitProof txs, sum their fees, cache, return.
//  3. If either query fails or returns zero, return the protocol floor
//     (2 upokt — the minimum possible fee pair; see minClaimAndProofCostUpokt).
//
// The function never returns 0: the floor ensures callers always have a
// defensible lower bound.
func (c *HASupplierClient) GetEstimatedFeeUpokt(ctx context.Context) uint64 {
	c.feeCacheMu.RLock()
	if c.feeCacheUpokt > 0 && time.Since(c.feeCacheTime) < feeCacheTTL {
		v := c.feeCacheUpokt
		c.feeCacheMu.RUnlock()
		return v
	}
	c.feeCacheMu.RUnlock()

	claimFee, claimErr := c.txClient.queryLastTxFeeUpokt(ctx, "/pocket.proof.MsgCreateClaim")
	proofFee, proofErr := c.txClient.queryLastTxFeeUpokt(ctx, "/pocket.proof.MsgSubmitProof")

	// Fall back to the mathematical floor on any query error or zero result.
	if claimErr != nil || claimFee == 0 {
		claimFee = minFeePerTxUpokt
	}
	if proofErr != nil || proofFee == 0 {
		proofFee = minFeePerTxUpokt
	}
	total := claimFee + proofFee
	if total < minClaimAndProofCostUpokt {
		total = minClaimAndProofCostUpokt
	}

	c.feeCacheMu.Lock()
	c.feeCacheUpokt = total
	c.feeCacheTime = time.Now()
	c.feeCacheMu.Unlock()

	return total
}

// CreateClaims implements client.SupplierClient.
// CreateClaims implements client.SupplierClient. The resulting tx hash is
// stashed for retrieval via GetLastClaimTxHash. Callers that need the hash
// atomically (e.g. concurrent in-window rebroadcasts sharing one client) should
// use CreateClaimsReturningHash, which avoids the CreateClaims()+GetLastClaimTxHash()
// cross-attribution race.
func (c *HASupplierClient) CreateClaims(
	ctx context.Context,
	timeoutHeight int64,
	claimMsgs ...pocktclient.MsgCreateClaim,
) error {
	_, err := c.CreateClaimsReturningHash(ctx, timeoutHeight, claimMsgs...)
	return err
}

// CreateClaimsReturningHash submits claims and returns the resulting tx hash
// directly, alongside still stashing it for GetLastClaimTxHash. Returning the
// hash inline lets concurrent rebroadcasts of different sessions through the
// same shared client each record their own tx hash, instead of racing on the
// shared lastClaimTxHash field.
func (c *HASupplierClient) CreateClaimsReturningHash(
	ctx context.Context,
	timeoutHeight int64,
	claimMsgs ...pocktclient.MsgCreateClaim,
) (string, error) {
	// DEBUG/TEST: Force claim TX error to test claim_tx_error state transition
	// Set environment variable TEST_FORCE_CLAIM_TX_ERROR=true to enable
	if testCfg := getTestConfig(); testCfg.ForceClaimTxError {
		c.logger.Warn().
			Msg("TEST MODE: TEST_FORCE_CLAIM_TX_ERROR detected - forcing claim TX error")
		return "", fmt.Errorf("TEST MODE: simulated claim transaction error")
	}

	claims := make([]*prooftypes.MsgCreateClaim, len(claimMsgs))
	for i, msg := range claimMsgs {
		claim, ok := msg.(*prooftypes.MsgCreateClaim)
		if !ok {
			return "", fmt.Errorf("invalid claim message type: %T", msg)
		}
		claims[i] = claim
	}

	// Call TxClient and capture TX hash for deduplication
	txHash, err := c.txClient.CreateClaims(ctx, c.operatorAddr, timeoutHeight, claims)
	if err != nil {
		return "", err
	}

	// Store TX hash for retrieval by caller (1 line after broadcast)
	c.lastClaimTxMu.Lock()
	c.lastClaimTxHash = txHash
	c.lastClaimTxMu.Unlock()

	// The fee we just paid is now the freshest observation available.
	// Drop any cached chain-observed estimate so the next economic-viability
	// check reflects what the network is actually charging rather than a
	// possibly-stale spike.
	c.InvalidateFeeCache()

	return txHash, nil
}

// SubmitProofs implements client.SupplierClient. The resulting tx hash is
// stashed for retrieval via GetLastProofTxHash. Callers that need the hash
// atomically (e.g. concurrent in-window rebroadcasts sharing one client) should
// use SubmitProofsReturningHash instead, which avoids the
// SubmitProofs()+GetLastProofTxHash() cross-attribution race.
func (c *HASupplierClient) SubmitProofs(
	ctx context.Context,
	timeoutHeight int64,
	proofMsgs ...pocktclient.MsgSubmitProof,
) error {
	// TEST: fail ONLY the lifecycle's original submit (this wrapper), so the
	// inclusion reconciler's self-heal resend (SubmitProofsReturningHash, called
	// directly) can still land. Validates the never-broadcast self-heal path.
	if getTestConfig().FailOriginalProofSubmit {
		c.logger.Warn().Msg("TEST MODE: TEST_FAIL_ORIGINAL_PROOF_SUBMIT - failing original proof submit (reconciler resend will recover)")
		return fmt.Errorf("TEST MODE: simulated original proof submit error")
	}
	_, err := c.SubmitProofsReturningHash(ctx, timeoutHeight, proofMsgs...)
	return err
}

// SubmitProofsReturningHash submits proofs and returns the resulting tx hash
// directly, alongside still stashing it for GetLastProofTxHash. Returning the
// hash inline lets concurrent rebroadcasts of different sessions through the
// same shared client each record their own tx hash, instead of racing on the
// shared lastProofTxHash field.
func (c *HASupplierClient) SubmitProofsReturningHash(
	ctx context.Context,
	timeoutHeight int64,
	proofMsgs ...pocktclient.MsgSubmitProof,
) (string, error) {
	// DEBUG/TEST: Force proof TX error to test proof_tx_error state transition
	// Set environment variable TEST_FORCE_PROOF_TX_ERROR=true to enable
	if testCfg := getTestConfig(); testCfg.ForceProofTxError {
		c.logger.Warn().
			Msg("TEST MODE: TEST_FORCE_PROOF_TX_ERROR detected - forcing proof TX error")
		return "", fmt.Errorf("TEST MODE: simulated proof transaction error")
	}

	proofs := make([]*prooftypes.MsgSubmitProof, len(proofMsgs))
	for i, msg := range proofMsgs {
		proof, ok := msg.(*prooftypes.MsgSubmitProof)
		if !ok {
			return "", fmt.Errorf("invalid proof message type: %T", msg)
		}
		proofs[i] = proof
	}

	// Call TxClient and capture TX hash for deduplication
	txHash, err := c.txClient.SubmitProofs(ctx, c.operatorAddr, timeoutHeight, proofs)
	if err != nil {
		return "", err
	}

	// Store TX hash for retrieval by caller (1 line after broadcast)
	c.lastProofTxMu.Lock()
	c.lastProofTxHash = txHash
	c.lastProofTxMu.Unlock()

	// Same rationale as CreateClaims — refresh the cached estimate from the
	// most recent successful submission.
	c.InvalidateFeeCache()

	return txHash, nil
}

// OperatorAddress implements client.SupplierClient.
func (c *HASupplierClient) OperatorAddress() string {
	return c.operatorAddr
}

// GetLastClaimTxHash returns the TX hash of the last claim submission.
// This is used for deduplication tracking in Redis.
func (c *HASupplierClient) GetLastClaimTxHash() string {
	c.lastClaimTxMu.RLock()
	defer c.lastClaimTxMu.RUnlock()
	return c.lastClaimTxHash
}

// GetLastProofTxHash returns the TX hash of the last proof submission.
// This is used for deduplication tracking in Redis.
func (c *HASupplierClient) GetLastProofTxHash() string {
	c.lastProofTxMu.RLock()
	defer c.lastProofTxMu.RUnlock()
	return c.lastProofTxHash
}
