package tx

import (
	"context"
	"errors"
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
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"

	grpc1 "github.com/cosmos/gogoproto/grpc"
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

	// MaxConcurrent caps concurrent broadcasts on this connection. Zero uses
	// DefaultTxMaxConcurrent. See tx_permits.go for what it bounds and what
	// evidence would move it.
	MaxConcurrent int

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

	// pool is this client's own set of connections, and it is nil exactly when
	// the connection was handed in by the caller: a shared connection is
	// somebody else's to size, probe and close, and wrapping it in a pool
	// would claim all three.
	pool *grpcconn.Pool

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

	// Lifecycle.
	//
	// closed is an atomic and not a mutex-guarded bool because a caller waiting
	// for a permit must not hold a lock -- see acquirePermit.
	closed atomic.Bool

	// permits bounds concurrent broadcasts AND is the quiesce mechanism:
	// nobody broadcasts without one, and Close() takes them all. There is no
	// second WaitGroup; two things waiting for the same thing is how the next
	// change to Close() writes a deadlock.
	//
	// The invariant is about BROADCASTS, not about the connection: the probe
	// and the fee query use it without a permit, deliberately, and Close()
	// documents what that leaves unguarded.
	permits       *semaphore.Weighted
	permitWaiters atomic.Int64
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

	var grpcConn *grpc.ClientConn
	var pool *grpcconn.Pool
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
		pool, err = grpcconn.NewPool(
			grpcconn.Target{Endpoint: config.GRPCEndpoint, UseTLS: config.UseTLS},
			grpcconn.RoleTx,
			grpcconn.DefaultPoolFloor,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
		}
		ownsConn = true
	}

	// Create codec and tx config
	cdc, txConfig := createCodecAndTxConfig()

	// The stubs take gogoproto's grpc.ClientConn -- Invoke plus NewStream --
	// so a pool goes here in place of a connection and nothing above this
	// constructor knows the difference.
	var stubConn grpc1.ClientConn = grpcConn
	if pool != nil {
		stubConn = pool
	}

	tc := &TxClient{
		logger:       logging.ForComponent(logger, logging.ComponentTxClient),
		config:       config,
		keyManager:   keyManager,
		grpcConn:     grpcConn,
		pool:         pool,
		ownsConn:     ownsConn,
		codec:        cdc,
		txConfig:     txConfig,
		authQuerier:  authtypes.NewQueryClient(stubConn),
		txClient:     txtypes.NewServiceClient(stubConn),
		accountCache: make(map[string]*authtypes.BaseAccount),
	}
	tc.permits = newPermits(tc.maxConcurrent())

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
) (string, SignedTxPayload, error) {
	if err := tc.acquirePermit(ctx); err != nil {
		return "", SignedTxPayload{}, err
	}
	defer tc.releasePermit()

	if len(claims) == 0 {
		return "", SignedTxPayload{}, nil
	}

	// Convert claims to Msg interface
	msgs := make([]cosmostypes.Msg, len(claims))
	for i, claim := range claims {
		msgs[i] = claim
	}

	txHash, signed, err := tc.signAndBroadcastReturningSigned(ctx, supplierOperatorAddr, timeoutHeight, "claim", msgs...)
	if err != nil {
		txClaimErrors.WithLabelValues(supplierOperatorAddr).Inc()
		// The payload travels WITH the error, and that is the case the whole
		// cache exists for: a broadcast that got no answer is the one where we
		// never learned whether it arrived, so those bytes are exactly the ones
		// worth re-injecting. Dropping them here would leave the first row of
		// the resend table with nothing to re-send.
		return "", signed, fmt.Errorf("failed to broadcast claims: %w", err)
	}

	tc.logger.Info().
		Str("supplier", supplierOperatorAddr).
		Int("num_claims", len(claims)).
		Str("tx_hash", txHash).
		Msg("claims submitted")

	txClaimsSubmitted.WithLabelValues(supplierOperatorAddr).Add(float64(len(claims)))
	return txHash, signed, nil
}

// SubmitProofs submits proof transactions for a supplier.
// Returns the TX hash for deduplication tracking.
func (tc *TxClient) SubmitProofs(
	ctx context.Context,
	supplierOperatorAddr string,
	timeoutHeight int64,
	proofs []*prooftypes.MsgSubmitProof,
) (string, SignedTxPayload, error) {
	if err := tc.acquirePermit(ctx); err != nil {
		return "", SignedTxPayload{}, err
	}
	defer tc.releasePermit()

	if len(proofs) == 0 {
		return "", SignedTxPayload{}, nil
	}

	// Convert proofs to Msg interface
	msgs := make([]cosmostypes.Msg, len(proofs))
	for i, proof := range proofs {
		msgs[i] = proof
	}

	txHash, signed, err := tc.signAndBroadcastReturningSigned(ctx, supplierOperatorAddr, timeoutHeight, "proof", msgs...)
	if err != nil {
		// Check if error is "proof not required" - this is benign (claim already settled without proof)
		if isProofNotRequiredError(err) {
			txProofNotRequired.WithLabelValues(supplierOperatorAddr).Inc()
			tc.logger.Info().
				Str("supplier", supplierOperatorAddr).
				Int("num_proofs", len(proofs)).
				Msg("proof submission skipped: blockchain indicates proof not required (claim already settled)")
			// Reporting this as a successful submission is what hid it: the batch
			// travels as ONE transaction, so a single not-required proof fails the
			// whole tx and the other N-1 were never transmitted. Returning the
			// sentinel keeps the fact askable with errors.Is and the rejection
			// readable with errors.As; both wrap so neither is lost.
			return "", SignedTxPayload{}, fmt.Errorf("%w: %w", ErrTxProofNotRequired, err)
		}
		txProofErrors.WithLabelValues(supplierOperatorAddr).Inc()
		// Same as the claim path: the bytes of a send whose answer never
		// arrived are the ones a resend must re-inject.
		return "", signed, fmt.Errorf("failed to broadcast proofs: %w", err)
	}

	tc.logger.Info().
		Str("supplier", supplierOperatorAddr).
		Int("num_proofs", len(proofs)).
		Str("tx_hash", txHash).
		Msg("proofs submitted")

	txProofsSubmitted.WithLabelValues(supplierOperatorAddr).Add(float64(len(proofs)))
	return txHash, signed, nil
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
	// regime records WHY raw has the value it has -- see WindowTimeout. It
	// travels with the value rather than being re-derived at the broadcast,
	// because re-deriving needs the window length and block time, which the
	// resend path does not have: supplier_manager has no access to shared
	// params at all. Carrying it is also what keeps the label honest when the
	// budget was inherited rather than computed.
	regime string
}

