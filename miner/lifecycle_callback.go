package miner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alitto/pond/v2"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/pokt-network/smt"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/tx"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// TestConfig holds test mode flags read once at initialization.
// These environment variables are only for testing and should not be set in production.
type TestConfig struct {
	// ClaimDelaySeconds delays claim submission by N seconds (for testing claim window timeout)
	ClaimDelaySeconds int

	// ProofDelaySeconds delays proof submission by N seconds (for testing proof window timeout)
	ProofDelaySeconds int
}

var (
	cachedTestConfig     TestConfig
	cachedTestConfigOnce sync.Once
)

// getTestConfig returns the test configuration, reading environment variables once.
func getTestConfig() TestConfig {
	cachedTestConfigOnce.Do(func() {
		if delayStr := os.Getenv("TEST_CLAIM_DELAY_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				cachedTestConfig.ClaimDelaySeconds = delay
			}
		}

		// Support both TEST_DELAY_PROOF_SECONDS and TEST_PROOF_DELAY_SECONDS for backward compatibility
		if delayStr := os.Getenv("TEST_DELAY_PROOF_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				cachedTestConfig.ProofDelaySeconds = delay
			}
		}
		if delayStr := os.Getenv("TEST_PROOF_DELAY_SECONDS"); delayStr != "" {
			if delay, err := strconv.Atoi(delayStr); err == nil && delay > 0 {
				cachedTestConfig.ProofDelaySeconds = delay
			}
		}
	})
	return cachedTestConfig
}

// LifecycleCallbackConfig contains configuration for the lifecycle callback.
type LifecycleCallbackConfig struct {
	// SupplierAddress is the supplier this callback is for.
	SupplierAddress string

	// ClaimRetryAttempts is the number of times to retry failed claims.
	ClaimRetryAttempts int

	// ClaimRetryDelay is the delay between retry attempts.
	ClaimRetryDelay time.Duration

	// ProofRetryAttempts is the number of times to retry failed proofs.
	ProofRetryAttempts int

	// ProofRetryDelay is the delay between retry attempts.
	ProofRetryDelay time.Duration

	// BlockTimeSeconds is the expected block time used to convert remaining window
	// blocks into a TX broadcast deadline. The TxClient enforces hard min/max bounds
	// regardless of this value. Default: 30.
	BlockTimeSeconds int64

	// DisablePreProofClaimVerification turns off the pre-proof GetClaim guard.
	// When the guard is on (the default — zero value is false), the miner
	// queries the chain for each session's claim before proof submission and
	// drops sessions whose claim is missing on-chain, transitioning them to
	// SessionStateClaimMissing. This prevents the "no claim found for session
	// ID ..." FailedPrecondition retry storm and the wasted gas that follows
	// when a claim tx was accepted to mempool but never included in a block.
	//
	// Leave this false (guard enabled) in production.
	DisablePreProofClaimVerification bool
}

// DefaultLifecycleCallbackConfig returns sensible defaults.
func DefaultLifecycleCallbackConfig() LifecycleCallbackConfig {
	return LifecycleCallbackConfig{
		ClaimRetryAttempts: 3,
		ClaimRetryDelay:    2 * time.Second,
		ProofRetryAttempts: 3,
		ProofRetryDelay:    2 * time.Second,
	}
}

// SMSTManager provides SMST operations for claim/proof generation.
// This interface combines what the lifecycle callback needs from both
// SMSTFlusher and SMSTProver.
type SMSTManager interface {
	// FlushTree flushes the SMST for a session and returns the root hash.
	FlushTree(ctx context.Context, sessionID string) (rootHash []byte, err error)

	// GetTreeRoot returns the root hash for an already-flushed session.
	GetTreeRoot(ctx context.Context, sessionID string) (rootHash []byte, err error)

	// ProveClosest generates a proof for the closest leaf to the given path.
	ProveClosest(ctx context.Context, sessionID string, path []byte) (proofBytes []byte, err error)

	// DeleteTree removes the SMST for a session (cleanup after settlement).
	DeleteTree(ctx context.Context, sessionID string) error
}

// SessionQueryClient queries session information from the blockchain.
type SessionQueryClient interface {
	GetSession(ctx context.Context, appAddr, serviceID string, blockHeight int64) (*sessiontypes.Session, error)
}

// LifecycleCallback implements SessionLifecycleCallback to handle claim and proof submission.
// It coordinates SMST operations with transaction submission and uses proper timing spread.
type LifecycleCallback struct {
	logger             logging.Logger
	config             LifecycleCallbackConfig
	supplierClient     LifecycleTxClient
	sharedClient       pocktclient.SharedQueryClient
	blockClient        pocktclient.BlockClient
	sessionClient      SessionQueryClient
	smstManager        SMSTManager
	sessionCoordinator *SessionCoordinator

	// proofChecker determines if a proof is required for a claimed session.
	// If nil, proofs are always submitted (legacy behavior).
	proofChecker *ProofRequirementChecker

	// serviceClient queries the current service CUPR for the claim-build
	// CUPR-mismatch guard. If nil, the guard is skipped.
	serviceClient pocktclient.ServiceQueryClient

	// submissionTracker tracks claim/proof submissions to Redis for debugging.
	// If nil, submissions are not tracked.
	submissionTracker *SubmissionTracker

	// deduplicator is used to clean up session deduplication entries after settlement.
	// If nil, local cache cleanup is skipped (Redis entries still expire via TTL).
	deduplicator Deduplicator

	// proofQueryClient is used by the pre-proof GetClaim guard to verify each
	// session's claim exists on-chain before proof submission. If nil, the
	// guard is skipped (legacy behavior).
	proofQueryClient pocktclient.ProofQueryClient

	// rebroadcastStore persists each built MsgCreateClaim / MsgSubmitProof at
	// submit so the process-wide InclusionReconciler can verify on-chain
	// inclusion per block and re-broadcast a still-missing claim/proof while its
	// window is open (HA-safe: survives leader failover). If nil, the built
	// messages are not persisted and the reconciler has nothing to verify —
	// fire-once-at-window-open behavior (silent CLAIM_MISSING/PROOF_MISSING).
	rebroadcastStore RebroadcastStorage

	// buildPool is used for bounded parallel claim/proof building.
	// If nil, falls back to unbounded goroutines (legacy behavior).
	buildPool pond.Pool
}

// NewLifecycleCallback creates a new lifecycle callback.
// The proofChecker parameter is optional - if nil, proofs are always submitted (legacy behavior).
func NewLifecycleCallback(
	logger logging.Logger,
	supplierClient LifecycleTxClient,
	sharedClient pocktclient.SharedQueryClient,
	blockClient pocktclient.BlockClient,
	sessionClient SessionQueryClient,
	smstManager SMSTManager,
	sessionCoordinator *SessionCoordinator,
	proofChecker *ProofRequirementChecker,
	config LifecycleCallbackConfig,
) *LifecycleCallback {
	if config.ClaimRetryAttempts <= 0 {
		config.ClaimRetryAttempts = 3
	}
	if config.ClaimRetryDelay <= 0 {
		config.ClaimRetryDelay = 2 * time.Second
	}
	if config.ProofRetryAttempts <= 0 {
		config.ProofRetryAttempts = 3
	}
	if config.ProofRetryDelay <= 0 {
		config.ProofRetryDelay = 2 * time.Second
	}

	return &LifecycleCallback{
		logger:             logging.ForSupplierComponent(logger, logging.ComponentLifecycleCallback, config.SupplierAddress),
		config:             config,
		supplierClient:     supplierClient,
		sharedClient:       sharedClient,
		blockClient:        blockClient,
		sessionClient:      sessionClient,
		smstManager:        smstManager,
		sessionCoordinator: sessionCoordinator,
		proofChecker:       proofChecker,
	}
}

// SetServiceClient sets the service query client used by the claim-build
// CUPR-mismatch guard. Optional — if not set, the guard is skipped.
func (lc *LifecycleCallback) SetServiceClient(client pocktclient.ServiceQueryClient) {
	lc.serviceClient = client
}

// SetSubmissionTracker sets the submission tracker for debugging claim/proof submissions.
// This is optional - if not set, submissions are not tracked.
func (lc *LifecycleCallback) SetSubmissionTracker(tracker *SubmissionTracker) {
	lc.submissionTracker = tracker
}

// SetBuildPool sets the worker pool for bounded parallel claim/proof building.
// If not set, falls back to unbounded goroutines (legacy behavior).
func (lc *LifecycleCallback) SetBuildPool(pool pond.Pool) {
	lc.buildPool = pool
}

// SetDeduplicator sets the deduplicator for cleaning up session deduplication entries.
// If not set, local cache cleanup is skipped (Redis entries still expire via TTL).
func (lc *LifecycleCallback) SetDeduplicator(dedup Deduplicator) {
	lc.deduplicator = dedup
}

// SetProofQueryClient wires the on-chain proof query client used by the
// pre-proof GetClaim guard. If not set (or if the config flag
// EnablePreProofClaimVerification is false), the guard is skipped and the
// proof pipeline behaves as before the WS-A fix.
func (lc *LifecycleCallback) SetProofQueryClient(client pocktclient.ProofQueryClient) {
	lc.proofQueryClient = client
}

// SetRebroadcastStore wires the store that persists built claim/proof messages
// for the InclusionReconciler. Optional — without it, messages are not persisted
// and the reconciler has nothing to verify/rebroadcast (fire-once behavior).
// SetRebroadcastStore installs the persistence. It takes the INTERFACE, so a
// different backing is a wiring change.
//
// Pass a genuine nil to disable it, never a nil *RebroadcastStore: a typed nil
// pointer assigned into an interface produces a value that is NOT nil, so the
// six `rebroadcastStore != nil` guards downstream would all pass and then call
// through a nil receiver. Today's only caller hands over a store built by
// NewRebroadcastStore, so the trap is not reachable -- it is named because
// switching this parameter from a pointer to an interface is what created it.
func (lc *LifecycleCallback) SetRebroadcastStore(store RebroadcastStorage) {
	lc.rebroadcastStore = store
}

// LifecycleTxClient is what the claim and proof cycles call on the chain.
//
// Every submission returns its OWN hash and signed payload. The client is
// shared with the inclusion reconciler, which submits through it from another
// goroutine woken by the same block event, so "the last submission" is not a
// question this client can answer for a caller: a value read back from the
// client after the call may belong to a submission of another session.
//
// The fee estimate is here rather than behind a type assertion so that a client
// without it cannot compile, instead of silently disabling the economic
// viability floor.
type LifecycleTxClient interface {
	CreateClaimsReturningHash(ctx context.Context, timeoutHeight int64, claimMsgs ...pocktclient.MsgCreateClaim) (string, tx.SignedTxPayload, error)
	SubmitProofsReturningHash(ctx context.Context, timeoutHeight int64, proofMsgs ...pocktclient.MsgSubmitProof) (string, tx.SignedTxPayload, error)
	GetEstimatedFeeUpokt(ctx context.Context) uint64
	// BroadcastRawReturningHash and LatestBlockTime are what a RETRY needs to
	// re-inject the bytes it already has instead of signing a second live
	// transaction. They are on the interface rather than behind a type
	// assertion so a client that cannot re-inject cannot compile.
	BroadcastRawReturningHash(ctx context.Context, txType string, p tx.SignedTxPayload) (string, error)
	LatestBlockTime() time.Time
}

var _ LifecycleTxClient = (*tx.HASupplierClient)(nil)

// resendOrSignClaims sends one claim batch: re-injecting the bytes of the
// previous attempt when they are still worth sending, and signing a new
// transaction otherwise.
//
// `previous` is empty on the first attempt and after an ejection, so both sign
// by construction. `previousErr` is how the attempt that produced those bytes
// ended: bytes the chain JUDGED are not worth re-injecting, which is the same
// rule the reconciler applies to its own resends and the one persistence
// applies to what it stores.
func (lc *LifecycleCallback) resendOrSignClaims(
	ctx context.Context,
	timeoutHeight int64,
	previous tx.SignedTxPayload,
	previousErr error,
	msgs []pocktclient.MsgCreateClaim,
) (string, tx.SignedTxPayload, error) {
	if lc.canReinject(previous, previousErr) {
		hash, err := lc.supplierClient.BroadcastRawReturningHash(ctx, string(RebroadcastPhaseClaim), previous)
		// The payload goes back unchanged: nothing was signed, so the caller
		// keeps exactly the bytes it already had.
		return hash, previous, err
	}
	return lc.supplierClient.CreateClaimsReturningHash(ctx, timeoutHeight, msgs...)
}

// resendOrSignProofs is the proof twin of resendOrSignClaims.
func (lc *LifecycleCallback) resendOrSignProofs(
	ctx context.Context,
	timeoutHeight int64,
	previous tx.SignedTxPayload,
	previousErr error,
	msgs []pocktclient.MsgSubmitProof,
) (string, tx.SignedTxPayload, error) {
	if lc.canReinject(previous, previousErr) {
		hash, err := lc.supplierClient.BroadcastRawReturningHash(ctx, string(RebroadcastPhaseProof), previous)
		return hash, previous, err
	}
	return lc.supplierClient.SubmitProofsReturningHash(ctx, timeoutHeight, msgs...)
}

// canReinject answers whether the bytes of the previous attempt may be sent
// again as they are.
//
// Three conditions, and each rules out a different way of being wrong: there
// have to BE bytes (the first attempt has none); the chain must not have judged
// them (a refusal re-injected asks the same question and gets the same answer);
// and they must still be valid against the CHAIN's clock, which is what
// reusable() reads -- an unknown clock answers no, because re-injecting on a
// guess spends the attempt on bytes the ante handler may already refuse.
func (lc *LifecycleCallback) canReinject(previous tx.SignedTxPayload, previousErr error) bool {
	if len(previous.Bytes) == 0 || previousErr == nil {
		return false
	}
	if !tx.RejectionPreservesBytes(previousErr) {
		return false
	}
	return reusable(previous, lc.supplierClient.LatestBlockTime())
}

// signedTimeoutNanos converts the sealed deadline for storage, keeping ZERO as
// "not known" rather than as the Unix epoch: an entry with no cached
// transaction and one whose deadline happens to be time.Time{} must both read
// back as absent, or a resend would compare against 1970 and re-inject bytes
// the chain refused long ago.
func signedTimeoutNanos(p tx.SignedTxPayload) int64 {
	if p.TimeoutAt.IsZero() {
		return 0
	}
	return p.TimeoutAt.UnixNano()
}

// isClaimNotFoundError returns true only when the chain has definitively answered
// that no claim exists for this (supplier, session).
//
// The decision this feeds is terminal and costs money — see the pre-proof guard in
// OnClaimedSessionsResumed queues again the cold compaction of the sessions the
// lifecycle loaded with their claim already sent (see resumeColdCompactions).
//
// It does not help a Redis that is already full: the leaves blob is written
// before the nodes hash is deleted, on purpose, so with maxmemory reached every
// attempt is refused and the retries run out. This keeps the next restart from
// leaving those trees behind; it does not recover the one that already did.
func (lc *LifecycleCallback) OnClaimedSessionsResumed(ctx context.Context, sessions []*SessionSnapshot) {
	compactor, ok := lc.smstManager.(interface {
		ScheduleColdCompaction(ctx context.Context, sessionID string)
	})
	if !ok {
		return
	}
	for _, snapshot := range sessions {
		compactor.ScheduleColdCompaction(ctx, snapshot.SessionID)
	}
	lc.logger.Info().
		Str(logging.FieldSupplier, lc.config.SupplierAddress).
		Int("sessions", len(sessions)).
		Msg("queued again the cold compaction of the sessions claimed before this miner started")
}

// OnSessionsNeedProof — so it follows query.IsEntityNotFound's policy: an explicit
// gRPC NotFound and nothing else. Any other error means we failed to get an answer,
// and the caller must fail OPEN rather than skip a proof we cannot prove is
// unnecessary. This mirrors the claim-side CUPR guard, which already fails open on
// query errors so it never drops a claim it cannot prove is doomed.
func isClaimNotFoundError(err error) bool {
	return query.IsEntityNotFound(err)
}

// persistRebroadcastEntries stores one rebroadcast entry per built message so
// the InclusionReconciler can later verify inclusion and re-broadcast. snapshots
// and marshalAt(i) are indexed identically (the built-only ordered set). A
// failure to persist one session is logged, not fatal — the worst case is that
// session falls back to fire-once behavior, identical to pre-reconciler.
func (lc *LifecycleCallback) persistRebroadcastEntries(
	ctx context.Context,
	phase RebroadcastPhase,
	snapshots []*SessionSnapshot,
	submitHeight int64,
	txHash string,
	// signed is the transaction the messages went out in. It is stored ON each
	// entry so a resend re-injects it rather than signing a new one; empty means
	// the resend signs, which is what it did before this existed.
	signed tx.SignedTxPayload,
	// attemptErr is how the send that produced `signed` ended, and nil means it
	// was accepted. See persistRebroadcastEntry: it decides whether those bytes
	// are worth re-injecting at all.
	attemptErr error,
	// budget and regime are the broadcast budget the submission was born with
	// (tx.WindowTimeout). A resend that signs a new transaction spends them
	// rather than a budget of its own.
	budget time.Duration,
	regime string,
	marshalAt func(i int) ([]byte, error),
) {
	for i, snapshot := range snapshots {
		msgBytes, mErr := marshalAt(i)
		if mErr != nil {
			lc.logger.Warn().Err(mErr).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Str("phase", string(phase)).
				Msg("failed to marshal message for rebroadcast persistence")
			continue
		}
		if pErr := lc.persistRebroadcastEntry(ctx, phase, snapshot, submitHeight, txHash, signed, attemptErr, budget, regime, msgBytes); pErr != nil {
			lc.logger.Warn().Err(pErr).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Str("phase", string(phase)).
				Msg("failed to persist rebroadcast entry")
		}
	}
}