// WithTxWindowTimeout injects the window budget into ctx, together with the
// regime that produced it. Both come from WindowTimeout, and signAndBroadcast
// uses the value as given: there is no adjustment left on the broadcast side.
//
// Pass the SAME context to every attempt for one window -- that is what makes
// the budget belong to the window rather than to each try.
func WithTxWindowTimeout(ctx context.Context, d time.Duration, regime string) context.Context {
	return context.WithValue(ctx, txWindowTimeoutKey{}, txWindow{
		raw:        d,
		computedAt: time.Now(),
		regime:     regime,
	})
}

// TxWindowFrom reports the window budget a context carries, and whether it
// carries one at all.
//
// The writer above has been public since it existed; the reader was not, and the
// key is private, so a budget could be PUT INTO a context from anywhere and read
// back from nowhere. A value that is writable from outside and unreadable from
// outside makes "the budget travelled" unobservable by construction -- not hard
// to check, impossible to express -- which is why nothing outside this package
// ever asserted it. Declaring the missing half of the pair is the fix; the
// asymmetry was the defect.
//
// It is honest about its readers: `effectiveTxTimeout` does NOT call it, because
// that one needs the whole txWindow including computedAt, and bending it to go
// through here would be dressing up a seam as a refactor. Today the caller is a
// test in another package, and that is the point -- there was no way to write one.
func TxWindowFrom(ctx context.Context) (time.Duration, string, bool) {
	window, ok := ctx.Value(txWindowTimeoutKey{}).(txWindow)
	if !ok {
		return 0, "", false
	}
	return window.raw, window.regime, true
}

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
// It only has to separate transactions inside ONE block. The duration added to
// the anchor is constant for a whole window -- WindowTimeout takes the window's
// full length, not the blocks left in it, and a resend reuses the budget stored
// with its original (the ceiling, if an older binary stripped it) -- so
// anchor+timeout moves with the anchor, and the chain
// only accepts a block whose time is strictly after the previous one. Two
// transactions of one window built against different latest blocks are
// therefore apart by the interval between those blocks. What collides is
// several built against the SAME latest block: two session-end groups, the
// retry loop, a rebroadcast landing beside a retry.
//
// A retry in a LATER block used to recompute its original's nonce, when the
// duration was the blocks left and shrank as the anchor advanced. That went
// with the arithmetic. NOT MEASURED: a claim and a proof can carry different
// window lengths, and could then still meet across blocks if the real block
// interval matched that difference to the nanosecond.
//
// WHY ADDING IS SAFE, AND SUBTRACTING IS NOT. The ante handler makes three
// checks and an offset that only moves the timestamp LATER can trip none of
// them: it cannot make the deadline look already-passed. Never subtract. The
// ceiling is handled by deriving DefaultTxTimeoutMax from it.
//
// THE SIZE. 10ms is 10^7 slots for a problem that needs ~10^4, and it costs
// 0.1% of the drift budget instead of the 10% a full second cost. It must stay
// well under the minimum block interval so the offset cannot create an overlap
// between adjacent blocks that the constant duration otherwise separates -- three
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

// Timeout regimes, and the label values of the regime counter. The set is
// closed and small on purpose: it is a Prometheus label.
const (
	// TimeoutRegimeWindow -- the window fits under the chain's ceiling, which is
	// every network whose window is shorter than ~590 s of wall time.
	TimeoutRegimeWindow = "window"
	// TimeoutRegimeCeiling -- the window is longer than the chain will accept,
	// so the ceiling decides. Mainnet lives here: 10 blocks x 60 s is 600 s
	// against a ceiling of 589.99 s.
	TimeoutRegimeCeiling = "ceiling"
	// TimeoutRegimeUnknown -- the window could not be measured (non-positive
	// block time or window length). The ceiling is used, because it is the
	// safest value that still lets a transaction land, and timeout_height is
	// what actually bounds it. It should never be seen, which is exactly why it
	// is counted rather than logged.
	TimeoutRegimeUnknown = "unknown"
)