// persistRebroadcastEntry stores ONE session's message and REPORTS whether it
// landed. It is the single-session half of persistRebroadcastEntries, split out
// rather than inlined because exactly one caller needs the answer: on the
// ejection path the message being stored never travelled, so a failure here is
// not "this session loses its retry", it is "this session loses its claim".
// Every other caller stores a message that was already broadcast, and for those
// a failure really does degrade to the pre-reconciler fire-once behaviour.
func (lc *LifecycleCallback) persistRebroadcastEntry(
	ctx context.Context,
	phase RebroadcastPhase,
	snapshot *SessionSnapshot,
	submitHeight int64,
	txHash string,
	signed tx.SignedTxPayload,
	attemptErr error,
	budget time.Duration,
	regime string,
	msgBytes []byte,
) error {
	// STORE THE BYTES ONLY IF THE CHAIN HAS NOT ALREADY JUDGED THEM.
	//
	// A failed send hands its payload back whatever went wrong, so "bytes
	// present" does NOT mean "nobody answered": a CheckTx refusal returns them
	// too. Re-injecting bytes the chain refused asks the same question again
	// and gets the same answer, which costs blocks of a window that is about
	// ten long -- and if the node keeps invalid transactions in its cache, the
	// resend is answered "I already hold this", which spends no attempt and
	// stalls the entry until the stall bound.
	//
	// Dropping them costs one signature: the entry keeps its MsgBytes, so the
	// reconciler signs a fresh transaction, which is a question the node has
	// not answered yet. RejectionPreservesBytes is the same predicate the
	// reconciler applies to its own resend, one block later; asking it here
	// only means asking it as soon as the answer is known. A nil error is an
	// accepted send, and the predicate answers true for it, so the success path
	// is unchanged by construction rather than by care.
	if !tx.RejectionPreservesBytes(attemptErr) {
		signed = tx.SignedTxPayload{}
	}
	entryBytes, eErr := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:            msgBytes,
		SubmitHeight:        submitHeight,
		TxHash:              txHash,
		OrigTxHash:          txHash,
		ServiceID:           snapshot.ServiceID,
		SignedBytes:         signed.Bytes,
		SignedTimeoutAt:     signedTimeoutNanos(signed),
		SignedTimeoutHeight: signed.TimeoutHeight,
		TimeoutSeconds:      int64(budget / time.Second),
		TimeoutRegime:       regime,
	})
	if eErr != nil {
		return fmt.Errorf("encoding rebroadcast entry: %w", eErr)
	}
	if pErr := lc.rebroadcastStore.Put(
		ctx, phase,
		snapshot.SupplierOperatorAddress, snapshot.SessionEndHeight, snapshot.SessionID,
		entryBytes,
	); pErr != nil {
		return fmt.Errorf("storing rebroadcast entry: %w", pErr)
	}
	return nil
}

// getClaimReward calculates the expected reward for a claim using the canonical
// poktroll formula (prooftypes.Claim.GetClaimeduPOKT). This uses the SMST root
// hash as the source of truth — not snapshot counters.
func (lc *LifecycleCallback) getClaimReward(
	ctx context.Context,
	claim *prooftypes.Claim,
	serviceID string,
) (sdk.Coin, error) {
	if lc.proofChecker == nil {
		return sdk.Coin{}, fmt.Errorf("proof checker not available")
	}
	difficultyClient := lc.proofChecker.ServiceDifficultyClient()
	if difficultyClient == nil {
		return sdk.Coin{}, fmt.Errorf("difficulty client not available")
	}

	// Both the shared params (ComputeUnitsToTokensMultiplier, ComputeUnitCostGranularity)
	// and the relay mining difficulty must be those effective at the session START
	// height, to exactly match how the chain computes the claimed uPOKT in
	// x/proof/keeper/msg_server_create_claim.go (GetParamsAtHeight(sessionStartHeight)
	// + GetRelayMiningDifficultyAtHeight(sessionStartHeight)). Using live shared params
	// would diverge from the chain after a session-length / CUTTM param change.
	sessionStartHeight := claim.SessionHeader.GetSessionStartBlockHeight()

	difficulty, err := difficultyClient.GetServiceRelayDifficultyAtHeight(
		ctx, serviceID, sessionStartHeight,
	)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("failed to fetch relay mining difficulty: %w", err)
	}

	sharedParams, err := lc.sharedClient.GetParamsAtHeight(ctx, sessionStartHeight)
	if err != nil {
		return sdk.Coin{}, fmt.Errorf("failed to get shared params: %w", err)
	}

	return claim.GetClaimeduPOKT(*sharedParams, difficulty)
}

// claimBuildResult carries the outcome of building a single session's claim
// inside OnSessionsNeedClaim's parallel fan-out. It is lifted out of the
// enclosing method so the collector logic can be unit tested independently
// of the full claim pipeline (block waits, supplier client, etc.).
type claimBuildResult struct {
	index      int
	snapshot   *SessionSnapshot
	claimMsg   *prooftypes.MsgCreateClaim
	rootHash   []byte
	err        error
	skipped    bool // true if session was skipped (empty tree, unprofitable)
	skipReason string
}

// claimBuildCollection is the partitioned result of draining numTasks
// claimBuildResult values from the worker channel. The caller uses `built`
// to populate the batched MsgCreateClaim, iterates `skipped` to run the
// per-reason finalise paths, and surfaces `failed` via warnings + metrics
// for operators (and for the next claim-window retry).
type claimBuildCollection struct {
	built   []claimBuildResult
	skipped []claimBuildResult
	failed  []claimBuildResult
}

// proofBuildResult is the output of a single proof-build goroutine
// inside OnSessionsNeedProof. Declared at package scope so that the
// collector (collectProofBuildResults) can consume a typed channel
// without redeclaring the struct inside the function.
type proofBuildResult struct {
	index    int
	snapshot *SessionSnapshot
	proofMsg *prooftypes.MsgSubmitProof
	err      error
}

// settleEjectedClaim gives ONE ejected message its own verdict, at the moment
// the chain names it, so that the batch it was holding back can go on.
//
// It persists a rebroadcast entry, and that is the load-bearing decision. The
// proof-side precedent (settleNotRequiredBatch) deliberately persists NOTHING,
// but its verdict is terminal BY NATURE -- the proof requirement is seeded from
// a fixed block hash, so every future resend asks the same question. No claim
// verdict is demonstrated terminal that way, and one of them is provably
// TRANSIENT: poktroll x/proof/keeper/session.go rejects a claim that arrives
// BEFORE the supplier's earliest commit height, which the next block fixes. An
// ejected message with no entry would be forfeited for a condition that heals
// itself, so the default is to keep it and there is no enumeration of "terminal"
// verdicts to maintain -- classifying chain behaviour by text is exactly what
// goes stale. Keeping one too many costs up to one resend per block the window
// has left: the default sets no cap, and the entry holds no signed bytes, so the
// reconciler signs a fresh transaction each block, the chain refuses it again
// and the attempt is counted, until the window closes and the entry is recorded
// missing and cleared. Each one is a signature and a permit, plus a simulation
// under automatic gas; whether the chain also charges a fee depends on where it
// refuses, which is not walked here. Keeping one too few costs a claim.
//
// OrigTxHash is empty because nothing was transmitted, which is TRUE. The
// reconciler resends it from SubmitHeight+1, as it does every stored entry
// (canRebroadcast); the empty hash does not decide that.
func (lc *LifecycleCallback) settleEjectedClaim(
	ctx context.Context,
	logger logging.Logger,
	ejected claimBuildResult,
	submitErr error,
	earliestClaimHeight int64,
	budget time.Duration,
	regime string,
) {
	snapshot := ejected.snapshot
	currentHeight := lc.blockClient.LastBlock(ctx).Height()

	// THE RECOVERY PATH IS PERSISTED FIRST, AND THE ORDER IS THE POINT.
	//
	// These four writes are not atomic, so the process can die between any two
	// of them. Marking the session terminal first is the one order that loses
	// the claim outright: `claim_tx_error` is terminal (SessionState.IsTerminal)
	// and loadExistingSessions refuses to load a terminal session back into
	// activeSessions, so nothing re-forms a batch containing it -- while the
	// reconciler's entire universe is the set of PERSISTED entries (it iterates
	// `pending`, the store listing). Terminal-without-an-entry is reachable by
	// neither path, and this message never travelled, so nothing is on-chain
	// either: the claim is simply gone.
	//
	// With the entry first, every intermediate death is benign instead. The
	// session stays non-terminal AND has an entry, so it is either re-formed
	// into the batch and ejected again -- Put is an HSet keyed by session ID, so
	// the second persist overwrites rather than duplicating -- or re-sent alone
	// by the reconciler.
	//
	// It does NOT close a death BEFORE the first write: there the ejection
	// happened for nobody, which is the ordinary loss of a whole in-flight group
	// and not specific to ejection.
	recoverable := false
	if lc.rebroadcastStore != nil {
		msgBytes, mErr := ejected.claimMsg.Marshal()
		if mErr != nil {
			logger.Error().Err(mErr).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Msg("ejected claim will never be retried: its message cannot be marshalled")
		} else if pErr := lc.persistRebroadcastEntry(
			// No signed transaction: this claim was ejected from its batch and
			// never broadcast, so there is nothing to re-inject and the resend
			// will sign. That is the same state as an entry written before this
			// field existed.
			ctx, RebroadcastPhaseClaim, snapshot, currentHeight, "", tx.SignedTxPayload{}, nil, budget, regime, msgBytes,
		); pErr != nil {
			logger.Error().Err(pErr).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Msg("ejected claim will never be retried: its rebroadcast entry did not land")
		} else {
			recoverable = true
		}
	}

	// The verdict has to say WHICH loss this is. An ordinary `claim_tx_error`
	// promises a retry that the reconciler will actually make; with no entry
	// there is no such retry, and counting both under the same reason makes a
	// permanent loss indistinguishable from a pending one on the only surface an
	// operator watches. A nil store is deliberately NOT counted as unrecoverable:
	// an operator who disabled the reconciler already knows no retry is coming,
	// and stamping every ejection would drown the case that is a surprise.
	if recoverable || lc.rebroadcastStore == nil {
		// `recoverable` is exactly "an entry landed, so the reconciler will
		// answer for this session". With no store at all nothing will answer,
		// and the money is counted forgone rather than left waiting forever.
		RecordClaimTxError(
			snapshot.SupplierOperatorAddress,
			snapshot.ServiceID,
			recoverable,
			snapshot.RelayCount,
			int64(snapshot.TotalComputeUnits),
		)
	} else {
		RecordClaimEjectedUnrecoverable(
			snapshot.SupplierOperatorAddress,
			snapshot.ServiceID,
			snapshot.RelayCount,
			int64(snapshot.TotalComputeUnits),
		)
	}

	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnClaimTxError(ctx, snapshot.SessionID); err != nil && !errors.Is(err, ErrClaimAlreadyOnChain) {
			logger.Warn().Err(err).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Msg("failed to mark ejected session as claim_tx_error in Redis")
		}
	}

	if lc.submissionTracker != nil {
		if trackErr := lc.submissionTracker.TrackClaimSubmission(
			ctx,
			snapshot.SupplierOperatorAddress,
			snapshot.ServiceID,
			snapshot.ApplicationAddress,
			snapshot.SessionID,
			snapshot.SessionStartHeight,
			snapshot.SessionEndHeight,
			hex.EncodeToString(ejected.rootHash),
			"",    // no TX hash: this message never travelled
			false, // failed
			submitErr.Error(),
			earliestClaimHeight,
			currentHeight,
			snapshot.RelayCount,
			int64(snapshot.TotalComputeUnits),
			false, // proof_required unknown at claim time
			"",    // proof_requirement_seed unknown at claim time
		); trackErr != nil {
			logger.Warn().Err(trackErr).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Msg("failed to track ejected claim submission")
		}
	}
}

// settleNotRequiredBatch records the per-session outcome of a batch the chain
// refused because one of its proofs was not required.
//
// The message the chain NAMED is settled, not failed: its claim settles without
// a proof, and that answer is stable across a retry because the requirement is
// seeded from a fixed block hash and read with params at the session's own
// heights. The others were never transmitted -- the batch is one transaction --
// so their loss is real and they take the error. Marking the named one an error
// too would record a false fact about the one session the chain actually told us
// about, and throw away the only datum it offered.
//
// Without an index every message is indistinguishable and all of them take the
// error. That is not a second policy; it is this one with nothing to split on.
// A fee, nonce or TTL failure arrives that way: the ante handler runs in
// simulation too and fails before any message executes.
//
// It deliberately persists NO rebroadcast entry. The reconciler resends every
// stored entry from the block after its submit (canRebroadcast), so a proof the
// chain just refused would be re-sent a block later, doomed, burning a permit
// and a simulation.
func (lc *LifecycleCallback) settleNotRequiredBatch(
	ctx context.Context,
	logger logging.Logger,
	submitErr error,
	snapshots []*SessionSnapshot,
) {
	named := -1
	var rejection *tx.TxRejection
	if errors.As(submitErr, &rejection) && rejection.HasMsgIndex {
		// The index is parsed out of the server's text, which can carry a second
		// "message index:" of its own. An out-of-range value is already harmless
		// here -- the loop below COMPARES against named rather than indexing
		// with it -- so this check buys audibility, not safety: without it a
		// nonsense index would settle every session as an error in silence,
		// which is indistinguishable from a batch that legitimately had none.
		if rejection.MsgIndex >= 0 && rejection.MsgIndex < len(snapshots) {
			named = rejection.MsgIndex
		} else {
			logger.Warn().
				Int("msg_index", rejection.MsgIndex).
				Int("batch_size", len(snapshots)).
				Msg("proof not required: message index outside the batch, settling every session as an error")
		}
	}

	for i, snapshot := range snapshots {
		if i == named {
			logger.Info().
				Str(logging.FieldSessionID, snapshot.SessionID).
				Int("batch_size", len(snapshots)).
				Msg("proof not required: the chain named this session, settling it as probabilistically proved")
			RecordRevenueProbabilisticProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)
			if lc.sessionCoordinator != nil {
				if err := lc.sessionCoordinator.OnProbabilisticProved(ctx, snapshot.SessionID); err != nil {
					logger.Warn().Err(err).Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("failed to mark session as probabilistic_proved")
				}
			}
			continue
		}

		// NOT resolvable, and the comment above this function says why: this path
		// deliberately persists no rebroadcast entry, because the chain just
		// refused these proofs and resending the same bytes is doomed. With
		// nothing that will ever answer, the money is lost now rather than
		// waiting in `unresolved` for a resolver that does not exist.
		RecordProofTxError(snapshot.SupplierOperatorAddress, snapshot.ServiceID, false, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))
		if lc.sessionCoordinator != nil {
			if err := lc.sessionCoordinator.OnProofTxError(ctx, snapshot.SessionID); err != nil {
				logger.Warn().Err(err).Str(logging.FieldSessionID, snapshot.SessionID).
					Msg("failed to mark session as proof_tx_error in Redis")
			}
		}
	}
}

// alignClaimBatch derives, from ONE slice of build results, every parallel view
// the submission path needs. It returns FOUR, and the fourth is the one that
// actually travels: re-deriving three and reusing a stale interfaceClaimMsgs
// would send a batch whose contents disagree with the bookkeeping, attributing
// each outcome to the wrong session. Deriving them together in one pass is what
// makes that disagreement unrepresentable.
func alignClaimBatch(built []claimBuildResult) (
	[]*prooftypes.MsgCreateClaim,
	[][]byte,
	[]*SessionSnapshot,
	[]pocktclient.MsgCreateClaim,
) {
	claimMsgs := make([]*prooftypes.MsgCreateClaim, len(built))
	rootHashes := make([][]byte, len(built))
	snapshots := make([]*SessionSnapshot, len(built))
	iface := make([]pocktclient.MsgCreateClaim, len(built))
	for i, r := range built {
		claimMsgs[i] = r.claimMsg
		rootHashes[i] = r.rootHash
		snapshots[i] = r.snapshot
		iface[i] = r.claimMsg
	}
	return claimMsgs, rootHashes, snapshots, iface
}

// namedMessageIndex reports WHICH message of the batch the chain rejected, and
// whether it named one at all.
//
// This is the entire trigger for degradation, and it is narrow by CONSTRUCTION
// rather than by an enumeration someone has to keep correct: HasMsgIndex is set
// only by newSimulateRejection, the one constructor that calls parseMsgIndex.
// A transport failure, a CheckTx rejection, a saturated permit, an expired
// context, and every ante-handler failure (fee, nonce, TTL -- the ante runs in
// simulation too and fails BEFORE any message executes) all arrive without one,
// and all of them must retry the batch AS A BATCH. A trigger any wider means a
// network hiccup breaks the group into singles forever.
//
// An out-of-range index is reported as "not named": the value is parsed out of
// server text that can carry a second "message index:" of its own, and it is
// about to decide which session takes a verdict.
func namedMessageIndex(err error, batchSize int) (int, bool) {
	var rejection *tx.TxRejection
	if !errors.As(err, &rejection) || !rejection.HasMsgIndex {
		return 0, false
	}
	if rejection.MsgIndex < 0 || rejection.MsgIndex >= batchSize {
		return 0, false
	}
	return rejection.MsgIndex, true
}

// withoutSession returns snapshots minus the one with this session ID. The
// ejected session is settled at the moment of ejection, so it must also leave
// the group: every later use of groupSnapshots is a verdict, and a session that
// stayed would receive a second one.
func withoutSession(snapshots []*SessionSnapshot, sessionID string) []*SessionSnapshot {
	out := make([]*SessionSnapshot, 0, len(snapshots))
	for _, s := range snapshots {
		if s.SessionID != sessionID {
			out = append(out, s)
		}
	}
	return out
}

// alignProofBatch turns the built proof results into the three parallel slices
// the submission path needs, and it is the ONLY place they are built.
//
// Their alignment is load-bearing and invisible. When the chain refuses one
// message of a batch it names it by INDEX, and that index only means anything
// because proofMsgs[i], interfaceProofMsgs[i] and validProofSnapshots[i] all
// describe the same session. Built inline as three separate loops, that identity
// held because nobody had reordered anything yet -- with nothing asserting it,
// and with the consequence of getting it wrong being that the WRONG session is
// recorded as proved. Silently, on the money path.
//
// The sort is part of the invariant rather than preparation for it: results come
// back from the worker pool in completion order, and index is the position the
// caller handed them in.
func alignProofBatch(built []proofBuildResult) (
	[]*prooftypes.MsgSubmitProof,
	[]pocktclient.MsgSubmitProof,
	[]*SessionSnapshot,
) {
	sorted := make([]proofBuildResult, len(built))
	copy(sorted, built)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].index < sorted[j].index })

	proofMsgs := make([]*prooftypes.MsgSubmitProof, len(sorted))
	interfaceProofMsgs := make([]pocktclient.MsgSubmitProof, len(sorted))
	snapshots := make([]*SessionSnapshot, len(sorted))
	for i, result := range sorted {
		proofMsgs[i] = result.proofMsg
		interfaceProofMsgs[i] = result.proofMsg
		snapshots[i] = result.snapshot
	}
	return proofMsgs, interfaceProofMsgs, snapshots
}

// sessionIDLogPrefix is how much of a session ID goes into a log line.
const sessionIDLogPrefix = 16

// firstSessionIDsForLog renders up to five session IDs for one log line,
// truncating each to sessionIDLogPrefix characters.
//
// The length check is the reason this exists. Both call sites sliced
// SessionID[:16] bare, which PANICS on any shorter ID -- and both sit inside
// the group loop of a submission cycle, so such a panic does not cost one
// session, it takes every session of that supplier: the same "one proof's fate
// is not the others'" property this cycle is being built to buy, broken by
// another door.
//
// Not reachable from a relay on the path that was checked: relayer/validator.go
// rejects a request whose session ID does not match the chain's, and
// chain-issued IDs are 64 hex characters. NOT VERIFIED for every producer --
// the miner reads these IDs back from its own store, and nothing on that path
// enforces a length.
func firstSessionIDsForLog(snapshots []*SessionSnapshot) []string {
	const maxIDs = 5
	ids := make([]string, 0, min(maxIDs, len(snapshots)))
	for _, s := range snapshots {
		if len(ids) == maxIDs {
			break
		}
		if len(s.SessionID) > sessionIDLogPrefix {
			ids = append(ids, s.SessionID[:sessionIDLogPrefix]+"...")
			continue
		}
		ids = append(ids, s.SessionID)
	}
	return ids
}

// groupOnePerSession puts every session in its own group -- one group is one
// transaction -- in the deterministic order orderGroupsByWindow defines.
//
// It takes no flag, and that is the point: proofs are never batched, and while
// this was a bool parameter shared with the claim path, restoring the batching
// S4 removed took changing one argument. A guarantee that costs one character to
// undo is not one. Grouping proofs again now requires writing code.
func groupOnePerSession(snapshots []*SessionSnapshot) [][]*SessionSnapshot {
	groups := make([][]*SessionSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		groups = append(groups, []*SessionSnapshot{snapshot})
	}
	return orderGroupsByWindow(groups)
}

// groupByEndHeight puts the sessions that share a session end height -- and so
// share a submission window -- in one group, so they travel in one transaction.
//
// This is the ONLY shape a claim group takes. It read "by default" until the
// operator switch was retired (config/unknown_keys.go), and that phrasing
// outlived the choice it described: it tells the reader to look for the other
// form, and there is none. Ejection narrows a group's CONTENTS when the chain
// names one message, but never its shape -- the rest keeps travelling batched.
func groupByEndHeight(snapshots []*SessionSnapshot) [][]*SessionSnapshot {
	groups := make([][]*SessionSnapshot, 0, len(snapshots))
	byEndHeight := make(map[int64]int, len(snapshots))
	for _, snapshot := range snapshots {
		if idx, ok := byEndHeight[snapshot.SessionEndHeight]; ok {
			groups[idx] = append(groups[idx], snapshot)
			continue
		}
		byEndHeight[snapshot.SessionEndHeight] = len(groups)
		groups = append(groups, []*SessionSnapshot{snapshot})
	}
	return orderGroupsByWindow(groups)
}

// orderGroupsByWindow puts the group whose window closes first at the front, and
// is DETERMINISTIC even when it cannot tell two groups apart.
//
// Both paths used to range over a map keyed by end height, and Go randomises map
// iteration, so the order in which groups reached the chain changed between
// runs. That is invisible while one failing group ends the cycle -- the abandoned
// ones are abandoned either way -- and becomes a coin flip over which sessions
// get through once the cycle keeps going.
//
// Sorting by end height alone is NOT enough, and this is the part that is easy to
// get wrong: sessions are anchored to a global grid, so in the ordinary case
// every group carries the SAME end height and the comparison is a tie on every
// pair. sort.SliceStable keeps arrival order under that tie, which is what
// actually fixes the sequence; the sort by height only matters when heights
// differ.
func orderGroupsByWindow(groups [][]*SessionSnapshot) [][]*SessionSnapshot {
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i][0].SessionEndHeight < groups[j][0].SessionEndHeight
	})
	return groups
}

// proofBuildCollection is the partitioned result of draining numTasks
// proofBuildResult values from the proof worker channel. Proofs have no
// skip path (unlike claims, which can bail early on "unprofitable" or
// "empty_tree"); every attempt either builds or fails.
type proofBuildCollection struct {
	built  []proofBuildResult
	failed []proofBuildResult
}

// collectProofBuildResults drains exactly numTasks values from resultsCh
// and partitions them into (built, failed). A prior implementation bailed
// out on the first result.err != nil, which had two serious consequences:
//
//  1. The remaining goroutines were still attempting to write into a
//     buffered channel of size numTasks that would never be read again,
//     leaking one goroutine per remaining task until process exit.
//  2. Proof-builds that had already completed successfully were silently
//     abandoned — their sessions stayed in SessionStateClaimed forever,
//     the proof window closed, and the supplier lost the claim even
//     though the proof was ready to submit.
//
// Like collectClaimBuildResults, this function never returns an error:
// it drains every task and lets the caller decide what to do with each
// bucket. resultsCh MUST deliver exactly numTasks values.
func collectProofBuildResults(numTasks int, resultsCh <-chan proofBuildResult) proofBuildCollection {
	coll := proofBuildCollection{}
	if numTasks <= 0 {
		return coll
	}
	coll.built = make([]proofBuildResult, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		r := <-resultsCh
		if r.err != nil {
			coll.failed = append(coll.failed, r)
			continue
		}
		coll.built = append(coll.built, r)
	}
	return coll
}

// collectClaimBuildResults drains exactly numTasks values from results and
// partitions them into (built, skipped, failed).
//
// A prior implementation bailed out on the first result.err != nil, which
// silently abandoned every session whose buildFunc had already completed
// successfully in the same batch — including sessions whose SMST had
// already been sealed (claimedRoot set) and so could not be re-built on a
// subsequent claim-window retry. That produced claims that never made it
// on-chain even though the supplier had fully mined the relays.
//
// This function never returns an error: it collects all results and lets
// the caller decide per-bucket what to do. Failures are surfaced to the
// caller verbatim (full err chain preserved) so the caller can log and
// meter them without losing context.
//
// The resultsCh MUST deliver exactly numTasks values; callers that
// cancel early must drain it themselves to avoid leaking goroutines.
func collectClaimBuildResults(numTasks int, resultsCh <-chan claimBuildResult) claimBuildCollection {
	coll := claimBuildCollection{}
	if numTasks <= 0 {
		return coll
	}
	coll.built = make([]claimBuildResult, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		r := <-resultsCh
		switch {
		case r.err != nil:
			coll.failed = append(coll.failed, r)
		case r.skipped:
			coll.skipped = append(coll.skipped, r)
		default:
			coll.built = append(coll.built, r)
		}
	}
	return coll
}

// OnSessionActive is called when a new session starts.
// For HA miner, sessions are created on-demand when relays arrive, so this is mostly informational.
func (lc *LifecycleCallback) OnSessionActive(_ context.Context, snapshot *SessionSnapshot) error {
	lc.logger.Debug().
		Str(logging.FieldSessionID, snapshot.SessionID).
		Int64(logging.FieldSessionEndHeight, snapshot.SessionEndHeight).
		Str(logging.FieldServiceID, snapshot.ServiceID).
		Msg("session active")

	return nil
}