// WindowTimeout is the broadcast deadline for ONE claim or proof window:
// min(window length in blocks x block time, the chain's ceiling).
//
// It replaced a skew-then-clamp pipeline fed by four operator knobs, and the
// reason none of them survived is that the timestamp stopped being a decision:
// now that the transaction carries a timeout_height the chain enforces, the
// timestamp only has to be unique (the unordered nonce) and stay under the SDK's
// ceiling. Neither is something an operator can know better than the code.
//
// It takes the WHOLE window, not the blocks left in it, and that is what makes
// it constant: every attempt inside one window is born with the same number in
// front of it, so a retry at block 8 of 10 is not handed a shrinking budget.
// What kills a late transaction is the height, at the close -- the one place the
// decision belongs.
//
// It is min, NOT max. Taking the larger of the two would hand mainnet 600 s,
// precisely the value the chain refuses with "unordered tx ttl exceeds 10m0s".
//
// The window length is the CALLER'S to supply because it is a chain parameter,
// not a constant: poktroll ships defaults of 3 blocks for the claim window and 4
// for the proof window, while mainnet governs both to 10. Writing 10 here would
// be a number calibrated for one network applied to all of them, wearing the
// word "constant" -- the very defect this change exists to remove.
func WindowTimeout(windowBlocks, blockTimeSeconds int64) (time.Duration, string) {
	// DefaultTxTimeoutMax, not the same arithmetic written out again. The
	// subtraction already exists as a derived constant, and re-deriving it here
	// would create a second copy of a number this repository has already watched
	// drift into four different values across its comments, its getter, its test
	// and production.
	ceiling := DefaultTxTimeoutMax
	if windowBlocks <= 0 || blockTimeSeconds <= 0 {
		return ceiling, TimeoutRegimeUnknown
	}
	window := time.Duration(windowBlocks) * time.Duration(blockTimeSeconds) * time.Second
	if window > ceiling {
		return ceiling, TimeoutRegimeCeiling
	}
	return window, TimeoutRegimeWindow
}

// signAndBroadcastReturningSigned builds, signs, broadcasts, and hands back the
// payload it signed so a caller can cache it for re-injection.
//
// It is the only place the broadcast budget is derived and the deadline
// installed, so both halves of one submission share a single clock. The variant
// that dropped the payload was deleted rather than kept beside it: two entry
// points into the same sequence is how they start disagreeing about how long an
// attempt has.
//
// TWO THINGS HERE ARE CALLED A TIMEOUT AND THEY ARE DIFFERENT CLOCKS.
// timeoutHeight is what the CHAIN enforces against its own block height; the
// deadline installed below is our client-side broadcast budget, a wall-clock
// limit on how long we wait for a reply. Neither bounds the other, and reading
// one for the other is how a transaction ends up with a deadline that outlives
// its own window.
func (tc *TxClient) signAndBroadcastReturningSigned(
	ctx context.Context,
	signerAddr string,
	timeoutHeight int64,
	txType string,
	msgs ...cosmostypes.Msg,
) (string, SignedTxPayload, error) {
	startTime := time.Now()
	defer func() {
		txBroadcastLatency.WithLabelValues(signerAddr).Observe(time.Since(startTime).Seconds())
	}()

	timeoutDuration, timeoutSource, window := tc.effectiveTxTimeout(ctx)
	ctx, cancelDeadline := tc.withBroadcastDeadline(ctx, timeoutDuration, window)
	defer cancelDeadline()

	return tc.signEncodeAndBroadcast(ctx, signerAddr, timeoutHeight, txType, timeoutDuration, timeoutSource, msgs...)
}

// signEncodeAndBroadcast is signAndBroadcast that also hands back WHAT IT
// SIGNED, so a caller can cache it and re-inject the same bytes instead of
// signing a second transaction for the same intent.
//
// The payload is returned even when the broadcast FAILS, and that is the point
// rather than an accident: the failure this cache exists for is the one where
// the node never answered, so the attempt whose outcome is unknown is exactly
// the one whose bytes are worth keeping.
func (tc *TxClient) signEncodeAndBroadcast(
	ctx context.Context,
	signerAddr string,
	timeoutHeight int64,
	txType string,
	timeoutDuration time.Duration,
	timeoutSource string,
	msgs ...cosmostypes.Msg,
) (string, SignedTxPayload, error) {
	st, err := tc.signAndEncode(ctx, signerAddr, timeoutHeight, timeoutDuration, timeoutSource, msgs...)
	if err != nil {
		return "", SignedTxPayload{}, err
	}
	hash, bErr := tc.broadcastRaw(ctx, signerAddr, txType, st)
	return hash, st.payload(), bErr
}

// BroadcastRawReturningHash re-injects bytes that were signed earlier.
//
// It never signs, so the unordered nonce is the ORIGINAL one and the network
// recognises the duplicate on its own -- which is the whole reason to keep the
// bytes. The caller owns the budget: the deadline check lives here only as the
// guard inside broadcastRaw, while deciding whether these bytes are still
// VALID (their own timeout has not passed) belongs to whoever stored them,
// because only that side knows the chain's clock.
func (tc *TxClient) BroadcastRawReturningHash(
	ctx context.Context,
	signerAddr, txType string,
	p SignedTxPayload,
) (string, error) {
	return tc.broadcastRaw(ctx, signerAddr, txType, signedTx{
		bytes:         p.Bytes,
		hash:          p.Hash,
		timeoutHeight: p.TimeoutHeight,
		// The provenance fields stay zero deliberately: this attempt derived
		// nothing, so the accepted-to-mempool log reports no anchor and no
		// regime rather than inventing ones that would describe this pass
		// instead of the signing it is replaying.
	})
}

// SignedTxPayload is what a caller needs to re-inject a transaction later: the
// exact bytes, the hash the chain reports for them, and the two deadlines
// sealed inside them.
//
// Both deadlines travel because neither can be recomputed later and they answer
// different questions. TimeoutAt is the unordered nonce's expiry and decides
// whether these bytes are still usable at all; TimeoutHeight is what the chain
// enforces against its own block height, and it is carried so a re-injection's
// rejection log names the height the transaction ACTUALLY holds rather than one
// the current pass derived.
type SignedTxPayload struct {
	Bytes         []byte
	Hash          string
	TimeoutAt     time.Time
	TimeoutHeight int64
}