// OnSessionsNeedClaim is called when sessions need claims submitted (batched).
// It waits for the proper timing spread, flushes SMSTs, and submits all claims in a single transaction.
//
// It reports WHICH sessions were claimed, by session ID. It used to return the
// root hashes in a slice parallel to snapshots, filled through a counter that
// only advanced for sessions that submitted -- so the slice left-packed and the
// caller transitioned the first k sessions whatever they were. The root hash was
// never needed there: it is written into the session itself while the claim is
// built, so returning it a second time was a duplicate that could disagree.
func (lc *LifecycleCallback) OnSessionsNeedClaim(ctx context.Context, snapshots []*SessionSnapshot) (ClaimCycleResult, error) {
	result := ClaimCycleResult{Claimed: make(map[string]struct{}, len(snapshots))}
	if len(snapshots) == 0 {
		return result, nil
	}
	// groupErrs accumulates one entry per group that did not submit, so the
	// caller sees every failure instead of only the first -- and so a group
	// that fails does not take the groups behind it with it.
	var groupErrs []error

	// All sessions for a single supplier, so we can batch them
	firstSnapshot := snapshots[0]
	logger := lc.logger.With().
		Str(logging.FieldSupplier, firstSnapshot.SupplierOperatorAddress).
		Int("batch_size", len(snapshots)).
		Logger()

	logger.Debug().Msg("batched sessions need claims - starting claim process")

	// Group sessions by session end height (they might have different claim
	// windows). There is no other shape: the operator switch that used to make
	// this one-per-session is retired (config/unknown_keys.go), so claims are
	// always grouped and groupOnePerSession belongs to the proof path alone.
	//
	// A SLICE, not a map: Go randomises map iteration, so the order in which
	// groups reached the chain changed between runs -- and once a failing group
	// no longer ends the cycle, that order decides which sessions get through
	// when the window runs out. Sorting by end height alone does not settle it
	// either, because sessions share one end height in the ordinary case; the
	// stable sort keeps arrival order under that tie.
	groups := groupByEndHeight(snapshots)

	logger.Info().
		Int("total_sessions", len(snapshots)).
		Int("num_transactions", len(groups)).
		Msg("grouping claims by session end height")

	// Process each group (same claim window) separately
	for _, groupSnapshots := range groups {
		// Get the actual session end height from the first snapshot in the group
		// (all snapshots in a group have the same end height)
		sessionEndHeight := groupSnapshots[0].SessionEndHeight

		// Shared params effective at this session's height, not live params. After a
		// session-length change (poktroll #543 anchored grid), an old-epoch group
		// computed with new-epoch params would resolve the wrong claim window.
		sharedParams, err := lc.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
		if err != nil {
			groupErrs = append(groupErrs,
				fmt.Errorf("failed to get shared params at height %d: %w", sessionEndHeight, err))
			continue
		}

		// Wait for claim window to open and get the block hash for timing spread
		claimWindowOpenHeight := sharedtypes.GetClaimWindowOpenHeight(sharedParams, sessionEndHeight)
		claimWindowCloseHeight := sharedtypes.GetClaimWindowCloseHeight(sharedParams, sessionEndHeight)
		currentHeight := lc.blockClient.LastBlock(ctx).Height()

		// Build session IDs list for logging (truncate if too many)
		sessionIDs := firstSessionIDsForLog(groupSnapshots)

		logger.Debug().
			Int64("claim_window_open_height", claimWindowOpenHeight).
			Int64("claim_window_close_height", claimWindowCloseHeight).
			Int64("session_end_height", sessionEndHeight).
			Int64("current_height", currentHeight).
			Int("group_size", len(groupSnapshots)).
			Str("first_service_id", groupSnapshots[0].ServiceID).
			Strs("session_ids", sessionIDs).
			Msg("waiting for claim window to open")

		if _, blockErr := lc.waitForBlock(ctx, claimWindowOpenHeight); blockErr != nil {
			groupErrs = append(groupErrs,
				fmt.Errorf("failed to wait for claim window open: %w", blockErr))
			continue
		}

		// NOTE: Timing spread DISABLED - submit claims immediately when window opens
		// The protocol's GetEarliestSupplierClaimCommitHeight() spreads suppliers across the window,
		// but this can cause claims to be submitted too close to window close, resulting in failures.
		// We now submit immediately when the claim window opens to maximize success rate.
		earliestClaimHeight := claimWindowOpenHeight // Use window open, not spread height

		logger.Info().
			Int64("claim_window_open", claimWindowOpenHeight).
			Int64("session_end_height", sessionEndHeight).
			Msg("claim window open - submitting immediately (timing spread disabled)")

		// CRITICAL: Verify claim window is still open AND we have enough time to build+submit
		// AGGRESSIVE MODE: Push claims until the very last block of the window
		claimWindowClose := sharedtypes.GetClaimWindowCloseHeight(sharedParams, sessionEndHeight)
		currentBlock := lc.blockClient.LastBlock(ctx)
		blocksRemaining := claimWindowClose - currentBlock.Height()

		// AGGRESSIVE: No buffer - use every available block in the window
		const minBlocksRequired = 2

		if blocksRemaining <= minBlocksRequired {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("claim_window_close", claimWindowClose).
				Int64("blocks_remaining", blocksRemaining).
				Int64("min_blocks_required", minBlocksRequired).
				Int64("session_end_height", sessionEndHeight).
				Int("group_size", len(groupSnapshots)).
				Msg("insufficient time remaining to build and submit claims - aborting (sessions will be marked as failed)")

			// Mark all sessions in this group as failed (metrics + Redis state for HA)
			for _, snapshot := range groupSnapshots {
				lc.markAndCountClaimWindowClosed(ctx, snapshot, claimWindowClose)
			}

			groupErrs = append(groupErrs,
				fmt.Errorf("insufficient time to build claims: %d blocks remaining, %d required (window closes at %d, current: %d)",
					blocksRemaining, minBlocksRequired, claimWindowClose, currentBlock.Height()))
			continue
		}

		logger.Debug().
			Int("group_size", len(groupSnapshots)).
			Int64("claim_window_close", claimWindowClose).
			Int64("current_height", currentBlock.Height()).
			Int64("blocks_remaining", claimWindowClose-currentBlock.Height()).
			Msg("claim window timing reached - flushing SMSTs and submitting batched claims")

		// DEBUG/TEST: Intentionally delay claim submission to test claim window timeout tracking
		// Set environment variable TEST_CLAIM_DELAY_SECONDS to enable (e.g., "60" for 60 seconds)
		// This simulates scenarios where SMST flushing takes too long or network delays occur
		if testCfg := getTestConfig(); testCfg.ClaimDelaySeconds > 0 {
			delayDuration := time.Duration(testCfg.ClaimDelaySeconds) * time.Second
			logger.Warn().
				Int("delay_seconds", testCfg.ClaimDelaySeconds).
				Int64("claim_window_close", claimWindowClose).
				Int64("current_height", currentBlock.Height()).
				Msg("TEST MODE: Intentionally delaying claim submission to test window timeout tracking")
			time.Sleep(delayDuration)

			// Log current state after delay
			postDelayHeight := lc.blockClient.LastBlock(ctx).Height()
			logger.Warn().
				Int64("height_after_delay", postDelayHeight).
				Int64("claim_window_close", claimWindowClose).
				Bool("window_still_open", postDelayHeight < claimWindowClose).
				Msg("TEST MODE: Delay complete, continuing with claim submission")
		}

		// Pre-filter: skip already-claimed sessions (dedup).
		// This is the only check that uses snapshot data as a gate.
		var candidateSnapshots []*SessionSnapshot
		for _, snapshot := range groupSnapshots {
			if snapshot.ClaimTxHash != "" {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
					Str("existing_claim_tx_hash", snapshot.ClaimTxHash).
					Msg("skipping claim - already submitted for this session (deduplication)")
				continue
			}
			candidateSnapshots = append(candidateSnapshots, snapshot)
		}

		// PARALLEL CLAIM BUILDING: flush SMST first (source of truth), then
		// use the root hash for economic viability — matching poktroll's
		// canonical flow (flush → GetClaimeduPOKT → submit).
		//
		// Snapshot counters (relay_count, total_compute_units) are NOT used
		// for claim decisions because they can race with concurrent updates.
		// The SMST root hash encodes the real count and sum atomically.
		results := make(chan claimBuildResult, len(candidateSnapshots))
		numTasks := len(candidateSnapshots)

		// Resolve fee cost once for the entire batch (shared across all sessions)
		claimAndProofCostUpokt := lc.supplierClient.GetEstimatedFeeUpokt(ctx)

		for i, snapshot := range candidateSnapshots {
			index := i
			snap := snapshot

			buildFunc := func() {
				result := claimBuildResult{
					index:    index,
					snapshot: snap,
				}

				// Panic guard: the collector downstream MUST receive exactly
				// numTasks values or it leaks a goroutine per missing slot.
				// If any step below panics (nil snapshot field, SMST boundary
				// miss, proto zero-value) the deferred recover converts the
				// panic into a failed result so the collector drains cleanly.
				// Covers both the pool path (pond also absorbs panics, but
				// would not write our channel) and the fallback go path.
				defer func() {
					if r := recover(); r != nil {
						logging.PanicRecoveriesTotal.WithLabelValues("claim_build").Inc()
						lc.logger.Error().
							Str(logging.FieldSessionID, snap.SessionID).
							Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
							Str("panic_value", fmt.Sprintf("%v", r)).
							Str("stack_trace", string(debug.Stack())).
							Msg("PANIC RECOVERED in claim build goroutine")
						results <- claimBuildResult{
							index:    index,
							snapshot: snap,
							err:      fmt.Errorf("claim build panic for session %s: %v", snap.SessionID, r),
						}
					}
				}()

				// Phase 1: Flush the SMST to get the root hash (source of truth).
				// The root hash encodes count (relays) and sum (compute units)
				// atomically — no race with concurrent relay processing.
				rootHash, flushErr := lc.smstManager.FlushTree(ctx, snap.SessionID)
				if flushErr != nil {
					result.err = fmt.Errorf("failed to flush SMST for session %s: %w", snap.SessionID, flushErr)
					results <- result
					return
				}
				// Belt-and-suspenders: smt.MerkleSumRoot.Count()/Sum() slice into
				// the trailing 16 bytes without bounds checking. Refuse to
				// build a claim from a malformed root rather than panicking.
				if len(rootHash) != SMSTRootLen {
					result.err = fmt.Errorf("session %s: flushed root has invalid length %d, expected %d", snap.SessionID, len(rootHash), SMSTRootLen)
					results <- result
					return
				}
				result.rootHash = rootHash

				// Phase 2: Extract count and sum from the SMST root hash.
				// These are the authoritative values — not the snapshot counters.
				smstRoot := smt.MerkleSumRoot(rootHash)
				smstCount, countErr := smstRoot.Count()
				if countErr != nil {
					result.err = fmt.Errorf("failed to read count from SMST root for session %s: %w", snap.SessionID, countErr)
					results <- result
					return
				}
				smstSum, sumErr := smstRoot.Sum()
				if sumErr != nil {
					result.err = fmt.Errorf("failed to read sum from SMST root for session %s: %w", snap.SessionID, sumErr)
					results <- result
					return
				}

				// The two numbers that decide whether we get paid for what we
				// served, and the only moment they coexist: smstCount is what the
				// chain will bill (num_relays is Count() of this very root), and
				// snap.RelayCount is what the session coordinator counted in Redis.
				// They come from different writers -- a Lua HINCRBY on the shared
				// session hash versus this process's own trie -- so a shortfall
				// here is work we performed and will not be paid for.
				//
				// Measured 2026-08-19 (FINDING-partial-claim): five claims where
				// the coordinator had counted 4 and the flushed root held 1, and
				// nothing said so -- claim_success and proof_success were both
				// true. This recorder was written for exactly that in April, its
				// only caller was deleted with claim_pipeline.go, and the metric
				// has been declared, documented and silent ever since.
				RecordClaimLeafStats(
					snap.SupplierOperatorAddress,
					snap.ServiceID,
					snap.SessionID,
					int64(smstCount),
					snap.RelayCount,
				)
				if int64(smstCount) < snap.RelayCount {
					logger.Warn().
						Str(logging.FieldSessionID, snap.SessionID).
						Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
						Str(logging.FieldServiceID, snap.ServiceID).
						Int64("claim_leaf_count", int64(smstCount)).
						Int64("coordinator_relay_count", snap.RelayCount).
						Msg("claiming fewer leaves than relays counted -- served work that will not be billed")
				}

				// Phase 3: Check if tree is empty (no relays mined).
				if smstCount == 0 || smstSum == 0 {
					logger.Warn().
						Str(logging.FieldSessionID, snap.SessionID).
						Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
						Uint64("smst_count", smstCount).
						Uint64("smst_sum", smstSum).
						Int64("snapshot_relay_count", snap.RelayCount).
						Msg("skipping claim - SMST tree is empty (0 mined relays)")
					result.skipped = true
					result.skipReason = "empty_tree"
					results <- result
					return
				}

				// Phase 3.5: CUPR consistency guard. poktroll validates
				// num_claimed_compute_units == num_relays * service.ComputeUnitsPerRelay
				// at claim-create (x/proof) and again at settlement (x/tokenomics),
				// resolving CUPR at the session's START height. Compare against that
				// same height: a service owner who changes CUPR after this session
				// ended does not affect what the chain will accept, so reading the
				// LIVE value here would terminally skip a claim the chain would have
				// paid. Skip only on a real mismatch, and fail OPEN on query error —
				// never drop a claim we cannot prove is doomed.
				//
				// No cache-busting: CUPR at a past height is immutable, so the query
				// layer's per-(service, height) entry is always correct.
				cuprAllowed, guardCUPR, guardErr := evaluateClaimCUPRGuard(
					ctx, lc.serviceClient, snap.ServiceID, snap.SessionStartHeight, smstSum, smstCount,
				)
				switch {
				case guardErr != nil:
					logger.Debug().Err(guardErr).
						Str(logging.FieldSessionID, snap.SessionID).
						Int64("session_start_height", snap.SessionStartHeight).
						Msg("CUPR guard: failed to query session-start service CUPR, allowing claim")
				case !cuprAllowed:
					logger.Warn().
						Str(logging.FieldSessionID, snap.SessionID).
						Str(logging.FieldServiceID, snap.ServiceID).
						Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
						Int64("session_start_height", snap.SessionStartHeight).
						Uint64("smst_compute_units", smstSum).
						Uint64("smst_relay_count", smstCount).
						Uint64("session_start_compute_units_per_relay", guardCUPR).
						Uint64("expected_compute_units", smstCount*guardCUPR).
						Msg("SKIP CUPR MISMATCH: SMST sum != relays * session-start CUPR (relays mined at a different CUPR); claim would be rejected on-chain")
					result.skipped = true
					result.skipReason = "cupr_mismatch"
					results <- result
					return
				}

				// Phase 4: Economic viability using the SMST root hash.
				// Build a temporary Claim (like poktroll does) to call GetClaimeduPOKT.
				if claimAndProofCostUpokt > 0 {
					sessionHeader, headerErr := lc.buildSessionHeader(ctx, snap)
					if headerErr != nil {
						result.err = fmt.Errorf("failed to build session header for %s: %w", snap.SessionID, headerErr)
						results <- result
						return
					}

					tempClaim := prooftypes.Claim{
						SupplierOperatorAddress: snap.SupplierOperatorAddress,
						SessionHeader:           sessionHeader,
						RootHash:                rootHash,
					}

					rewardCoin, rewardErr := lc.getClaimReward(ctx, &tempClaim, snap.ServiceID)
					if rewardErr != nil {
						// Fail open on reward calculation errors — don't skip a
						// potentially valid claim due to a transient query failure.
						logger.Debug().Err(rewardErr).
							Str(logging.FieldSessionID, snap.SessionID).
							Msg("economic viability: failed to calculate reward, allowing claim")
					} else if rewardCoin.IsNil() || uint64(rewardCoin.Amount.Int64()) <= claimAndProofCostUpokt {
						logger.Warn().
							Str(logging.FieldSessionID, snap.SessionID).
							Str(logging.FieldServiceID, snap.ServiceID).
							Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
							Uint64("claim_and_proof_cost_upokt", claimAndProofCostUpokt).
							Str("expected_reward", rewardCoin.String()).
							Uint64("smst_compute_units", smstSum).
							Uint64("smst_relay_count", smstCount).
							Msg("SKIP UNPROFITABLE: expected reward < claim+proof cost (calculated from SMST root hash)")

						result.skipped = true
						result.skipReason = "unprofitable"
						results <- result
						return
					}

					// Build claim message (session header already built above)
					result.claimMsg = &prooftypes.MsgCreateClaim{
						SupplierOperatorAddress: snap.SupplierOperatorAddress,
						SessionHeader:           sessionHeader,
						RootHash:                rootHash,
					}

					results <- result
					return
				}

				// No fee estimation available — build claim without viability check
				sessionHeader, headerErr := lc.buildSessionHeader(ctx, snap)
				if headerErr != nil {
					result.err = fmt.Errorf("failed to build session header for %s: %w", snap.SessionID, headerErr)
					results <- result
					return
				}

				result.claimMsg = &prooftypes.MsgCreateClaim{
					SupplierOperatorAddress: snap.SupplierOperatorAddress,
					SessionHeader:           sessionHeader,
					RootHash:                rootHash,
				}

				results <- result
			}

			if lc.buildPool != nil {
				lc.buildPool.Submit(buildFunc)
			} else {
				go buildFunc()
			}
		}

		// Collect all results (blocking until all tasks complete). We
		// deliberately collect every result even when some buildFuncs
		// fail: a single-session failure (e.g. a transient
		// buildSessionHeader query error) must not abandon the other
		// sessions in the batch, because their SMSTs have already been
		// sealed (claimedRoot set) and cannot be re-built on a future
		// retry — leaving them in-limbo silently drops claims that were
		// otherwise valid and properly mined.
		partitioned := collectClaimBuildResults(numTasks, results)

		// Per-reason finalise paths for skipped sessions.
		for _, r := range partitioned.skipped {
			snap := r.snapshot
			switch r.skipReason {
			case "unprofitable", "cupr_mismatch":
				RecordClaimSkipped(snap.SupplierOperatorAddress, snap.ServiceID, r.skipReason)
				if skipErr := lc.OnClaimSkipped(ctx, snap); skipErr != nil {
					logger.Warn().Err(skipErr).
						Str(logging.FieldSessionID, snap.SessionID).
						Msg("failed to finalise claim_skipped transition")
				}
			case "empty_tree":
				// Session had no mined relays — not a failure, just nothing to
				// claim. It is still TERMINAL, so the per-session gauges have to
				// go: RecordClaimLeafStats already fired for this session before
				// Phase 3 decided the tree was empty, and this arm is the only
				// one that reaches no OnClaimSkipped / OnSessionProved, so
				// nothing else would ever delete them. Both carry session_id as
				// a label, which CLAUDE.md forbids leaving unbounded.
				//
				// The recording itself stays where it is on purpose: an empty
				// tree with RelayCount > 0 is the EXTREME shortfall, and
				// claimLeafCollapseTotal -- labelled supplier+service only, so
				// bounded -- must still fire for it. Only the per-session detail
				// is dropped here.
				ClearClaimLeafStats(snap.SupplierOperatorAddress, snap.ServiceID, snap.SessionID)
			}
		}

		// Failed per-session builds are surfaced as warnings + metrics
		// for operator visibility and next-window retry inspection.
		// The batch continues with whatever successfully built.
		for _, r := range partitioned.failed {
			snap := r.snapshot
			sessionID := ""
			supplier := ""
			serviceID := ""
			if snap != nil {
				sessionID = snap.SessionID
				supplier = snap.SupplierOperatorAddress
				serviceID = snap.ServiceID
			}
			RecordClaimSkipped(supplier, serviceID, "build_failed")
			logger.Warn().
				Err(r.err).
				Str(logging.FieldSessionID, sessionID).
				Str(logging.FieldSupplier, supplier).
				Str(logging.FieldServiceID, serviceID).
				Msg("session claim build failed - dropping from batch, other sessions continue")
		}

		// Valid claims — collect for submission. The scheduled-height metric is
		// recorded here and NOT inside alignClaimBatch: the batch is re-derived
		// after every ejection, and a metric inside would be re-recorded for the
		// sessions that stayed.
		for _, r := range partitioned.built {
			SetClaimScheduledHeight(
				r.snapshot.SupplierOperatorAddress,
				r.snapshot.ServiceID,
				r.snapshot.SessionID,
				float64(earliestClaimHeight),
			)
		}

		// `remaining` is the batch as it stands, and it SHRINKS when the chain
		// names a message. The four views below are derived from it and
		// re-derived together on every change.
		remaining := partitioned.built
		claimMsgs, groupRootHashes, validSnapshots, interfaceClaimMsgs := alignClaimBatch(remaining)

		// CRITICAL: Re-check window is still open RIGHT before submission
		// Building claims (SMST flush, headers) takes time - blocks may have advanced!
		currentBlock = lc.blockClient.LastBlock(ctx)
		if currentBlock.Height() >= claimWindowClose {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("claim_window_close", claimWindowClose).
				Int64("session_end_height", sessionEndHeight).
				Int("batch_size", len(claimMsgs)).
				Msg("claim window closed while building claims - cannot submit")

			// Mark all sessions in this batch as failed (metrics + Redis state for HA)
			for _, snapshot := range groupSnapshots {
				lc.markAndCountClaimWindowClosed(ctx, snapshot, claimWindowClose)
			}

			groupErrs = append(groupErrs,
				fmt.Errorf("claim window closed while building claims at height %d (current: %d)", claimWindowClose, currentBlock.Height()))
			continue
		}

		// The budget is the WHOLE claim window, not the blocks left in it, so
		// every attempt for this batch is born with the same number in front of
		// it -- a retry at block 8 of 10 is not handed a shrinking deadline.
		// What stops a late transaction is timeout_height, at the close.
		//
		// The length comes from the chain's own parameters rather than from a
		// constant: poktroll's defaults give a 3-block claim window while
		// mainnet governs it to 10, so a literal here would be one network's
		// number applied to all of them.
		claimWindowOpen := sharedtypes.GetClaimWindowOpenHeight(sharedParams, sessionEndHeight)
		claimTimeout, claimTimeoutRegime := tx.WindowTimeout(
			claimWindowClose-claimWindowOpen,
			lc.config.BlockTimeSeconds,
		)
		claimCtx := tx.WithTxWindowTimeout(ctx, claimTimeout, claimTimeoutRegime)

		logger.Info().
			Int64("current_height", currentBlock.Height()).
			Int64("claim_window_close", claimWindowClose).
			Int64("blocks_remaining", claimWindowClose-currentBlock.Height()).
			Dur("tx_deadline", claimTimeout).
			Str("tx_deadline_regime", claimTimeoutRegime).
			Int("batch_size", len(claimMsgs)).
			Msg("submitting claims")

		// Count each claim built into this batch (attempts; submitted/errors tracked separately).
		for _, snapshot := range validSnapshots {
			RecordClaimCreated(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
		}

		// Submit all claims in a single transaction with retries
		var lastErr error
		// windowClosed records that the batch already reached its terminal state
		// through markAndCount*WindowClosed. lastErr stays set (the submission DID
		// fail, and the tracker and the returned error both need it), so without
		// this the block after the loop counts the SAME sessions a second time --
		// relays_lost_total, compute_units_lost_total and upokt_lost_total doubled
		// for one session -- and overwrites the accurate window-closed state in
		// Redis with the vaguer tx_error one.
		windowClosed := false
		// claimTxHash and claimSigned are what the LAST attempt returned. They
		// come from that call's return values and from nowhere else: the client
		// is shared with the reconciler, and anything read back from it after
		// the call may be another session's transaction.
		//
		// On the failure path claimSigned is what gets persisted for
		// re-injection. The last attempt is always one made with the current
		// batch -- the loop only exhausts on a send, never on an ejection -- so
		// its bytes are the only ones that match the messages stored beside them.
		var claimTxHash string
		var claimSigned tx.SignedTxPayload
		// The increment lives in the BODY because an ejection is not a retry: the
		// batch changed, so the next send asks a different question. What bounds
		// the ejections instead is that each one strictly shrinks `remaining`,
		// and the loop refuses to go below one message.
		for attempt := 1; attempt <= lc.config.ClaimRetryAttempts; {
			// A RETRY RE-INJECTS THE SAME BYTES WHEN THEY ARE STILL WORTH
			// SENDING, INSTEAD OF SIGNING AGAIN.
			//
			// Signing again makes a transaction with a NEW unordered nonce, so
			// the node cannot recognise it as the one it may already hold: the
			// duplicate protection ("I already have this") is designed out
			// between siblings of one retry loop, and a send that got no answer
			// becomes two live transactions rather than one asked twice.
			//
			// The decision is not invented here. The inclusion reconciler
			// already makes it for its own resends -- reusable() says whether
			// the bytes are still valid against the CHAIN's clock, and
			// RejectionPreservesBytes says whether the chain judged them -- and
			// this is the other caller finally asking the same question.
			// When either says no, the send below signs, which is what this
			// loop did before: the worst case of this branch is the previous
			// behaviour.
			txHash, signed, submitErr := lc.resendOrSignClaims(
				claimCtx, claimWindowClose, claimSigned, lastErr, interfaceClaimMsgs)
			claimSigned = signed
			if submitErr != nil {
				lastErr = submitErr

				// Check if error is due to claim window being closed (permanent failure - don't retry)
				//
				// TWO LAYERS refuse a closed window and they speak different
				// languages. x/proof refuses by TEXT, during the gas simulation
				// that executes the messages, and that is what the substrings
				// below match. The SDK's ante handler refuses by CODE, in
				// CheckTx, now that the transaction carries a timeout height --
				// and its text ("block height: N, timeout height: M") contains
				// neither substring, so without the sentinel that rejection
				// falls through to the generic retry, burns the attempts, and
				// settles the session as claim_tx_error instead of
				// claim_window_closed. Same fact, opposite diagnosis.
				errorMsg := submitErr.Error()
				if errors.Is(submitErr, tx.ErrTxWindowExpired) ||
					strings.Contains(errorMsg, "claim window") || strings.Contains(errorMsg, "claim_window") {
					logger.Error().
						Err(submitErr).
						Int64("current_height", lc.blockClient.LastBlock(ctx).Height()).
						Int64("claim_window_close", claimWindowClose).
						Int("batch_size", len(claimMsgs)).
						Msg("claim window closed during submission - permanent failure, not retrying")

					// Mark all sessions in this batch as failed (metrics + Redis state for HA)
					for _, snapshot := range groupSnapshots {
						lc.markAndCountClaimWindowClosed(ctx, snapshot, claimWindowClose)
					}

					windowClosed = true
					break // Don't retry - this is a permanent failure
				}

				// DEGRADATION: the chain executed the messages and told us WHICH
				// one it refused. Eject exactly that message and re-send the
				// rest; tying the fate of healthy claims to one bad message
				// costs them their window for no reason.
				//
				// `len(remaining) > 1` is not defensive. The tx client returns
				// SUCCESS with no hash and no bytes for an empty batch, so
				// ejecting the last message would report "submitted
				// successfully" for a claim that never travelled -- and with no
				// hash, the success path would store nothing for the reconciler.
				if named, ok := namedMessageIndex(submitErr, len(remaining)); ok && len(remaining) > 1 {
					ejected := remaining[named]
					remaining = append(remaining[:named:named], remaining[named+1:]...)
					claimMsgs, groupRootHashes, validSnapshots, interfaceClaimMsgs = alignClaimBatch(remaining)
					// THE BATCH CHANGED, SO ITS BYTES ARE NO LONGER ITS BYTES.
					// Re-injecting them would send the chain exactly the message
					// it just named. Dropping them makes the next attempt sign,
					// which is the only correct answer for a batch that is not
					// the one that was signed.
					claimSigned = tx.SignedTxPayload{}
					groupSnapshots = withoutSession(groupSnapshots, ejected.snapshot.SessionID)

					lc.settleEjectedClaim(ctx, logger, ejected, submitErr, earliestClaimHeight, claimTimeout, claimTimeoutRegime)

					logger.Warn().
						Err(submitErr).
						Str(logging.FieldSessionID, ejected.snapshot.SessionID).
						Int("remaining_batch_size", len(remaining)).
						Msg("the chain named this claim; ejecting it and re-sending the rest")

					// Re-check the window on every round: an ejection costs a
					// round-trip, and the batch must not be re-sent into a window
					// that closed while we were splitting it.
					if lc.blockClient.LastBlock(ctx).Height() >= claimWindowClose {
						for _, snapshot := range groupSnapshots {
							lc.markAndCountClaimWindowClosed(ctx, snapshot, claimWindowClose)
						}
						windowClosed = true
						break
					}
					continue
				}

				logger.Warn().
					Err(submitErr).
					Int(logging.FieldAttempt, attempt).
					Int(logging.FieldMaxRetry, lc.config.ClaimRetryAttempts).
					Int("batch_size", len(claimMsgs)).
					Msg("batched claim submission failed, retrying")

				attempt++
				if attempt <= lc.config.ClaimRetryAttempts {
					select {
					case <-ctx.Done():
						// The one exit that is not a continue: a cancelled context
						// fails every remaining group too, so continuing would burn
						// the window repeating one error. It still returns the
						// sessions already claimed rather than discarding them.
						return result, errors.Join(append(groupErrs, ctx.Err())...)
					case <-time.After(lc.config.ClaimRetryDelay):
						continue
					}
				}
			} else {
				// The batch SUCCEEDED, so there is no error left to report.
				// Clearing is the whole fix: lastErr is set on every failed
				// attempt and was never unset, so a batch that failed once and
				// then succeeded still entered the `lastErr != nil` block below
				// -- counting the group as lost, rewriting the tracker as a
				// failure, and overwriting the good rebroadcast entry with
				// OrigTxHash="" (that persist has no `claimTxHash != ""` guard,
				// unlike the one in this branch). With a single-resend cap that
				// overwrite burned the one resend on a claim already on its way.
				// There is no such cap now, and the overwrite keeps the bytes of
				// the attempt that succeeded, so a resend re-injects them while
				// they are still valid. What it would cost today is the record:
				// with both hashes empty, UpdateClaimOnChainOutcome returns
				// without writing, so the "failure" the block below records is
				// never corrected to found.
				//
				// `windowClosed` is NOT the model to copy here: it exists
				// because in that case lastErr must STAY set (the submission did
				// fail, and the tracker and the returned error both need it).
				// Here the submission did not fail, so the error itself is what
				// is wrong.
				lastErr = nil

				// SUCCESS: Claim TX broadcast accepted to mempool. The hash and
				// the signed bytes are the ones this call returned, so they
				// describe one transaction. This is the ORIGINAL submission,
				// which is where re-injection has to begin -- the failure it
				// exists for is the send whose answer never arrived, and a
				// resend that had to sign again would be a second live
				// transaction for one claim.
				claimTxHash = txHash

				currentBlock := lc.blockClient.LastBlock(ctx)
				blocksAfterWindowOpen := float64(currentBlock.Height() - claimWindowOpenHeight)

				// CRITICAL: Save TX hash to Redis IMMEDIATELY (1 line after broadcast)
				// This prevents duplicate submissions if we crash after TX broadcast
				for i, snapshot := range validSnapshots {
					// Update snapshot manager with TX hash for deduplication
					if lc.sessionCoordinator != nil {
						if updateErr := lc.sessionCoordinator.OnSessionClaimed(ctx, snapshot.SessionID, groupRootHashes[i], claimTxHash); updateErr != nil {
							logger.Warn().
								Err(updateErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Str("claim_tx_hash", claimTxHash).
								Msg("failed to update snapshot after claim")
						}
					}

					// Update in-memory snapshot with claimed root hash (for proof requirement check below)
					validSnapshots[i].ClaimedRootHash = groupRootHashes[i]
				}

				// Persist each built claim message so the InclusionReconciler can
				// verify on-chain inclusion per block and re-broadcast a
				// still-missing claim while the claim window is open. The index
				// into claimMsgs matches validSnapshots (both the built-only
				// ordered set). Survives leader failover (state lives in Redis).
				//
				// It is written RIGHT AFTER the claimed state, before anything
				// else: those two writes together are what make a sent claim
				// resendable, and a process killed between them left a claimed
				// session no reconciler could see -- its claim lost if the mempool
				// dropped it.
				if lc.rebroadcastStore != nil && claimTxHash != "" {
					lc.persistRebroadcastEntries(
						ctx, RebroadcastPhaseClaim, validSnapshots, currentBlock.Height(), claimTxHash, claimSigned, nil,
						claimTimeout, claimTimeoutRegime,
						func(i int) ([]byte, error) { return claimMsgs[i].Marshal() },
					)
				}

				// NOTE: Proof requirement check moved to OnSessionsNeedProof.
				// Previously we blocked here waiting for the proof requirement seed block
				// (proofWindowOpen - 1), which is ~19 blocks in the future after claim submission.
				// This caused sequential claim groups to timeout while waiting.
				// Now sessions stay in 'claimed' state until proof window opens, where
				// OnSessionsNeedProof properly checks if proof is required.

				// Record metrics for all sessions in the batch
				for _, snapshot := range validSnapshots {
					RecordClaimSubmitted(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
					RecordClaimSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
					RecordRevenueClaimed(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

					// Name the session as claimed. By ID, not by position: the
					// root hash it just received is already on the snapshot
					// (set above) and in Redis via OnSessionClaimed.
					result.Claimed[snapshot.SessionID] = struct{}{}
				}

				// The claim is sent: from here the tree only waits for its proof,
				// and can be stored as its leaves. The compaction itself checks
				// that FlushTree's claimed_root reached Redis.
				if compactor, ok := lc.smstManager.(interface {
					ScheduleColdCompaction(ctx context.Context, sessionID string)
				}); ok {
					for _, snapshot := range validSnapshots {
						compactor.ScheduleColdCompaction(ctx, snapshot.SessionID)
					}
				}

				// Track claim submissions to Redis for debugging
				if lc.submissionTracker != nil {
					for i, snapshot := range validSnapshots {
						claimHash := hex.EncodeToString(groupRootHashes[i])
						if trackErr := lc.submissionTracker.TrackClaimSubmission(
							ctx,
							snapshot.SupplierOperatorAddress,
							snapshot.ServiceID,
							snapshot.ApplicationAddress,
							snapshot.SessionID,
							snapshot.SessionStartHeight,
							snapshot.SessionEndHeight,
							claimHash,
							claimTxHash,
							true, // success
							"",   // no error
							earliestClaimHeight,
							currentBlock.Height(),
							snapshot.RelayCount,
							int64(snapshot.TotalComputeUnits),
							false, // proof_required unknown at claim time
							"",    // proof_requirement_seed unknown at claim time
						); trackErr != nil {
							logger.Warn().
								Err(trackErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to track claim submission")
						}
					}
				}

				logger.Info().
					Int("batch_size", len(claimMsgs)).
					Str("claim_tx_hash", claimTxHash).
					Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
					Msg("batched claims submitted successfully")

				break // Success, exit retry loop
			}
		}

		if lastErr != nil && !windowClosed {
			// A store means the self-heal persist further down will hand these
			// sessions to the inclusion reconciler, which is what makes the
			// failure an ATTEMPT rather than a verdict. Read from the store
			// being wired and not from the persist's result on purpose: an
			// individual persist that fails is logged there and degrades that
			// one session to fire-once, which is the pre-reconciler behaviour
			// and is already the accepted degradation.
			resolvable := lc.rebroadcastStore != nil

			// THE NODE ANSWERING "I ALREADY HAVE THIS" IS NOT A LOST CLAIM.
			//
			// A re-injection that ARRIVES is refused with code 19, so the very
			// outcome this loop now aims for reads as an error here. Counting
			// it would report the session's relays and compute units as lost
			// and write a TERMINAL claim_tx_error, for a transaction the node
			// is holding and will most likely include -- turning the retry that
			// worked into the one that killed the session.
			//
			// The policy is nothingWasSpent, the SAME function the inclusion
			// reconciler decides with, which is why that one does not charge
			// the attempt either. Asking it rather than restating the sentinels
			// is deliberate: two copies of this rule would agree today and
			// drift later, and the claim/proof twins in this file have already
			// proven they drift.
			//
			// What is NOT done here is settling the session as a success. The
			// sentinel's guarantee is weaker than that: "already queued" means
			// this node holds it NOW, not that the chain will include it. So
			// the session stays unsettled and the persist below hands the bytes
			// to the reconciler, which is what verifies inclusion.
			nodeHoldsTheBatch := nothingWasSpent(lastErr)
			if nodeHoldsTheBatch {
				logger.Info().
					Err(lastErr).
					Int("batch_size", len(claimMsgs)).
					Msg("claim not counted as lost: the node reports it already holds the transaction")
			}

			// Mark all sessions as failed due to claim TX error (after exhausting retries)
			for _, snapshot := range groupSnapshots {
				if nodeHoldsTheBatch {
					break
				}
				RecordClaimTxError(snapshot.SupplierOperatorAddress, snapshot.ServiceID, resolvable, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnClaimTxError(ctx, snapshot.SessionID); err != nil && !errors.Is(err, ErrClaimAlreadyOnChain) {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to mark session as claim_tx_error in Redis")
					}
				}
			}

			// Track failed claim submissions to Redis for debugging
			if lc.submissionTracker != nil {
				for i, snapshot := range validSnapshots {
					claimHash := hex.EncodeToString(groupRootHashes[i])
					if trackErr := lc.submissionTracker.TrackClaimSubmission(
						ctx,
						snapshot.SupplierOperatorAddress,
						snapshot.ServiceID,
						snapshot.ApplicationAddress,
						snapshot.SessionID,
						snapshot.SessionStartHeight,
						snapshot.SessionEndHeight,
						claimHash,
						"",    // no TX hash on failure
						false, // failed
						lastErr.Error(),
						earliestClaimHeight,
						lc.blockClient.LastBlock(ctx).Height(),
						snapshot.RelayCount,
						int64(snapshot.TotalComputeUnits),
						false, // proof_required unknown at claim time
						"",    // proof_requirement_seed unknown at claim time
					); trackErr != nil {
						logger.Warn().
							Err(trackErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to track failed claim submission")
					}
				}
			}

			// Self-heal: persist the built claim messages even though submit
			// FAILED, so the InclusionReconciler can re-send them while the claim
			// window is open. The session is now in a terminal claim_tx_error
			// state with no lifecycle retry, so without this a build-OK-but-
			// submit-failed claim (gap / lazyload-at-submit / transient error) is
			// silently forfeited. No tx hash — OrigTxHash="" marks "never
			// confirmed". It does not change when the reconciler resends: every
			// stored entry goes from the block after its submit (canRebroadcast).
			if lc.rebroadcastStore != nil {
				lc.persistRebroadcastEntries(
					// Hash empty and bytes PRESENT is the case this mechanism
					// exists for: the transaction was signed and the send never
					// answered, so nobody knows whether it arrived and
					// re-injecting the same bytes is the only reply that cannot
					// duplicate it. But the pair does NOT prove that case -- a
					// send the chain REFUSED hands its payload back too -- so
					// lastErr travels with the bytes and persistRebroadcastEntry
					// drops them when the chain already judged them.
					ctx, RebroadcastPhaseClaim, validSnapshots, lc.blockClient.LastBlock(ctx).Height(), "",
					claimSigned, lastErr, claimTimeout, claimTimeoutRegime,
					func(i int) ([]byte, error) { return claimMsgs[i].Marshal() },
				)
			}

			groupErrs = append(groupErrs,
				fmt.Errorf("claim submission failed after %d attempts: %w", lc.config.ClaimRetryAttempts, lastErr))
			continue
		}
	}

	return result, errors.Join(groupErrs...)
}

// OnSessionsNeedProof is called when sessions need proofs submitted.
// It waits for the proper timing spread, generates proofs, and submits them.
//
// One failing group no longer ends the cycle. Every group runs, the sessions
// that reached the chain are named in the returned ProofCycleResult, and the
// failures are aggregated into one error. Before this, ten function-level
// returns lived in the group loop: the first to fire abandoned every group
// behind it, and those sessions reached no verdict at all -- no state written,
// no rebroadcast entry, and no lifecycle retry, because `proving` has a single
// exit (proof_timeout -> ProofWindowClosed).
func (lc *LifecycleCallback) OnSessionsNeedProof(ctx context.Context, snapshots []*SessionSnapshot) (ProofCycleResult, error) {
	result := ProofCycleResult{Settled: make(map[string]struct{}, len(snapshots))}
	if len(snapshots) == 0 {
		return result, nil
	}
	// groupErrs accumulates one entry per group that did not settle, so the
	// caller sees every failure instead of only the first.
	var groupErrs []error

	// All sessions for a single supplier, so we can batch them
	firstSnapshot := snapshots[0]
	logger := lc.logger.With().
		Str(logging.FieldSupplier, firstSnapshot.SupplierOperatorAddress).
		Int("batch_size", len(snapshots)).
		Logger()

	// TEST MODE: Delay proof submission to simulate slow proof generation or test window timeout
	if testCfg := getTestConfig(); testCfg.ProofDelaySeconds > 0 {
		logger.Warn().
			Int("delay_seconds", testCfg.ProofDelaySeconds).
			Msg("TEST MODE: delaying proof submission")
		time.Sleep(time.Duration(testCfg.ProofDelaySeconds) * time.Second)
		logger.Warn().
			Int("delay_seconds", testCfg.ProofDelaySeconds).
			Msg("TEST MODE: proof delay complete, continuing with submission")
	}

	logger.Debug().Msg("batched sessions need proofs - starting proof process")

	// One proof per transaction, always. There is no setting for this: a batch
	// dies whole, so a single message the chain refuses takes every other proof
	// in it down -- each of those a forfeited session, and the batch is largest
	// exactly when IsProofRequired fell back to its fail-open branch, which is
	// when its members are likeliest to be the ones refused.
	//
	// The groups are a SLICE, not a map, and that is load-bearing rather than
	// stylistic: Go randomises map iteration, so with a map the order in which
	// proofs reach the chain differs run to run. Ordering by end height alone
	// does not fix it either -- sessions are anchored to a global grid, so in
	// the normal case every session shares one end height and the comparison is
	// a tie. Building in arrival order and sorting with SliceStable makes
	// arrival order the tiebreak, which is what stays fixed across runs.
	groups := groupOnePerSession(snapshots)

	logger.Info().
		Int("total_sessions", len(snapshots)).
		Int("num_transactions", len(groups)).
		Msg("submitting one proof per transaction")

	// Process each group (one session, its own transaction) separately
	for _, groupSnapshots := range groups {
		// Get the actual session end height from the first snapshot in the group
		// (all snapshots in a group have the same end height)
		sessionEndHeight := groupSnapshots[0].SessionEndHeight

		// Shared params effective at this session's height, not live params. After a
		// session-length change (poktroll #543 anchored grid), an old-epoch group
		// computed with new-epoch params would resolve the wrong proof window.
		sharedParams, err := lc.sharedClient.GetParamsAtHeight(ctx, sessionEndHeight)
		if err != nil {
			groupErrs = append(groupErrs,
				fmt.Errorf("failed to get shared params at height %d: %w", sessionEndHeight, err))
			continue
		}

		// Wait for proof window to open
		proofWindowOpenHeight := sharedtypes.GetProofWindowOpenHeight(sharedParams, sessionEndHeight)
		proofWindowCloseHeight := sharedtypes.GetProofWindowCloseHeight(sharedParams, sessionEndHeight)
		currentHeight := lc.blockClient.LastBlock(ctx).Height()

		// Build session IDs list for logging (truncate if too many)
		proofSessionIDs := firstSessionIDsForLog(groupSnapshots)

		logger.Debug().
			Int64("proof_window_open_height", proofWindowOpenHeight).
			Int64("proof_window_close_height", proofWindowCloseHeight).
			Int64("session_end_height", sessionEndHeight).
			Int64("current_height", currentHeight).
			Int("group_size", len(groupSnapshots)).
			Str("first_service_id", groupSnapshots[0].ServiceID).
			Strs("session_ids", proofSessionIDs).
			Msg("waiting for proof window to open")

		// Wait for proof window to open (we'll use the seed block, not this one)
		_, blockErr := lc.waitForBlock(ctx, proofWindowOpenHeight)
		if blockErr != nil {
			groupErrs = append(groupErrs,
				fmt.Errorf("failed to wait for proof window open: %w", blockErr))
			continue
		}

		// Proof requirement seed block height.
		//
		// CRITICAL: the probabilistic proof decision is only valid if this seed is the
		// same block the validator uses. poktroll seeds it with
		// BlockHash(GetEarliestSupplierProofCommitHeight(...) - 1)
		// (x/proof/keeper/msg_server_submit_proof.go getProofRequirementSeedBlockHash),
		// so call that same protocol function rather than restating what it currently
		// returns. It resolves to proofWindowOpenHeight today because proof distribution
		// is disabled upstream; deriving it keeps us correct if that is ever re-enabled,
		// instead of diverging silently on every probabilistic decision.
		//
		// The nil block hash mirrors poktroll: the argument is unused while distribution
		// is disabled, and it deliberately does not read one (consensus hardening).
		// Supplier address comes from this group, like every other value in the loop
		// (sessionEndHeight above): it is the seeding input for the per-supplier spread,
		// so it must belong to the sessions being scheduled rather than rely on the
		// batch-wide "one supplier per call" invariant holding forever.
		earliestProofCommitHeight := sharedtypes.GetEarliestSupplierProofCommitHeight(
			sharedParams,
			sessionEndHeight,
			nil,
			groupSnapshots[0].SupplierOperatorAddress,
		)
		proofRequirementSeedHeight := earliestProofCommitHeight - 1

		// Wait for the seed block to be available
		proofRequirementSeedBlock, seedErr := lc.waitForBlock(ctx, proofRequirementSeedHeight)
		if seedErr != nil {
			logger.Warn().
				Err(seedErr).
				Int64("seed_height", proofRequirementSeedHeight).
				Int64("proof_window_open_height", proofWindowOpenHeight).
				Msg("failed to wait for proof requirement seed block")
			groupErrs = append(groupErrs,
				fmt.Errorf("failed to wait for proof requirement seed block: %w", seedErr))
			continue
		}

		logger.Debug().
			Int64("proof_requirement_seed_height", proofRequirementSeedHeight).
			Str("proof_requirement_seed_hash", fmt.Sprintf("%x", proofRequirementSeedBlock.Hash())).
			Msg("obtained proof requirement seed block hash")

		// Filter sessions based on proof requirement (probabilistic proof selection)
		var sessionsNeedingProof []*SessionSnapshot
		for _, snapshot := range groupSnapshots {
			// CRITICAL: Deduplication check - never submit the same proof twice.
			//
			// The hash is stored when the mempool accepts the transaction
			// (session_coordinator.go:487), so it says the proof was SENT, not
			// that it was included: a transaction accepted and never included
			// leaves this session skipped for good. Inclusion is readable only
			// from the claim -- the proof is deleted in the same EndBlocker that
			// judges it (poktroll v0.1.35 x/proof/module/abci.go:14-24), which
			// writes Claim.ProofValidationStatus (keeper/validate_proofs.go:197).
			// Checking it here is 323b.
			if snapshot.ProofTxHash != "" {
				logger.Warn().
					Str(logging.FieldSessionID, snapshot.SessionID).
					Str("existing_proof_tx_hash", snapshot.ProofTxHash).
					Msg("skipping proof - already submitted for this session (deduplication)")
				continue // Skip this session
			}

			// Pre-proof GetClaim guard (WS-A).
			//
			// The miner records claim_success=true on CheckTx/mempool acceptance, so a
			// session in SessionStateClaimed does NOT guarantee the claim is on-chain.
			// Submitting a proof when the claim is missing triggers an unrecoverable
			// FailedPrecondition ("no claim found for session ID") and burns gas
			// across three retries. Query GetClaim first; if NotFound, drop the session
			// from this batch and mark it terminal. Fail-open on other RPC errors so a
			// flapping chain node does not lose valid proofs.
			if !lc.config.DisablePreProofClaimVerification && lc.proofQueryClient != nil {
				_, claimErr := lc.proofQueryClient.GetClaim(ctx, snapshot.SupplierOperatorAddress, snapshot.SessionID)
				if claimErr != nil && isClaimNotFoundError(claimErr) {
					logger.Warn().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
						Str("claim_tx_hash", snapshot.ClaimTxHash).
						Msg("pre-proof guard: no on-chain claim found for session — skipping proof, marking session claim_missing")
					RecordProofSkipped(snapshot.SupplierOperatorAddress, snapshot.ServiceID, ProofSkippedReasonClaimMissingOnChain)
					if lc.sessionCoordinator != nil {
						if markErr := lc.sessionCoordinator.OnClaimMissing(ctx, snapshot.SessionID); markErr != nil {
							logger.Warn().
								Err(markErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to mark session as claim_missing")
						}
					}
					continue
				}
				if claimErr != nil {
					logger.Warn().
						Err(claimErr).
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("pre-proof guard: GetClaim RPC error — fail-open, proceeding with proof submission")
				}
			}

			if lc.proofChecker != nil {
				required, checkErr := lc.proofChecker.IsProofRequired(ctx, snapshot, proofRequirementSeedBlock.Hash())
				if checkErr != nil {
					// ErrClaimedRootUnreadable means we could not READ the
					// root this block, not that it is gone: the tree is
					// still in Redis and the proof is buildable next block.
					// Defer instead of marking terminal — the claim is
					// already on chain, so a proof that never arrives costs
					// the whole claim plus a flat slash, while another block
					// of waiting costs nothing. The session leaves this
					// batch exactly as it does today, so the healthy
					// sessions in the group are not delayed by it.
					if errors.Is(checkErr, ErrClaimedRootUnreadable) {
						RecordProofSkipped(snapshot.SupplierOperatorAddress, snapshot.ServiceID, ProofSkippedReasonClaimedRootUnreadable)
						logger.Warn().
							Err(checkErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("cannot read claimed root this block; deferring proof to a later block inside the window")
						lc.deferProof(ctx, snapshot)
						continue
					}
					// ErrClaimedRootUnavailable means the session has no
					// authoritative root to anchor a proof on — falling
					// open to submission would produce an on-chain invalid
					// proof. Mark the session as proof_tx_error (terminal
					// failure) so the pipeline doesn't spend gas on a
					// guaranteed reject.
					if errors.Is(checkErr, ErrClaimedRootUnavailable) {
						RecordProofSkipped(snapshot.SupplierOperatorAddress, snapshot.ServiceID, ProofSkippedReasonClaimedRootUnavailable)
						logger.Error().
							Err(checkErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("cannot submit proof: claimed root unavailable (HA failover with failed OnSessionClaimed write); marking session as proof_tx_error")
						if lc.sessionCoordinator != nil {
							if markErr := lc.sessionCoordinator.OnProofTxError(ctx, snapshot.SessionID); markErr != nil {
								logger.Warn().
									Err(markErr).
									Str(logging.FieldSessionID, snapshot.SessionID).
									Msg("failed to mark session as proof_tx_error after unavailable claimed root")
							}
						}
						continue
					}
					logger.Warn().
						Err(checkErr).
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("failed to check proof requirement, submitting proof anyway to avoid potential penalty")
					sessionsNeedingProof = append(sessionsNeedingProof, snapshot)
				} else if !required {
					// Proof NOT required → transition to probabilistic_proved
					logger.Info().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("proof NOT required for this claim - marking as probabilistically proved")
					RecordRevenueProbabilisticProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

					// CRITICAL: Transition session state to probabilistic_proved
					if lc.sessionCoordinator != nil {
						if probErr := lc.sessionCoordinator.OnProbabilisticProved(ctx, snapshot.SessionID); probErr != nil {
							logger.Warn().
								Err(probErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to mark session as probabilistic_proved")
						}
					}
				} else {
					logger.Info().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("proof IS required for this claim")
					sessionsNeedingProof = append(sessionsNeedingProof, snapshot)
				}
			} else {
				// No proof checker, always submit proofs (legacy behavior)
				sessionsNeedingProof = append(sessionsNeedingProof, snapshot)
			}
		}

		if len(sessionsNeedingProof) == 0 {
			logger.Info().
				Int64("session_end_height", sessionEndHeight).
				Msg("no proofs required for this group")
			continue
		}

		// Submit as early as the chain will accept: the protocol's per-supplier spread can
		// push submission close to window close, where it fails. Deliberate choice.
		//
		// earliestProofCommitHeight is what the chain enforces — validateProofWindow
		// rejects anything committed before it — and it is >= proofWindowOpenHeight by
		// construction (poktroll adds a non-negative offset to the window open height).
		// It equals the window open height while proof distribution is disabled upstream,
		// so waiting on it is a no-op today and becomes load-bearing the moment that
		// spread is re-enabled; TestProofDistributionStillDisabled fails loudly then.
		earliestProofHeight := earliestProofCommitHeight
		if _, earliestErr := lc.waitForBlock(ctx, earliestProofHeight); earliestErr != nil {
			groupErrs = append(groupErrs,
				fmt.Errorf("failed to wait for earliest proof commit height: %w", earliestErr))
			continue
		}

		logger.Info().
			Int64("proof_window_open", proofWindowOpenHeight).
			Int64("earliest_proof_commit_height", earliestProofHeight).
			Int64("session_end_height", sessionEndHeight).
			Int("proofs_to_submit", len(sessionsNeedingProof)).
			Msg("proof window open - submitting immediately (timing spread disabled)")

		// The closest-path seed and the proof-requirement seed are the SAME block, by
		// protocol: validateClosestPath hashes GetBlockHash(earliestSupplierProofCommitHeight-1)
		// (x/proof/keeper/proof_validation.go) and getProofRequirementSeedBlockHash uses the
		// same height (x/proof/keeper/msg_server_submit_proof.go). That holds whether or not
		// the distribution spread is enabled, since both read the same function — so there is
		// one seed block here, not two that happen to coincide.
		proofPathSeedBlock := proofRequirementSeedBlock
		logger.Debug().
			Int64("proof_path_seed_block_height", proofRequirementSeedHeight).
			Str("proof_path_seed_block_hash", fmt.Sprintf("%x", proofPathSeedBlock.Hash())).
			Msg("proof path seed block is the proof requirement seed block (same height by protocol)")

		// CRITICAL: Verify proof window is still open before proceeding
		// This prevents wasting fees on proofs that will be rejected
		proofWindowClose := sharedtypes.GetProofWindowCloseHeight(sharedParams, sessionEndHeight)
		currentBlock := lc.blockClient.LastBlock(ctx)
		if currentBlock.Height() >= proofWindowClose {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("proof_window_close", proofWindowClose).
				Int64("session_end_height", sessionEndHeight).
				Int("group_size", len(sessionsNeedingProof)).
				Msg("proof window already closed - cannot submit proofs")

			// Mark all sessions in this group as failed (metrics + Redis state for HA)
			for _, snapshot := range sessionsNeedingProof {
				lc.markAndCountProofWindowClosed(ctx, snapshot)
			}

			groupErrs = append(groupErrs,
				fmt.Errorf("proof window already closed at height %d (current: %d)", proofWindowClose, currentBlock.Height()))
			continue
		}

		// CRITICAL: Re-check proof requirement RIGHT before building proofs
		// Between initial check (at proof window open) and now (at earliestProofHeight),
		// the blockchain might have already settled claims without requiring proofs.
		// This prevents wasting gas on simulation failures.
		// IMPORTANT: MUST use the same seed block as the validator (proofRequirementSeedBlock),
		// NOT proofPathSeedBlock, even though we're at a later height now.
		if lc.proofChecker != nil {
			stillNeedingProof := make([]*SessionSnapshot, 0, len(sessionsNeedingProof))
			for _, snapshot := range sessionsNeedingProof {
				required, recheckErr := lc.proofChecker.IsProofRequired(ctx, snapshot, proofRequirementSeedBlock.Hash())
				if recheckErr != nil {
					// Same split as the initial check: unreadable is deferred,
					// unavailable is terminal.
					if errors.Is(recheckErr, ErrClaimedRootUnreadable) {
						RecordProofSkipped(snapshot.SupplierOperatorAddress, snapshot.ServiceID, ProofSkippedReasonClaimedRootUnreadable)
						logger.Warn().
							Err(recheckErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("cannot read claimed root on re-check; deferring proof to a later block inside the window")
						lc.deferProof(ctx, snapshot)
						continue
					}
					// Same guard as the initial check — a missing claimed
					// root means we'd submit a fabricated proof. Surface
					// as proof_tx_error instead of falling open.
					if errors.Is(recheckErr, ErrClaimedRootUnavailable) {
						RecordProofSkipped(snapshot.SupplierOperatorAddress, snapshot.ServiceID, ProofSkippedReasonClaimedRootUnavailable)
						logger.Error().
							Err(recheckErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("cannot submit proof on re-check: claimed root unavailable; marking session as proof_tx_error")
						if lc.sessionCoordinator != nil {
							if markErr := lc.sessionCoordinator.OnProofTxError(ctx, snapshot.SessionID); markErr != nil {
								logger.Warn().
									Err(markErr).
									Str(logging.FieldSessionID, snapshot.SessionID).
									Msg("failed to mark session as proof_tx_error after unavailable claimed root")
							}
						}
						continue
					}
					// Other errors - err on side of caution and submit anyway
					logger.Warn().
						Err(recheckErr).
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("failed to re-check proof requirement before submission, will attempt submission anyway")
					stillNeedingProof = append(stillNeedingProof, snapshot)
				} else if !required {
					// Proof NO LONGER required - blockchain settled claim without proof
					logger.Info().
						Str(logging.FieldSessionID, snapshot.SessionID).
						Msg("proof not required (blockchain settled claim without proof)")

					RecordRevenueProbabilisticProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)

					if lc.sessionCoordinator != nil {
						if err := lc.sessionCoordinator.OnProbabilisticProved(ctx, snapshot.SessionID); err != nil {
							logger.Warn().
								Err(err).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to mark session as probabilistic_proved")
						}
					}
				} else {
					// Still required - proceed with proof
					stillNeedingProof = append(stillNeedingProof, snapshot)
				}
			}

			if len(stillNeedingProof) == 0 {
				logger.Info().
					Int64("session_end_height", sessionEndHeight).
					Msg("no proofs required after re-check (all claims settled without proof)")
				continue // Skip to next group
			}

			// Update the list to only include sessions that still need proofs
			sessionsNeedingProof = stillNeedingProof
		}

		logger.Debug().
			Int("group_size", len(sessionsNeedingProof)).
			Int64("proof_window_close", proofWindowClose).
			Int64("current_height", currentBlock.Height()).
			Int64("blocks_remaining", proofWindowClose-currentBlock.Height()).
			Msg("proof window timing reached - generating and submitting batched proofs")

		// DEBUG/TEST: Intentionally delay proof submission to test proof expiration tracking
		// Set environment variable TEST_PROOF_DELAY_SECONDS to enable (e.g., "60" for 60 seconds)
		// This simulates scenarios where proof generation takes too long or network delays occur
		if testCfg := getTestConfig(); testCfg.ProofDelaySeconds > 0 {
			delayDuration := time.Duration(testCfg.ProofDelaySeconds) * time.Second
			logger.Warn().
				Int("delay_seconds", testCfg.ProofDelaySeconds).
				Int64("proof_window_close", proofWindowClose).
				Int64("current_height", currentBlock.Height()).
				Msg("TEST MODE: Intentionally delaying proof submission to test expiration tracking")
			time.Sleep(delayDuration)

			// Log current state after delay
			postDelayHeight := lc.blockClient.LastBlock(ctx).Height()
			logger.Warn().
				Int64("height_after_delay", postDelayHeight).
				Int64("proof_window_close", proofWindowClose).
				Bool("window_still_open", postDelayHeight < proofWindowClose).
				Msg("TEST MODE: Delay complete, continuing with proof submission")
		}

		// PARALLEL PROOF BUILDING: Generate proofs and build messages concurrently.
		// Each proof generation is independent and thread-safe, so processing
		// batches of sessions in parallel significantly reduces latency.
		//
		// The result channel is exactly len(sessionsNeedingProof) so every
		// goroutine can write without blocking. The collector below MUST
		// drain every slot — early-return would (a) leak the remaining
		// goroutines permanently and (b) silently drop proofs that built
		// successfully, causing revenue loss when the proof window closes.
		proofResults := make(chan proofBuildResult, len(sessionsNeedingProof))
		numProofTasks := len(sessionsNeedingProof)

		for i, snapshot := range sessionsNeedingProof {
			// Capture loop variables for goroutine
			index := i
			snap := snapshot

			// Submit to bounded build pool if available, otherwise use unbounded goroutine
			buildProofFunc := func() {
				result := proofBuildResult{
					index:    index,
					snapshot: snap,
				}

				// Panic guard: collectProofBuildResults MUST receive exactly
				// numProofTasks values or a goroutine leaks per missing slot.
				// Converts any panic (nil snapshot field, SMST boundary miss,
				// nil seed hash, etc.) into a failed result so the collector
				// drains cleanly. Applies to both pool and fallback goroutine
				// paths because pond absorbs panics without writing our chan.
				defer func() {
					if r := recover(); r != nil {
						logging.PanicRecoveriesTotal.WithLabelValues("proof_build").Inc()
						lc.logger.Error().
							Str(logging.FieldSessionID, snap.SessionID).
							Str(logging.FieldSupplier, snap.SupplierOperatorAddress).
							Str("panic_value", fmt.Sprintf("%v", r)).
							Str("stack_trace", string(debug.Stack())).
							Msg("PANIC RECOVERED in proof build goroutine")
						proofResults <- proofBuildResult{
							index:    index,
							snapshot: snap,
							err:      fmt.Errorf("proof build panic for session %s: %v", snap.SessionID, r),
						}
					}
				}()

				// Record the scheduled proof height for operators
				SetProofScheduledHeight(snap.SupplierOperatorAddress, snap.ServiceID, snap.SessionID, float64(earliestProofHeight))

				// Generate the proof path from the seed block hash
				path := protocol.GetPathForProof(proofPathSeedBlock.Hash(), snap.SessionID)

				lc.logger.Info().
					Str(logging.FieldSessionID, snap.SessionID).
					Str("block_hash_from_blockid", fmt.Sprintf("%x", proofPathSeedBlock.Hash())).
					Str("proof_path_computed", fmt.Sprintf("%x", path)).
					Int64("block_height", proofRequirementSeedHeight).
					Msg("DEBUG: proof path computation (using BlockID.Hash)")

				// Generate the proof (CPU-bound cryptographic operation, can parallelize)
				proofBytes, proofErr := lc.smstManager.ProveClosest(ctx, snap.SessionID, path)
				if proofErr != nil {
					result.err = fmt.Errorf("failed to generate proof for session %s: %w", snap.SessionID, proofErr)
					proofResults <- result
					return
				}

				// Build the session header (network I/O, can parallelize)
				sessionHeader, headerErr := lc.buildSessionHeader(ctx, snap)
				if headerErr != nil {
					result.err = fmt.Errorf("failed to build session header for %s: %w", snap.SessionID, headerErr)
					proofResults <- result
					return
				}

				// Build proof message
				result.proofMsg = &prooftypes.MsgSubmitProof{
					SupplierOperatorAddress: snap.SupplierOperatorAddress,
					SessionHeader:           sessionHeader,
					Proof:                   proofBytes,
				}

				proofResults <- result
			}

			if lc.buildPool != nil {
				lc.buildPool.Submit(buildProofFunc)
			} else {
				go buildProofFunc()
			}
		}

		// Collect every result. Draining ALL numProofTasks values is required
		// even when some goroutines failed — the alternative is a permanent
		// goroutine leak (one per undrained slot) plus silent revenue loss
		// for sessions whose proof built successfully in the same batch.
		partitionedProofs := collectProofBuildResults(numProofTasks, proofResults)

		// Surface per-session build failures as warnings + metrics and keep
		// the batch moving with whatever built. Mirrors the claim path:
		// one bad session does not invalidate the rest of the batch.
		for _, r := range partitionedProofs.failed {
			snap := r.snapshot
			sessionID, supplier, serviceID := "", "", ""
			if snap != nil {
				sessionID = snap.SessionID
				supplier = snap.SupplierOperatorAddress
				serviceID = snap.ServiceID
			}
			RecordProofSkipped(supplier, serviceID, ProofSkippedReasonBuildFailed)
			logger.Warn().
				Err(r.err).
				Str(logging.FieldSessionID, sessionID).
				Str(logging.FieldSupplier, supplier).
				Str(logging.FieldServiceID, serviceID).
				Msg("session proof build failed - dropping from batch, other sessions continue")
		}

		// If every proof in the batch failed to build there is nothing to
		// submit. Surface the first failure (they are already logged above
		// individually) so the caller can meter / retry on the next cycle.
		if len(partitionedProofs.built) == 0 {
			if len(partitionedProofs.failed) > 0 {
				groupErrs = append(groupErrs,
					fmt.Errorf("all proofs in group failed to build (group_size=%d): %w",
						numProofTasks, partitionedProofs.failed[0].err))
			}
			// numProofTasks was zero — nothing to do for THIS group. This was a
			// bare `return nil`, which is the most dangerous shape in the loop:
			// it abandoned every group behind it AND told the caller the cycle
			// succeeded, and the caller answers a nil by marking every session
			// Proved -- including ones it never processed. A session recorded as
			// proved with no proof on-chain is a slash whose ledger says
			// everything is fine.
			continue
		}

		proofMsgs, interfaceProofMsgs, validProofSnapshots := alignProofBatch(partitionedProofs.built)

		// CRITICAL: Re-check window is still open RIGHT before submission
		// Building proofs (proof generation, headers) takes time - blocks may have advanced!
		currentBlock = lc.blockClient.LastBlock(ctx)
		if currentBlock.Height() >= proofWindowClose {
			logger.Error().
				Int64("current_height", currentBlock.Height()).
				Int64("proof_window_close", proofWindowClose).
				Int64("session_end_height", sessionEndHeight).
				Int("batch_size", len(proofMsgs)).
				Msg("proof window closed while building proofs - cannot submit")

			// Mark the sessions that actually built (and therefore would
			// have been submitted) as window-closed. Sessions whose proof
			// build failed are already accounted for via the per-build
			// warning + RecordProofSkipped("build_failed") above.
			for _, snapshot := range validProofSnapshots {
				lc.markAndCountProofWindowClosed(ctx, snapshot)
			}

			groupErrs = append(groupErrs,
				fmt.Errorf("proof window closed while building proofs at height %d (current: %d)", proofWindowClose, currentBlock.Height()))
			continue
		}

		proofBlocksRemaining := proofWindowClose - currentBlock.Height()

		// The whole proof window, for the reason spelled out on the claim side.
		// The two windows are NOT the same length -- poktroll's defaults give 3
		// blocks for claims and 4 for proofs -- so each phase measures its own.
		proofWindowOpen := sharedtypes.GetProofWindowOpenHeight(sharedParams, sessionEndHeight)
		proofTimeout, proofTimeoutRegime := tx.WindowTimeout(
			proofWindowClose-proofWindowOpen,
			lc.config.BlockTimeSeconds,
		)
		proofCtx := tx.WithTxWindowTimeout(ctx, proofTimeout, proofTimeoutRegime)

		logger.Info().
			Int64("current_height", currentBlock.Height()).
			Int64("proof_window_close", proofWindowClose).
			Int64("blocks_remaining", proofBlocksRemaining).
			Dur("tx_deadline", proofTimeout).
			Int("batch_size", len(proofMsgs)).
			Msg("submitting proofs")

		// Count each proof built into this batch (attempts; submitted tracked separately).
		for _, snapshot := range validProofSnapshots {
			RecordProofCreated(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
		}

		// Submit all proofs in a single transaction with retries
		var lastErr error
		// windowClosed records that the batch already reached its terminal state
		// through markAndCount*WindowClosed. lastErr stays set (the submission DID
		// fail, and the tracker and the returned error both need it), so without
		// this the block after the loop counts the SAME sessions a second time --
		// relays_lost_total, compute_units_lost_total and upokt_lost_total doubled
		// for one session -- and overwrites the accurate window-closed state in
		// Redis with the vaguer tx_error one.
		windowClosed := false
		// notRequired exists for the same reason windowClosed does: the batch
		// settled itself per session inside the loop, so the block after it must
		// not settle them a second time with a vaguer verdict.
		notRequired := false
		// proofTxHash and proofSigned are what the LAST attempt returned, for
		// the reason the claim cycle states: the client is shared with the
		// reconciler, so nothing may be read back from it after the call.
		var proofTxHash string
		var proofSigned tx.SignedTxPayload
		for attempt := 1; attempt <= lc.config.ProofRetryAttempts; attempt++ {
			// Re-inject instead of re-signing, for the reason the claim twin
			// states in full. THE PROOF LOOP HAS NO EJECTION -- the chain never
			// names one proof of a batch -- so there is no batch change that
			// could invalidate the bytes here. That asymmetry is written down
			// rather than left to be noticed: a fix applied to one of these two
			// loops and not the other is this file's recurring defect.
			txHash, signed, submitErr := lc.resendOrSignProofs(
				proofCtx, proofWindowClose, proofSigned, lastErr, interfaceProofMsgs)
			proofSigned = signed
			if submitErr != nil {
				lastErr = submitErr

				// The chain refused a proof it says was not required. Terminal
				// for the batch, like the window branch below: the requirement
				// is seeded from a fixed block hash and read at the session's
				// own heights, so a retry asks the same question and gets the
				// same answer while the window burns.
				if errors.Is(submitErr, tx.ErrTxProofNotRequired) {
					lc.settleNotRequiredBatch(ctx, logger, submitErr, validProofSnapshots)
					notRequired = true
					break
				}

				// Check if error is due to proof window being closed (permanent failure - don't retry)
				//
				// The claim path carries the same two-layer check and the same
				// reasoning; see the comment there. Both twins are edited
				// together deliberately: in this file a fix applied to one cycle
				// and not the other has been the recurring defect.
				errorMsg := submitErr.Error()
				if errors.Is(submitErr, tx.ErrTxWindowExpired) ||
					strings.Contains(errorMsg, "proof window") || strings.Contains(errorMsg, "proof_window") {
					logger.Error().
						Err(submitErr).
						Int64("current_height", lc.blockClient.LastBlock(ctx).Height()).
						Int64("proof_window_close", proofWindowClose).
						Int("batch_size", len(proofMsgs)).
						Msg("proof window closed during submission - permanent failure, not retrying")

					// Only mark sessions whose proof actually built (and
					// therefore entered the submit tx) as window-closed. Build
					// failures are already metered above.
					for _, snapshot := range validProofSnapshots {
						lc.markAndCountProofWindowClosed(ctx, snapshot)
					}

					windowClosed = true
					break // Don't retry - this is a permanent failure
				}

				logger.Warn().
					Err(submitErr).
					Int(logging.FieldAttempt, attempt).
					Int(logging.FieldMaxRetry, lc.config.ProofRetryAttempts).
					Int("batch_size", len(proofMsgs)).
					Msg("batched proof submission failed, retrying")

				if attempt < lc.config.ProofRetryAttempts {
					select {
					case <-ctx.Done():
						// The ONE exit that is not a `continue`, and deliberately
						// so: a cancelled context makes every remaining group fail
						// too, so continuing would only burn the rest of the window
						// producing the same error N times. It still returns the
						// sessions already settled instead of discarding them,
						// which is what the bare `return ctx.Err()` did.
						return result, errors.Join(append(groupErrs, ctx.Err())...)
					case <-time.After(lc.config.ProofRetryDelay):
						continue
					}
				}
			} else {
				// The batch SUCCEEDED -- same fix, same reason as the claim
				// cycle. The two loops are twins by construction, lastErr is
				// written in exactly two places in this file and was cleared in
				// neither, so the defect existed on both sides.
				//
				// It lands HARDER here: the success branch below fills
				// result.Settled, so without this the same session is named
				// settled to the caller AND written proof_tx_error in Redis --
				// two contradictory verdicts for one session in one cycle.
				lastErr = nil

				// SUCCESS: Proof TX broadcast accepted to mempool. The hash and
				// the signed bytes are the ones this call returned.
				proofTxHash = txHash

				currentBlock := lc.blockClient.LastBlock(ctx)
				blocksAfterWindowOpen := float64(currentBlock.Height() - proofWindowOpenHeight)

				// CRITICAL: Save TX hash to Redis IMMEDIATELY (1 line after broadcast)
				// This prevents duplicate submissions if we crash after TX broadcast.
				// Iterate only the snapshots actually submitted — build-failed
				// snapshots do not get a proof tx hash.
				for _, snapshot := range validProofSnapshots {
					if lc.sessionCoordinator != nil {
						if updateErr := lc.sessionCoordinator.OnProofSubmitted(ctx, snapshot.SessionID, proofTxHash); updateErr != nil {
							logger.Warn().
								Err(updateErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Str("proof_tx_hash", proofTxHash).
								Msg("failed to store proof TX hash")
						}
					}
				}

				// Record metrics for sessions whose proof was actually submitted.
				for _, snapshot := range validProofSnapshots {
					RecordProofSubmitted(snapshot.SupplierOperatorAddress, snapshot.ServiceID)
					RecordProofSubmissionLatency(snapshot.SupplierOperatorAddress, blocksAfterWindowOpen)
					RecordRevenueProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID, snapshot.TotalComputeUnits, snapshot.RelayCount)
				}

				// Track proof submissions to Redis for debugging. The index
				// into proofMsgs must match validProofSnapshots (both are the
				// built-only ordered set).
				if lc.submissionTracker != nil {
					proofRequirementSeed := hex.EncodeToString(proofRequirementSeedBlock.Hash())
					for i, snapshot := range validProofSnapshots {
						if trackErr := lc.submissionTracker.TrackProofSubmission(
							ctx,
							snapshot.SupplierOperatorAddress,
							snapshot.SessionEndHeight,
							snapshot.SessionID,
							proofMsgs[i].Proof,
							proofTxHash,
							true, // success
							"",   // no error
							earliestProofHeight,
							currentBlock.Height(),
							true,                 // proof was required
							proofRequirementSeed, // seed used for proof requirement check
						); trackErr != nil {
							logger.Warn().
								Err(trackErr).
								Str(logging.FieldSessionID, snapshot.SessionID).
								Msg("failed to track proof submission")
						}
					}
				}

				// Persist each built proof message so the InclusionReconciler can
				// verify on-chain inclusion per block and re-broadcast a
				// still-missing proof while the proof window is open. A code-0
				// BroadcastTx only means mempool acceptance; if the proof misses
				// its block and is never re-sent it is forfeited at settlement
				// (the silent PROOF_MISSING failure mode). The index into
				// proofMsgs matches validProofSnapshots (both the built-only
				// ordered set). Survives leader failover (state lives in Redis).
				if lc.rebroadcastStore != nil && proofTxHash != "" {
					lc.persistRebroadcastEntries(
						ctx, RebroadcastPhaseProof, validProofSnapshots, currentBlock.Height(), proofTxHash, proofSigned, nil,
						proofTimeout, proofTimeoutRegime,
						func(i int) ([]byte, error) { return proofMsgs[i].Marshal() },
					)
				}

				// Name this group's sessions as settled so the caller transitions
				// exactly these to Proved.
				//
				// It is groupSnapshots and not validProofSnapshots ON PURPOSE, and
				// the difference matters: a session whose proof build failed, or
				// which the chain did not require, is in the former and not the
				// latter. Today the caller marks every session it was handed once
				// the callback returns nil, so those sessions are already reaching
				// Proved -- naming only the built ones here would silently change
				// what a whole class of sessions ends up as, inside a commit whose
				// job is to preserve behaviour. Whether Proved is the right state
				// for them is a real question, and it is a SEPARATE one.
				for _, snapshot := range groupSnapshots {
					result.Settled[snapshot.SessionID] = struct{}{}
				}

				logger.Info().
					Int("batch_size", len(proofMsgs)).
					Str("proof_tx_hash", proofTxHash).
					Int64("blocks_after_window", int64(blocksAfterWindowOpen)).
					Msg("batched proofs submitted successfully")

				break // Success, exit retry loop
			}
		}

		if lastErr != nil && !windowClosed && !notRequired {
			// Same reading as the claim side: a store means the self-heal
			// persist below hands these to the reconciler, so the money waits in
			// `unresolved` instead of being declared lost by a submission that
			// may yet land.
			resolvable := lc.rebroadcastStore != nil

			// Same reading as the claim side, and deliberately the same
			// function: a node answering that it already holds this proof has
			// not lost it, so it is not counted as lost and the session is not
			// settled as failed. See the longer note on the claim twin.
			nodeHoldsTheBatch := nothingWasSpent(lastErr)
			if nodeHoldsTheBatch {
				logger.Info().
					Err(lastErr).
					Int("batch_size", len(proofMsgs)).
					Msg("proof not counted as lost: the node reports it already holds the transaction")
			}

			// Mark sessions that entered the tx as failed (the ones that did
			// not build are already counted as build_failed via RecordProofSkipped).
			for _, snapshot := range validProofSnapshots {
				if nodeHoldsTheBatch {
					break
				}
				RecordProofTxError(snapshot.SupplierOperatorAddress, snapshot.ServiceID, resolvable, snapshot.RelayCount, int64(snapshot.TotalComputeUnits))

				// CRITICAL: Update session state in Redis immediately for HA compatibility
				if lc.sessionCoordinator != nil {
					if err := lc.sessionCoordinator.OnProofTxError(ctx, snapshot.SessionID); err != nil {
						logger.Warn().
							Err(err).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to mark session as proof_tx_error in Redis")
					}
				}
			}

			// Track failed proof submissions to Redis for debugging. The index
			// into proofMsgs corresponds to validProofSnapshots by construction.
			if lc.submissionTracker != nil {
				proofRequirementSeed := hex.EncodeToString(proofRequirementSeedBlock.Hash())
				for i, snapshot := range validProofSnapshots {
					if trackErr := lc.submissionTracker.TrackProofSubmission(
						ctx,
						snapshot.SupplierOperatorAddress,
						snapshot.SessionEndHeight,
						snapshot.SessionID,
						proofMsgs[i].Proof,
						"",    // no TX hash on failure
						false, // failed
						lastErr.Error(),
						earliestProofHeight,
						lc.blockClient.LastBlock(ctx).Height(),
						true,                 // proof was required (we attempted submission)
						proofRequirementSeed, // seed used for proof requirement check
					); trackErr != nil {
						logger.Warn().
							Err(trackErr).
							Str(logging.FieldSessionID, snapshot.SessionID).
							Msg("failed to track failed proof submission")
					}
				}
			}

			// Self-heal: persist the built proof messages even though submit
			// FAILED, so the InclusionReconciler can re-send them while the proof
			// window is open. The session is now in a terminal proof_tx_error
			// state with no lifecycle retry, so without this a build-OK-but-
			// submit-failed proof (gap / lazyload-at-submit / transient error) is
			// silently forfeited. OrigTxHash="" marks "never confirmed". It does
			// not change when the reconciler resends: every stored entry goes from
			// the block after its submit (canRebroadcast).
			if lc.rebroadcastStore != nil {
				lc.persistRebroadcastEntries(
					// lastErr travels with the bytes for the reason the claim
					// twin states: a refusal also hands its payload back.
					ctx, RebroadcastPhaseProof, validProofSnapshots, lc.blockClient.LastBlock(ctx).Height(), "",
					proofSigned, lastErr, proofTimeout, proofTimeoutRegime,
					func(i int) ([]byte, error) { return proofMsgs[i].Marshal() },
				)
			}

			groupErrs = append(groupErrs,
				fmt.Errorf("proof submission failed after %d attempts: %w", lc.config.ProofRetryAttempts, lastErr))
			continue
		}
	}

	return result, errors.Join(groupErrs...)
}

// OnSessionProved is called when a session proof is successfully submitted.
// It cleans up resources associated with the session.
func (lc *LifecycleCallback) OnSessionProved(ctx context.Context, snapshot *SessionSnapshot) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Debug().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("session proved - cleaning up")

	// Record session proved metrics
	RecordSessionProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID)

	// Clean up session-specific metrics (gauges with session_id label)
	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	// Clean up SMST
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree")
	}

	// Update snapshot manager
	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnSessionProved(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to update snapshot after proof")
		}
	}

	// Clean up deduplication local cache entries for this session.
	// This prevents unbounded memory growth in the deduplicator's local cache.
	if lc.deduplicator != nil {
		if err := lc.deduplicator.CleanupSession(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to cleanup deduplication entries")
		}
	}

	// Remove session lock

	return nil
}

// OnClaimSkipped is called when the economic viability check rejected a
// session at claim time. It transitions the session to the terminal
// ClaimSkipped state and cleans up local resources (SMST tree, stream,
// dedup cache) the same way a successful claim path would.
func (lc *LifecycleCallback) OnClaimSkipped(ctx context.Context, snapshot *SessionSnapshot) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Debug().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("claim skipped for economic reasons - cleaning up")

	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree on claim_skipped")
	}

	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnClaimSkipped(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to update coordinator on claim_skipped")
			return err
		}
	}

	if lc.deduplicator != nil {
		if err := lc.deduplicator.CleanupSession(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to cleanup deduplication on claim_skipped")
		}
	}

	return nil
}

// OnProbabilisticProved is called when a session is probabilistically proved (no proof required).
// It cleans up resources associated with the session.
func (lc *LifecycleCallback) OnProbabilisticProved(ctx context.Context, snapshot *SessionSnapshot) error {
	logger := lc.logger.With().Str(logging.FieldSessionID, snapshot.SessionID).Logger()

	logger.Debug().
		Int64(logging.FieldCount, snapshot.RelayCount).
		Msg("session probabilistically proved (no proof required) - cleaning up")

	// Record session outcome
	RecordSessionProbabilisticProved(snapshot.SupplierOperatorAddress, snapshot.ServiceID)

	// Clean up session-specific metrics (gauges with session_id label)
	ClearSessionMetrics(snapshot.SupplierOperatorAddress, snapshot.SessionID, snapshot.ServiceID)

	// Clean up SMST tree
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		logger.Warn().Err(err).Msg("failed to delete SMST tree")
	}

	// Update snapshot manager
	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnProbabilisticProved(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to update session coordinator")
			return err
		}
	}

	// Clean up deduplication local cache entries for this session.
	// This prevents unbounded memory growth in the deduplicator's local cache.
	if lc.deduplicator != nil {
		if err := lc.deduplicator.CleanupSession(ctx, snapshot.SessionID); err != nil {
			logger.Warn().Err(err).Msg("failed to cleanup deduplication entries")
		}
	}

	// Remove session lock

	return nil
}

// markAndCountClaimWindowClosed marks a session claim_window_closed and records
// the loss ONLY IF the mark took.
//
// Order matters. OnClaimWindowClosed returns early when its Redis UpdateState
// fails, before firing the terminal callback that removes the session from the
// lifecycle's tracking -- so on that error the session stays in claiming, the
// sweep transitions it on a later pass, and the callback below records it
// there. Recording here as well would count the same relays, compute units and
// uPOKT twice on every Redis blip during a window abort. Both failing leaves it
// under-counted, which is the direction this codebase already chose elsewhere.
//
// That covers the double-count reachable from HERE. There is a second one, on
// the CALLER's side, and it needed its own guard: the submit loop breaks out of
// its retries with lastErr still set -- it has to, the tracker and the returned
// error both read it -- so the "if lastErr != nil" block after the loop used to
// record the very same snapshots again as a tx error. The windowClosed flag at
// the two call sites is what stops it; without it this function's careful
// ordering was undone one frame up the stack.
// ObserveClaimOnChain asks the chain whether this session already has a claim,
// and if it does, books the session claimed with the claim's root -- the edge
// the inclusion reconciler uses, OnClaimObservedOnChain -- and reports true.
//
// It exists for a session that reached claiming and is about to be booked
// claim_window_closed: a process killed after broadcasting a claim and before
// storing its hash leaves such a session with no hash and no rebroadcast entry,
// so nothing local knows the claim is on chain. Booking it failed would delete
// its tree while the chain waits for its proof. The chain says which one is
// true, keyed by (supplier, session), which is all a claim is keyed by.
//
// NotFound is an answer (false, nil). Any other error is NOT: the caller must
// leave the session as it is and ask again, never make it terminal on a
// question the chain did not answer.
func (lc *LifecycleCallback) ObserveClaimOnChain(ctx context.Context, snapshot *SessionSnapshot) (bool, error) {
	if lc.proofQueryClient == nil || lc.sessionCoordinator == nil {
		return false, nil
	}
	claim, err := lc.proofQueryClient.GetClaim(ctx, snapshot.SupplierOperatorAddress, snapshot.SessionID)
	if err != nil {
		if isClaimNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("querying the claim of session %s: %w", snapshot.SessionID, err)
	}
	if err := lc.sessionCoordinator.OnClaimObservedOnChain(ctx, snapshot.SessionID, claim.GetRootHash(), snapshot.ClaimTxHash); err != nil {
		return false, fmt.Errorf("booking the claim of session %s observed on chain: %w", snapshot.SessionID, err)
	}
	lc.logger.Info().
		Str(logging.FieldSessionID, snapshot.SessionID).
		Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
		Str(logging.FieldServiceID, snapshot.ServiceID).
		Msg("claim window closing on a session whose claim is already on chain: booked claimed instead of failed")
	return true, nil
}

func (lc *LifecycleCallback) markAndCountClaimWindowClosed(ctx context.Context, snapshot *SessionSnapshot, claimWindowClose int64) {
	// A session that reached claiming may have its claim on chain already (see
	// ObserveClaimOnChain). If the chain cannot say, the session is left as it
	// is: the lifecycle's next pass asks again.
	observed, err := lc.ObserveClaimOnChain(ctx, snapshot)
	if err != nil {
		lc.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, snapshot.SessionID).
			Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
			Msg("could not ask the chain for the session's claim: not booked claim_window_closed, asking again next pass")
		return
	}
	if observed {
		return
	}
	// A "no claim" read before close+1 can still be overtaken by a claim in
	// block close (claimReadIsFinal). The session stays as it is and the
	// lifecycle's sweep books it once the read is final.
	if !claimReadIsFinal(lc.blockClient.LastBlock(ctx).Height(), claimWindowClose) {
		return
	}
	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnClaimWindowClosed(ctx, snapshot.SessionID); err != nil {
			if errors.Is(err, ErrClaimAlreadyOnChain) {
				// Booked claimed by the reconciler in between: not a failure.
				return
			}
			lc.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
				Str(logging.FieldServiceID, snapshot.ServiceID).
				Msg("failed to mark session as claim_window_closed in Redis")
			return
		}
	}
	RecordClaimWindowClosed(
		snapshot.SupplierOperatorAddress,
		snapshot.ServiceID,
		snapshot.ClaimTxHash,
		snapshot.RelayCount,
		int64(snapshot.TotalComputeUnits),
	)
}

// deferProof returns a session to claimed after a proof attempt was
// abandoned for a reason that will not still hold next block, so the
// per-block transition engine tries it again until the proof window closes.
//
// Both halves are needed and the order matters. Redis is what a failover
// leader reads, and the in-memory snapshot is what checkSessionTransition
// reads on the next block: a session left at proving in memory can only
// leave through proof_window_closed, which is the same money lost. The
// snapshot pointer is the one activeSessions holds (session_lifecycle.go
// hands it to the callback and mutates it the same way right after this
// call returns), and this runs on that same goroutine.
//
// If the Redis write fails the snapshot is left alone on purpose: the
// session then ages out through proof_window_closed, which at least counts
// the loss. Same rule as resumeUnsentSubmission -- when the write does not
// land, the session stays as it was.
func (lc *LifecycleCallback) deferProof(ctx context.Context, snapshot *SessionSnapshot) {
	if lc.sessionCoordinator == nil {
		return
	}
	if err := lc.sessionCoordinator.OnProofDeferred(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, snapshot.SessionID).
			Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
			Msg("failed to return session to claimed after deferring the proof; it will age out at proof window close")
		return
	}
	snapshot.State = SessionStateClaimed
}