// payload converts the internal form into the exported one.
func (st signedTx) payload() SignedTxPayload {
	return SignedTxPayload{
		Bytes:         st.bytes,
		Hash:          st.hash,
		TimeoutAt:     st.timeoutTimestamp,
		TimeoutHeight: st.timeoutHeight,
	}
}

// signedTx is one transaction after signing and encoding, carried from the
// signing half to the broadcasting half.
//
// It exists because those halves stopped being one function: a resend now
// re-injects bytes it signed earlier instead of building a new transaction, so
// broadcasting has to be reachable WITHOUT signing. Everything here except the
// bytes and the hash is provenance for the accepted-to-mempool log, and it is
// carried rather than recomputed on purpose -- for a re-injection those values
// describe the ORIGINAL signing, which is the honest thing for that log to say.
type signedTx struct {
	bytes []byte
	// hash is computed from the bytes on THIS side, so it exists before the
	// transaction is sent -- which is what lets a send that never got an answer
	// still name what it sent. The node reports the same value; broadcastRaw
	// asserts it.
	hash string
	// timeoutHeight is the height SEALED INTO these bytes, not the one the
	// current pass would compute. A re-injection cannot change it without
	// signing again -- that immutability is the whole premise of deciding what
	// to re-inject -- so the caller's freshly derived height would describe a
	// transaction that does not exist. It is carried for the same reason as the
	// four fields below, and it matters more than they do: it is read by the
	// REJECTION log, the one somebody opens after a failure to decide whether
	// the window was closing.
	timeoutHeight    int64
	anchor           time.Time
	timeoutTimestamp time.Time
	timeoutDuration  time.Duration
	timeoutSource    string
	anchorSource     string
}

// signAndEncode builds, signs and encodes the transaction without sending it.
//
// The deadline and the broadcast budget are the CALLER's: both halves of a
// single submission must share one clock, and a re-injection reaching
// broadcastRaw directly brings its own. Passing them in rather than deriving
// them here is what keeps the two entry points from disagreeing about how long
// this attempt has.
func (tc *TxClient) signAndEncode(
	ctx context.Context,
	signerAddr string,
	timeoutHeight int64,
	timeoutDuration time.Duration,
	timeoutSource string,
	msgs ...cosmostypes.Msg,
) (signedTx, error) {

	// Get signing key
	privKey, err := tc.keyManager.GetSigner(signerAddr)
	if err != nil {
		return signedTx{}, fmt.Errorf("failed to get signing key: %w", err)
	}

	// Get account info
	account, err := tc.getAccount(ctx, signerAddr)
	if err != nil {
		return signedTx{}, fmt.Errorf("failed to get account: %w", err)
	}

	// Build the transaction
	txBuilder := tc.txConfig.NewTxBuilder()
	if setMsgsErr := txBuilder.SetMsgs(msgs...); setMsgsErr != nil {
		return signedTx{}, fmt.Errorf("failed to set messages: %w", setMsgsErr)
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
	// transaction this process builds for one supplier and one window against
	// one latest block carries the same (timeout, sender) pair. See
	// txNonceSpread.
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
			return signedTx{}, fmt.Errorf("gas simulation failed (gas_limit=0 requires successful simulation): %w", simErr)
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

	// The chain's own expiry, and it is set HERE -- after the simulation above,
	// not beside SetTimeoutTimestamp where its sibling lives.
	//
	// The tempting placement is next to the timestamp, so that "the simulation
	// reflects the transaction we send". That reason does not survive reading
	// the order: the gas limit and the fee are decided AFTER simulating and the
	// signature comes after that, so the simulated bytes were never the final
	// ones. What placement actually decides is WHICH LAYER refuses an expired
	// window, and the two answers are not equally good.
	//
	// Set before the simulation, the ante handler rejects during Simulate --
	// which runs the ante handler too, and whose decorator ignores its own
	// simulate flag. That reply is flattened to codes.Unknown by the SDK's tx
	// service, arrives with no ABCI code at all, and would displace the failure
	// that the callers already classify today. Set here, Simulate keeps
	// executing the messages and keeps failing inside x/proof, whose registered
	// errors read "claim attempted outside of the session's claim window" and
	// its proof twin (poktroll x/proof/types/errors.go:32-33) -- the substrings
	// the callers already match. The height rejection is then confined to
	// CheckTx, where it carries code 30 and where nothing classified anything
	// before. So this ADDS a covered case instead of replacing a covered one.
	//
	// The cost is that the gas estimate, taken before this field exists, does
	// not account for its few bytes of varint. DefaultGasAdjustment is 1.7 --
	// a 70% margin over the simulated figure, orders of magnitude more than the
	// field can consume. The exact gas delta was not measured.
	//
	// The guard is defensive, not reachable today: all three callers pass a
	// window close they have already compared against the current height. It
	// earns its place by being the last point where the value still has a sign
	// -- past the cast, a negative height is a far-future one.
	if timeoutHeight > 0 {
		txBuilder.SetTimeoutHeight(uint64(timeoutHeight))
	}

	// Sign the transaction (unordered=true means sequence=0)
	err = tc.signTx(ctx, txBuilder, privKey, account, true)
	if err != nil {
		return signedTx{}, fmt.Errorf("failed to sign transaction: %w", err)
	}

	// Encode the transaction
	txBytes, err := tc.txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return signedTx{}, fmt.Errorf("failed to encode transaction: %w", err)
	}

	return signedTx{
		bytes:            txBytes,
		hash:             txHashOf(txBytes),
		timeoutHeight:    timeoutHeight,
		anchor:           anchor,
		timeoutTimestamp: timeoutTimestamp,
		timeoutDuration:  timeoutDuration,
		timeoutSource:    timeoutSource,
		anchorSource:     anchorSource,
	}, nil
}