// markAndCountProofWindowClosed is markAndCountClaimWindowClosed one window later.
func (lc *LifecycleCallback) markAndCountProofWindowClosed(ctx context.Context, snapshot *SessionSnapshot) {
	if lc.sessionCoordinator != nil {
		if err := lc.sessionCoordinator.OnProofWindowClosed(ctx, snapshot.SessionID); err != nil {
			lc.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Str(logging.FieldSupplier, snapshot.SupplierOperatorAddress).
				Str(logging.FieldServiceID, snapshot.ServiceID).
				Msg("failed to mark session as proof_window_closed in Redis")
			return
		}
	}
	RecordProofWindowClosed(
		snapshot.SupplierOperatorAddress,
		snapshot.ServiceID,
		snapshot.ProofTxHash,
		snapshot.RelayCount,
		int64(snapshot.TotalComputeUnits),
	)
}

// OnClaimWindowClosed is called when a session fails due to claim window timeout.

func (lc *LifecycleCallback) OnClaimWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on claim window closed")
	}

	// Record the loss HERE, not at the caller. This callback is reached only
	// from the lifecycle sweep (SessionLifecycleManager.executeTransition),
	// which decides the timeout purely on height and recorded nothing -- so a
	// session that simply ran out of window had its tree deleted and its lock
	// released while sessions_failed_total, relays_lost_total,
	// compute_units_lost_total and upokt_lost_total all stayed silent. The
	// pre-submission aborts take a different route and cannot double count,
	// for two independent reasons: marking the session terminal through the
	// coordinator fires the terminal callback, which calls
	// SessionLifecycleManager.RemoveSession and takes it out of the sweep's
	// tracking altogether; and determineTransition has no case for a session
	// already in claim_window_closed, so it would produce no transition even
	// if it were still tracked.
	//
	// The sweep persists the terminal state BEFORE calling this, and that order
	// is the other half of "exactly once": a session counted here whose state
	// did not reach Redis is re-loaded as non-terminal by whoever takes the
	// supplier over, swept again, and counted again. See executeTransition.
	RecordClaimWindowClosed(
		snapshot.SupplierOperatorAddress,
		snapshot.ServiceID,
		snapshot.ClaimTxHash,
		snapshot.RelayCount,
		int64(snapshot.TotalComputeUnits),
	)

	return nil
}