// broadcastRaw sends bytes that are ALREADY signed and classifies the reply.
//
// It never builds or signs, which is the whole point: a resend that re-injects
// the same bytes keeps the same unordered nonce, so the node recognises the
// duplicate and discards it on its own instead of the network carrying several
// live transactions for one claim.
func (tc *TxClient) broadcastRaw(
	ctx context.Context,
	signerAddr, txType string,
	st signedTx,
) (string, error) {
	// The broadcast budget is the CALLER's, and this refuses to run without one.
	//
	// Every path here is meant to arrive with a deadline already on the context:
	// the original submission derives it from the window, and a re-injection
	// inherits the reconciler's per-group budget. Saying so in a comment is what
	// the previous version did, and a contract in prose is one a caller can
	// forget silently -- a resend with no deadline waits instead of failing
	// fast, which is precisely what the budget exists to prevent, and it fails
	// by being LATE rather than by erroring. So the machine checks.
	if _, ok := ctx.Deadline(); !ok {
		return "", fmt.Errorf("broadcast attempted with no deadline on the context: "+
			"the caller owns the budget (supplier %s, tx_type %s)", signerAddr, txType)
	}

	// Broadcast in SYNC mode (returns after CheckTx, fast)
	// Using unordered eliminates sequence mismatch issues
	// Duplicate protection handled by caller via Redis tracking
	res, err := tc.txClient.BroadcastTx(ctx, &txtypes.BroadcastTxRequest{
		TxBytes: st.bytes,
		Mode:    txtypes.BroadcastMode_BROADCAST_MODE_SYNC,
	})
	if err != nil {
		return "", newBroadcastRejection(err, st.bytes)
	}

	txHash := res.TxResponse.TxHash

	// The hash we computed and the hash the node reports must be the same, and
	// nothing but this line says so.
	//
	// They are the same function of the same bytes, so a difference does not
	// mean "two names for one transaction": it means the bytes that reached the
	// chain are NOT the ones we signed. Everything downstream is keyed by our
	// number -- the stored bytes a resend re-injects, the submission record a
	// claim outcome is matched against, the inclusion query that decides whether
	// a session is paid -- so the failure would be silent and total: we would be
	// asking the chain about a transaction that does not exist while a different
	// one carries our messages.
	//
	// Error rather than Warn, and without a metric on purpose. This cannot
	// happen unless something is broken between our encoder and the node, so it
	// is not a per-request condition that could flood: it is bounded by the
	// defect existing, and if it ever fires it is the first thing an operator
	// must see.
	if st.hash != "" && txHash != "" && st.hash != txHash {
		tc.logger.Error().
			Str("supplier", signerAddr).
			Str("tx_type", txType).
			Str("computed_tx_hash", st.hash).
			Str("reported_tx_hash", txHash).
			Msg("tx hash mismatch: the bytes that reached the chain are not the ones we signed")
	}

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

		// timeout_height is logged for EVERY CheckTx rejection, not just the
		// height one. Code 30 says the NODE considered the window closed, which
		// is not the same statement as "the window closed": the ante handler
		// compares against that node's last committed height, while our number
		// comes from window arithmetic over cached params. Without both, an
		// incident cannot separate "we were late" from "our number was wrong",
		// and by then the transaction is gone. The node's own height travels
		// inside RawLog, which this line already carries, so the field that has
		// to be added is ours.
		tc.logger.Warn().
			Str("supplier", signerAddr).
			Str("tx_type", txType).
			Str("tx_hash", txHash).
			Str("codespace", res.TxResponse.Codespace).
			Uint32("code", res.TxResponse.Code).
			Int64("timeout_height", st.timeoutHeight).
			Str("error", res.TxResponse.RawLog).
			Msg("transaction CheckTx failed")

		rejection := newCheckTxRejection(res.TxResponse)
		if isWindowExpiredRejection(res.TxResponse.Codespace, res.TxResponse.Code) {
			// Both wrap, so the fact stays askable with errors.Is and the
			// rejection stays readable with errors.As -- the same shape
			// ErrTxProofNotRequired uses at its own call site.
			return txHash, fmt.Errorf("%w: %w", ErrTxWindowExpired, rejection)
		}
		if isAlreadyQueuedRejection(res.TxResponse.Codespace, res.TxResponse.Code) {
			// Same shape, opposite meaning: the window one says the work can
			// never land, this one says it is already on its way.
			return txHash, fmt.Errorf("%w: %w", ErrTxAlreadyQueued, rejection)
		}
		return txHash, rejection
	}

	// CheckTx passed! TX accepted to mempool
	tc.logger.Info().
		Str("supplier", signerAddr).
		Str("tx_type", txType).
		Str("tx_hash", txHash).
		Str("timeout_source", st.timeoutSource).
		Str("anchor_source", st.anchorSource).
		Time("anchor", st.anchor).
		Dur("timeout_duration", st.timeoutDuration).
		Time("timeout_timestamp", st.timeoutTimestamp).
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
		return 0, newSimulateRejection(err)
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

// ErrTxProofNotRequired reports that the CHAIN refused a proof because none was
// required -- the claim did not meet the probabilistic threshold, or it had
// already settled without one.
//
// It exists so a caller can ask the FACT with errors.Is and read the DATUM with
// errors.As, instead of matching substrings on a message that grows a wrapper at
// every frame. The two are different questions: "was it this condition" and
// "which message of the batch, and what did the server actually say".
var ErrTxProofNotRequired = errors.New("chain reports no proof was required")

// ErrTxWindowExpired reports that the CHAIN refused the transaction because its
// timeout height had already passed -- the claim or proof window closed before
// the node saw it. It is terminal: the same bytes can never be accepted later,
// so a caller that retries is spending attempts and fees on nothing.
//
// It is recognised by CODE, not by text, and that is not a departure from the
// rule stated on TxRejection ("classify those by text"). That rule is about
// x/proof errors, where codes.FailedPrecondition alone covers the window check,
// a missing claim, a malformed address, two fee failures and "proof not
// required" -- six conditions behind one code, which therefore decides nothing.
// This rejection comes from a different layer: the SDK's own ante handler, where
// code 30 in codespace "sdk" means this and only this. One rule -- classify by
// whatever discriminates -- landing differently in two layers.
//
// The pair is what discriminates, not the number: codes are per-codespace, so a
// module of its own may well register a 30 that means something unrelated.
var ErrTxWindowExpired = errors.New("chain rejected transaction: timeout height already passed")

// ErrTxAlreadyQueued reports that the chain refused the transaction because it
// is ALREADY IN THE NODE'S MEMPOOL. It is not a failure: the work is done and
// the only correct response is to stop and be satisfied.
//
// It exists for the resend path. Re-sending the same transaction to the same
// node while it still sits in that node's mempool answers code 19 without
// transmitting anything, which is the cheapest possible outcome -- and, once
// resends happen on every block, the MOST FREQUENT one. Read as a generic
// rejection it would be the opposite: an attempt consumed, an entry in the error
// bucket, and a Warn per session per block, turning the operational signal into
// noise exactly when the resend loop is working as designed.
//
// Its guarantee is weaker than ErrTxWindowExpired's and the difference matters:
// the mempool cache is a bounded LRU, LOCAL to one node, that evicts by age
// without regard to validity. So "already queued" means "this node has it now",
// not "the chain will include it" -- which is why seeing this must not settle a
// session, only decline to count the attempt against it.
var ErrTxAlreadyQueued = errors.New("chain reports the transaction is already in the mempool")

// abciCodeTxInMempoolCache is cosmos-sdk's ErrTxInMempoolCache
// (types/errors/errors.go:66 in v0.53.7). It arrives through the same CheckTx
// path as any other rejection: client/broadcast.go turns CometBFT's own
// duplicate error into a synthetic TxResponse carrying this code and codespace.
const abciCodeTxInMempoolCache = 19

// isAlreadyQueuedRejection reports whether a CheckTx response is the node saying
// it already holds this transaction.
func isAlreadyQueuedRejection(codespace string, code uint32) bool {
	return code == abciCodeTxInMempoolCache && codespace == abciCodespaceSDK
}

// abciCodeTxTimeoutHeight is cosmos-sdk's ErrTxTimeoutHeight, registered in the
// root codespace (types/errors/errors.go:100 in v0.53.7). Its sibling in the
// SAME decorator is code 42, ErrTxTimeout, which is the timeout TIMESTAMP
// expiring -- a different clock and a different remedy, which is why the two
// must not be collapsed into "the deadline passed".
const (
	abciCodeTxTimeoutHeight = 30
	abciCodespaceSDK        = "sdk"
)

// isWindowExpiredRejection reports whether a CheckTx response is the chain
// refusing a transaction whose timeout height had passed.
func isWindowExpiredRejection(codespace string, code uint32) bool {
	return code == abciCodeTxTimeoutHeight && codespace == abciCodespaceSDK
}

// abciCodeMempoolIsFull is cosmos-sdk's ErrMempoolIsFull
// (types/errors/errors.go:69 in v0.53.7). It is the OPPOSITE of code 19 and the
// pair is easy to collapse: 19 says the node already holds this transaction,
// 20 says it could not take it at all. Both leave the bytes usable, for
// different reasons -- one because it is already in flight, the other because
// it was refused for space rather than for validity -- so a later attempt on a
// node with room is worth making with the SAME transaction.
const abciCodeMempoolIsFull = 20

// RejectionPreservesBytes reports whether a failed send leaves the signed
// transaction still worth re-injecting.
//
// The default is NO, and that asymmetry is deliberate: preserving bytes the
// chain will refuse again re-sends them on every block until the window closes,
// while discarding usable ones costs a single signature. An unbounded repeat
// against one extra signature is not a close call.
//
// It answers YES in three cases, all of them "the transaction was not judged
// invalid":
//
//   - TRANSPORT: the send never got an answer, so nobody knows whether it
//     arrived. Signing a replacement here is how one claim ends up with two
//     live transactions, which is the failure re-injection exists to avoid.
//   - CODE 19: the node already holds these exact bytes.
//   - CODE 20: the mempool was full. The transaction was not read, let alone
//     refused.
//
// WHAT IT DELIBERATELY DOES NOT DECIDE: code 18. It is ErrInvalidRequest,
// GENERIC, and one of the things it can mean is that our OWN earlier
// transaction already took this unordered nonce -- in which case these bytes
// are the ones in flight and re-signing duplicates them. Telling that apart
// from every other invalid request needs a discriminator this package does not
// have, so 18 falls to the default and is discarded. That is the safe direction
// and it is NOT the whole answer; the case is named here so the next reader
// finds a decision rather than an omission.
func RejectionPreservesBytes(err error) bool {
	if err == nil {
		return true
	}
	var rejection *TxRejection
	if !errors.As(err, &rejection) {
		// Not a chain rejection at all -- a local failure, a cancelled context.
		// Nothing judged the transaction, so its bytes are untouched.
		return true
	}
	if rejection.Stage == TxStageBroadcast {
		return true
	}
	// The codes below only mean what they say when they come from CheckTx. The
	// STAGE is part of the identity, not decoration: simulation runs the
	// messages, so a failure there is a judgement about the transaction no
	// matter which number it carries. Reading the code without the stage let a
	// simulate-stage 20 preserve bytes the chain had already refused.
	if rejection.Stage != TxStageCheckTx || rejection.Codespace != abciCodespaceSDK {
		return false
	}
	return rejection.ABCICode == abciCodeTxInMempoolCache ||
		rejection.ABCICode == abciCodeMempoolIsFull
}