// OnClaimTxError is called when a session fails due to claim transaction error.
func (lc *LifecycleCallback) OnClaimTxError(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on claim tx error")
	}

	// Metrics already recorded at failure point, just cleanup
	return nil
}

// OnProofWindowClosed is called when a session fails due to proof window timeout.
func (lc *LifecycleCallback) OnProofWindowClosed(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on proof window closed")
	}

	// Same gap as OnClaimWindowClosed above, one window later: the sweep is the
	// only route here and it recorded nothing. The same ordering applies -- the
	// state is persisted before this runs, so a handover cannot count it twice.
	RecordProofWindowClosed(
		snapshot.SupplierOperatorAddress,
		snapshot.ServiceID,
		snapshot.ProofTxHash,
		snapshot.RelayCount,
		int64(snapshot.TotalComputeUnits),
	)

	return nil
}

// OnProofTxError is called when a session fails due to proof transaction error.
func (lc *LifecycleCallback) OnProofTxError(ctx context.Context, snapshot *SessionSnapshot) error {
	// Clean up SMST tree to prevent memory leak
	if err := lc.smstManager.DeleteTree(ctx, snapshot.SessionID); err != nil {
		lc.logger.Warn().Err(err).Str("session_id", snapshot.SessionID).Msg("failed to delete SMST tree on proof tx error")
	}

	// Metrics already recorded at failure point, just cleanup
	return nil
}

// waitForBlock waits for a specific block height to be reached using event-driven
// block notifications. This is more efficient than polling and doesn't block workers.
func (lc *LifecycleCallback) waitForBlock(ctx context.Context, targetHeight int64) (pocktclient.Block, error) {
	startTime := time.Now()

	// Check if we're already at or past the target height
	currentBlock := lc.blockClient.LastBlock(ctx)
	currentHeight := currentBlock.Height()

	if currentHeight >= targetHeight {
		// Already at target height - no wait needed
		lc.logger.Debug().
			Int64("target_height", targetHeight).
			Int64("current_height", currentHeight).
			Dur("elapsed_ms", time.Since(startTime)).
			Msg("waitForBlock: already at target height (no wait)")
		return lc.getBlockAtHeight(ctx, targetHeight)
	}

	// BLOCKING WAIT DETECTED - this will block until target height
	blocksToWait := targetHeight - currentHeight
	lc.logger.Warn().
		Int64("target_height", targetHeight).
		Int64("current_height", currentHeight).
		Int64("blocks_to_wait", blocksToWait).
		Msg("BLOCKING: waitForBlock starting - waiting for future block")

	// Try to use event-driven approach with Subscribe()
	subscriber, ok := lc.blockClient.(interface {
		Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock
	})
	if !ok {
		// Fallback to polling if Subscribe() not available (shouldn't happen in production)
		lc.logger.Warn().
			Int64("target_height", targetHeight).
			Msg("block client does not support Subscribe(), falling back to polling")
		return lc.waitForBlockPolling(ctx, targetHeight)
	}

	// Subscribe to block events - use small buffer since we only need to detect one block
	blockCh := subscriber.Subscribe(ctx, 10)

	for {
		select {
		case <-ctx.Done():
			lc.logger.Warn().
				Int64("target_height", targetHeight).
				Dur("elapsed_ms", time.Since(startTime)).
				Msg("waitForBlock: context cancelled while waiting")
			return nil, ctx.Err()

		case block, ok := <-blockCh:
			if !ok {
				// Channel closed, fall back to polling
				lc.logger.Warn().
					Int64("target_height", targetHeight).
					Msg("block subscription channel closed, falling back to polling")
				return lc.waitForBlockPolling(ctx, targetHeight)
			}

			if block.Height() >= targetHeight {
				elapsed := time.Since(startTime)
				lc.logger.Info().
					Int64("target_height", targetHeight).
					Int64("reached_height", block.Height()).
					Int64("blocks_waited", block.Height()-currentHeight).
					Dur("elapsed_ms", elapsed).
					Msg("waitForBlock: target height reached")
				return lc.getBlockAtHeight(ctx, targetHeight)
			}
		}
	}
}