// isProofNotRequiredError recognises that refusal.
//
// The needle is DERIVED FROM THE SYMBOL: prooftypes.ErrProofNotRequired.Error()
// is its registered description verbatim (cosmossdk.io/errors renders a
// registered error as its desc), so deleting or renaming the symbol upstream
// breaks this build instead of silently un-matching. That is the only defence
// available, because every exit of poktroll's proof msg server flattens its
// error to text -- the registered object, its code and its codespace are all
// destroyed before it reaches us.
//
// It matches on RawLog, the server's own text, and NEVER on err.Error(): ours
// keeps growing wrappers, and Contains over a moving string is how a classifier
// starts matching something it was never meant to. No case folding, and that is
// safe only because BOTH sides come from the same symbol -- the description we
// look for and the description the server echoed. Replace the needle with a
// hand-typed literal and case starts mattering again with nothing to catch it.
//
// This is also NARROWER than the text match it replaces: it requires a
// TxRejection in the chain, so it fires on an actual chain refusal rather than
// on anything that happens to carry the phrase.
func isProofNotRequiredError(err error) bool {
	var rejection *TxRejection
	if !errors.As(err, &rejection) {
		return false
	}
	return strings.Contains(rejection.RawLog, prooftypes.ErrProofNotRequired.Error())
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

// Close closes the transaction client.
// If the client was created with a shared gRPC connection, it will not be closed.
func (tc *TxClient) Close() error {
	if !tc.closed.CompareAndSwap(false, true) {
		return nil
	}
	cancelProbe, probeDone := tc.probeCancel, tc.probeDone

	// Stop the probe and quiesce the broadcasts. Both must finish before the
	// connection goes: closing it underneath either is the failure this client
	// is being taught to survive.
	if cancelProbe != nil {
		cancelProbe()
	}
	if probeDone != nil {
		<-probeDone
	}

	// Taking every permit IS waiting for the in-flight broadcasts -- one
	// mechanism, two invariants. Marking closed first is what lets a caller
	// already queued behind these permits wake up, see the flag and hand its
	// permit back instead of broadcasting into a connection about to shut.
	//
	// context.Background() and not a cancellable context, deliberately: this
	// call is the only thing standing between a live broadcast and a closed
	// connection, and Acquire -- unlike the WaitGroup it replaces -- HAS an
	// error path. A context that can be cancelled would return early and fall
	// through to grpcConn.Close() with broadcasts still running, which is the
	// exact failure the wait exists to prevent. If anyone ever gives this a
	// context, the error path must NOT close the connection.
	// This error path returns BEFORE closing anything, which with a pool would
	// strand every member rather than one connection. It is left exactly as it
	// is, and the reason is that with context.Background() it cannot be
	// reached: Background's Done() is nil (measured), and all three branches of
	// semaphore.Acquire that return an error do it by receiving from that
	// channel. The one branch that could fire here -- n greater than the
	// semaphore's size -- receives from nil and BLOCKS FOREVER instead.
	//
	// So the failure this guards is not a leak, it is a silent hang at
	// shutdown, and what prevents it is that the count asked for here is
	// exactly the semaphore's size, which holds because both are
	// tc.maxConcurrent(). That invariant is now load bearing: anything that
	// makes Close ask for a different number hangs, and anything that hands
	// this a cancellable context makes the error path reachable again, at
	// which point the comment above applies and it must still NOT close.
	if err := tc.permits.Acquire(context.Background(), tc.maxConcurrent()); err != nil {
		return fmt.Errorf("failed to quiesce in-flight broadcasts: %w", err)
	}

	// What this does NOT cover, said rather than hidden: the fee query
	// (queryLastTxFeeUpokt, called from HASupplierClient) reads on this
	// connection without a permit, so it can race a Close and fail. A fee
	// lookup erroring during shutdown is acceptable; a claim erroring is not,
	// and that is the line the permit draws.

	// Only close connections we created ourselves. Closing the members in
	// series is deliberate: 626 never-dialled connections close in 3.5ms
	// (measured), and the waits inside ClientConn.Close are on its own
	// serializer goroutines rather than on anything the peer has to answer.
	// A connected member does more local work than that -- NOT measured -- but
	// nothing here waits on a network round trip, so a parallel shutdown would
	// buy nothing an orchestrator could notice.
	if tc.ownsConn && tc.pool != nil {
		if err := tc.pool.Close(); err != nil {
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
	// lastClaimSigned stores the payload that produced that hash, stashed in
	// the SAME critical section so the two can never describe different
	// transactions. The original submission is where re-injection has to start:
	// the failure it exists for is the send whose answer never arrived, and by
	// then there is nothing left to sign from.
	lastClaimSigned SignedTxPayload
	lastClaimTxMu   sync.RWMutex

	// lastProofTxHash stores the TX hash of the last proof submission (for deduplication)
	lastProofTxHash string
	// lastProofSigned is the claim field's twin, and stashed the same way.
	lastProofSigned SignedTxPayload
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
	_, _, err := c.CreateClaimsReturningHash(ctx, timeoutHeight, claimMsgs...)
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
) (string, SignedTxPayload, error) {
	// DEBUG/TEST: Force claim TX error to test claim_tx_error state transition
	// Set environment variable TEST_FORCE_CLAIM_TX_ERROR=true to enable
	if testCfg := getTestConfig(); testCfg.ForceClaimTxError {
		c.logger.Warn().
			Msg("TEST MODE: TEST_FORCE_CLAIM_TX_ERROR detected - forcing claim TX error")
		return "", SignedTxPayload{}, fmt.Errorf("TEST MODE: simulated claim transaction error")
	}

	claims := make([]*prooftypes.MsgCreateClaim, len(claimMsgs))
	for i, msg := range claimMsgs {
		claim, ok := msg.(*prooftypes.MsgCreateClaim)
		if !ok {
			return "", SignedTxPayload{}, fmt.Errorf("invalid claim message type: %T", msg)
		}
		claims[i] = claim
	}

	// Call TxClient and capture TX hash for deduplication
	txHash, signed, err := c.txClient.CreateClaims(ctx, c.operatorAddr, timeoutHeight, claims)
	if err != nil {
		// Stash the PAYLOAD but not a hash. A send that got no answer is
		// precisely the one whose bytes a resend must re-inject, and this is
		// the only place they still exist -- while the hash stays empty because
		// nothing confirmed it. The empty hash does not change WHEN the
		// reconciler resends: every stored entry goes from the block after its
		// submit (canRebroadcast).
		c.lastClaimTxMu.Lock()
		c.lastClaimSigned = signed
		c.lastClaimTxMu.Unlock()
		return "", signed, err
	}

	// Store TX hash for retrieval by caller (1 line after broadcast)
	c.lastClaimTxMu.Lock()
	c.lastClaimTxHash = txHash
	c.lastClaimSigned = signed
	c.lastClaimTxMu.Unlock()

	// The fee we just paid is now the freshest observation available.
	// Drop any cached chain-observed estimate so the next economic-viability
	// check reflects what the network is actually charging rather than a
	// possibly-stale spike.
	c.InvalidateFeeCache()

	return txHash, signed, nil
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
	_, _, err := c.SubmitProofsReturningHash(ctx, timeoutHeight, proofMsgs...)
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
) (string, SignedTxPayload, error) {
	// DEBUG/TEST: Force proof TX error to test proof_tx_error state transition
	// Set environment variable TEST_FORCE_PROOF_TX_ERROR=true to enable
	if testCfg := getTestConfig(); testCfg.ForceProofTxError {
		c.logger.Warn().
			Msg("TEST MODE: TEST_FORCE_PROOF_TX_ERROR detected - forcing proof TX error")
		return "", SignedTxPayload{}, fmt.Errorf("TEST MODE: simulated proof transaction error")
	}

	proofs := make([]*prooftypes.MsgSubmitProof, len(proofMsgs))
	for i, msg := range proofMsgs {
		proof, ok := msg.(*prooftypes.MsgSubmitProof)
		if !ok {
			return "", SignedTxPayload{}, fmt.Errorf("invalid proof message type: %T", msg)
		}
		proofs[i] = proof
	}

	// Call TxClient and capture TX hash for deduplication
	txHash, signed, err := c.txClient.SubmitProofs(ctx, c.operatorAddr, timeoutHeight, proofs)
	if err != nil {
		// Same as the claim path: keep the bytes, leave the hash empty.
		c.lastProofTxMu.Lock()
		c.lastProofSigned = signed
		c.lastProofTxMu.Unlock()
		return "", signed, err
	}

	// Store TX hash for retrieval by caller (1 line after broadcast)
	c.lastProofTxMu.Lock()
	c.lastProofTxHash = txHash
	c.lastProofSigned = signed
	c.lastProofTxMu.Unlock()

	// Same rationale as CreateClaims — refresh the cached estimate from the
	// most recent successful submission.
	c.InvalidateFeeCache()

	return txHash, signed, nil
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

// GetLastClaimSignedTx returns the payload of the last claim submission,
// alongside GetLastClaimTxHash and read under the same lock, so a caller that
// takes both gets one transaction rather than two halves of different ones.
func (c *HASupplierClient) GetLastClaimSignedTx() SignedTxPayload {
	if c == nil {
		return SignedTxPayload{}
	}
	c.lastClaimTxMu.RLock()
	defer c.lastClaimTxMu.RUnlock()
	return c.lastClaimSigned
}

// GetLastProofSignedTx is the proof twin of GetLastClaimSignedTx.
func (c *HASupplierClient) GetLastProofSignedTx() SignedTxPayload {
	if c == nil {
		return SignedTxPayload{}
	}
	c.lastProofTxMu.RLock()
	defer c.lastProofTxMu.RUnlock()
	return c.lastProofSigned
}

// LatestBlockTime exposes the chain clock this client anchors its deadlines to.
//
// It is the SAME clock that produced the timeout_timestamp inside a signed
// transaction, which is what makes it the right one to ask whether those bytes
// have expired: comparing against the wall clock would disagree with the ante
// handler exactly when the chain runs behind, and that is when a re-injection
// most needs the answer. Zero means unknown, and every caller must read that as
// "do not re-inject" rather than as "not expired".
func (c *HASupplierClient) LatestBlockTime() time.Time {
	if c == nil || c.txClient == nil || c.txClient.config.BlockTimeProvider == nil {
		return time.Time{}
	}
	return c.txClient.config.BlockTimeProvider.LatestBlockTime()
}

// BroadcastRawReturningHash re-injects previously signed bytes for this
// supplier, without building or signing anything.
func (c *HASupplierClient) BroadcastRawReturningHash(ctx context.Context, txType string, p SignedTxPayload) (string, error) {
	if c == nil || c.txClient == nil {
		return "", fmt.Errorf("no tx client")
	}
	return c.txClient.BroadcastRawReturningHash(ctx, c.operatorAddr, txType, p)
}

// GetLastProofTxHash returns the TX hash of the last proof submission.
// This is used for deduplication tracking in Redis.
func (c *HASupplierClient) GetLastProofTxHash() string {
	c.lastProofTxMu.RLock()
	defer c.lastProofTxMu.RUnlock()
	return c.lastProofTxHash
}