// waitForBlockPolling is the fallback polling approach (only used if Subscribe unavailable).
func (lc *LifecycleCallback) waitForBlockPolling(ctx context.Context, targetHeight int64) (pocktclient.Block, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			currentBlock := lc.blockClient.LastBlock(ctx)
			if currentBlock.Height() >= targetHeight {
				return lc.getBlockAtHeight(ctx, targetHeight)
			}

			lc.logger.Debug().
				Int64("current_height", currentBlock.Height()).
				Int64("target_height", targetHeight).
				Msg("waiting for block height (polling fallback)")
		}
	}
}

// getBlockAtHeight fetches the specific block at targetHeight.
// CRITICAL: We must query the exact block to get the correct BlockID.Hash for proof validation.
func (lc *LifecycleCallback) getBlockAtHeight(ctx context.Context, targetHeight int64) (pocktclient.Block, error) {
	blockSubscriber, ok := lc.blockClient.(interface {
		GetBlockAtHeight(context.Context, int64) (pocktclient.Block, error)
	})
	if !ok {
		return nil, fmt.Errorf("BlockClient doesn't support GetBlockAtHeight - proof generation will fail (target_height=%d)", targetHeight)
	}

	block, err := blockSubscriber.GetBlockAtHeight(ctx, targetHeight)
	if err != nil {
		return nil, fmt.Errorf("failed to get block at height %d: %w", targetHeight, err)
	}
	return block, nil
}

// buildSessionHeader builds a session header from the snapshot.
// It queries the session from the blockchain to get complete information.
func (lc *LifecycleCallback) buildSessionHeader(ctx context.Context, snapshot *SessionSnapshot) (*sessiontypes.SessionHeader, error) {
	if lc.sessionClient != nil {
		// Query the session from the blockchain to get the complete header
		session, err := lc.sessionClient.GetSession(
			ctx,
			snapshot.ApplicationAddress,
			snapshot.ServiceID,
			snapshot.SessionStartHeight,
		)
		if err != nil {
			lc.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, snapshot.SessionID).
				Msg("failed to query session from blockchain, using snapshot data")
		} else if session != nil {
			return session.Header, nil
		}
	}

	// Fallback: build from snapshot data
	return &sessiontypes.SessionHeader{
		SessionId:               snapshot.SessionID,
		ApplicationAddress:      snapshot.ApplicationAddress,
		ServiceId:               snapshot.ServiceID,
		SessionStartBlockHeight: snapshot.SessionStartHeight,
		SessionEndBlockHeight:   snapshot.SessionEndHeight,
	}, nil
}

// Ensure LifecycleCallback implements SessionLifecycleCallback
var _ SessionLifecycleCallback = (*LifecycleCallback)(nil)
