package miner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// SessionCreatedCallback is called when a new session is created.
// This allows external components (like SessionLifecycleManager) to be notified.
type SessionCreatedCallback func(ctx context.Context, snapshot *SessionSnapshot) error

// SessionTerminalCallback is called when a session transitions to a terminal state.
// This allows external components (like SessionLifecycleManager) to update in-memory state
// atomically with Redis updates.
type SessionTerminalCallback func(sessionID string, state SessionState)

// SMSTRecoveryConfig contains configuration for session recovery.
type SMSTRecoveryConfig struct {
	// SupplierAddress is the supplier this recovery service is for.
	SupplierAddress string

	// RecoveryTimeout is the maximum time allowed for recovery.
	RecoveryTimeout time.Duration
}

// SessionCoordinator manages session lifecycle events (creation, relay tracking).
// This replaces SMSTSnapshotManager which was tightly coupled to the now-removed WAL.
type SessionCoordinator struct {
	logger       logging.Logger
	sessionStore SessionStore
	config       SMSTRecoveryConfig

	// onSessionCreated is called when a new session is created
	onSessionCreated SessionCreatedCallback

	// onSessionTerminal is called when a session transitions to a terminal state.
	// This allows in-memory state to be updated atomically with Redis.
	onSessionTerminal SessionTerminalCallback

	// claimWindowClosedFn reports whether the claim window for a session ending
	// at the given height has already closed. Injected rather than built from a
	// block and a params client so this type keeps its two dependencies; nil
	// where the miner runs without chain clients (tooling, tests), in which
	// case ClaimWindowClosed answers false and nothing is dropped.
	claimWindowClosedFn func(sessionEndHeight int64) bool

	mu     sync.Mutex
	closed bool
}

// NewSessionCoordinator creates a new session coordinator.
func NewSessionCoordinator(
	logger logging.Logger,
	sessionStore SessionStore,
	config SMSTRecoveryConfig,
) *SessionCoordinator {
	return &SessionCoordinator{
		logger:       logging.ForSupplierComponent(logger, logging.ComponentSMSTSnapshot, config.SupplierAddress),
		sessionStore: sessionStore,
		config:       config,
	}
}

// SetOnSessionCreatedCallback sets the callback to be invoked when a new session is created.
func (c *SessionCoordinator) SetOnSessionCreatedCallback(callback SessionCreatedCallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSessionCreated = callback
}

// SetClaimWindowClosedFn installs the predicate behind ClaimWindowClosed.
func (c *SessionCoordinator) SetClaimWindowClosedFn(fn func(sessionEndHeight int64) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claimWindowClosedFn = fn
}

// ClaimWindowClosed reports whether the claim window for a session ending at
// sessionEndHeight has ALREADY CLOSED, so the session can no longer be
// claimed. It answers FALSE whenever it cannot tell -- no predicate
// wired, or a height that carries no information -- because the only caller
// uses it to discard a relay, and discarding on an unknown is how served work
// stops being paid for.
func (c *SessionCoordinator) ClaimWindowClosed(sessionEndHeight int64) bool {
	if sessionEndHeight <= 0 {
		return false
	}
	c.mu.Lock()
	fn := c.claimWindowClosedFn
	c.mu.Unlock()
	if fn == nil {
		return false
	}
	return fn(sessionEndHeight)
}

// SetOnSessionTerminalCallback sets the callback to be invoked when a session transitions to terminal state.
// This allows SessionLifecycleManager to update in-memory state atomically with Redis updates.
func (c *SessionCoordinator) SetOnSessionTerminalCallback(callback SessionTerminalCallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSessionTerminal = callback
}

// SessionRead is what a caller already learned about a session from the store,
// handed to EnsureSession so it does not read the same session again.
//
// The zero value means the caller learned nothing -- it did not read, or its
// read failed -- and EnsureSession reads for itself. The two must not be
// confused: "the read failed" says nothing about the session, while "the store
// answered and there is no snapshot" means it does not exist and is created.
type SessionRead struct {
	// Snapshot is what the store returned; nil with Answered means absent.
	Snapshot *SessionSnapshot
	// Answered is true when the store answered the read without an error.
	Answered bool
}

// EnsureSession creates the session snapshot if it does not exist yet.
//
// read carries the caller's own read of the session, when it made one; see
// SessionRead. The relay path makes no read of its own and passes the zero
// value; it calls this only until it reports the session exists (handleRelay).
//
// It is separate from OnRelayProcessed because the two have different gates.
// Counting a relay must happen exactly once — a relay counted twice skews the
// relay_count the claim-time comparison with the tree's leaves reads — so the
// caller gates it on the deduplicator. Creating the session
// must happen on EVERY delivery, because a redelivery can be the only chance
// left to do it: the consumer that first processed the relay can die between
// MarkProcessed and telling the coordinator anything, and then the consumer
// that reclaims the message finds the hash already marked and drops it.
// Skipping creation there leaves the SMST holding relays that no snapshot
// claims, and the work goes unpaid.
//
// Running it on every delivery is safe and nearly free: OnSessionCreated goes
// through CreateIfAbsent, a first-write-wins gate, so N callers racing on one
// fresh sessionID produce exactly one snapshot, one callback and one metric.
//
// Failures are logged, not returned: the caller's relay is already in the SMST
// and must be ACKed either way.
//
// exists reports whether the session is known to exist when this returns: the
// store answered with its snapshot, or CreateIfAbsent created it or found it
// there. false says nothing about the session -- a closed coordinator, missing
// metadata, or a read and a create that both failed -- and the caller must ask
// again next time.
func (c *SessionCoordinator) EnsureSession(
	ctx context.Context,
	read SessionRead,
	sessionID string,
	supplierAddress, serviceID, applicationAddress string,
	sessionStartHeight, sessionEndHeight int64,
) (exists bool) {
	// Same guard as its siblings. Without it a relay still in flight at
	// shutdown does a Redis Get, then OnSessionCreated returns "closed", and
	// the failure is logged Warn once PER RELAY — a per-request Warn, which
	// the logging policy reserves for state changes.
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false
	}

	// The read is an optimisation that skips the CreateIfAbsent round-trip on
	// the hot path where the session already exists; correctness does not
	// depend on it. It is made here only when the caller did not make one.
	snapshot := read.Snapshot
	if !read.Answered {
		var err error
		snapshot, err = c.sessionStore.Get(ctx, sessionID)
		if err != nil {
			c.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("failed to check session existence")
		}
	}
	if snapshot != nil {
		return true
	}

	if supplierAddress == "" || serviceID == "" {
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Msg("session not found and missing metadata to create it")
		return false
	}
	if err := c.OnSessionCreated(ctx, sessionID, supplierAddress, serviceID,
		applicationAddress, sessionStartHeight, sessionEndHeight); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to create session")
		return false
	}
	return true
}

// OnRelayProcessed should be called when a relay is successfully processed and
// added to SMST. It updates the session's relay count.
//
// It does NOT create the session: EnsureSession does, and the caller must have
// called it. The two are split because their gates differ — creation runs on
// every delivery, counting only on the first — and merging them again would
// either drop a session on a redelivery or count a relay twice.
//
// Concurrency contract, carried by EnsureSession: N concurrent creators for the
// same brand-new session result in EXACTLY ONE OnSessionCreated invocation (and
// therefore exactly one TrackSession / RecordSessionCreated). The
// first-write-wins gate lives in SessionStore.CreateIfAbsent; OnSessionCreated
// only fires the registered callback when CreateIfAbsent reports that this
// caller is the creator. Without that gate the old Get→nil→Save path was a
// TOCTOU: concurrent relays all saw snapshot == nil, all called Save, and could
// race on HSET(full) / HSET(metadata), potentially losing HINCRBY-tracked
// counters and duplicating lifecycle registration.
func (c *SessionCoordinator) OnRelayProcessed(
	ctx context.Context,
	sessionID string,
	computeUnits uint64,
	supplierAddress, serviceID, applicationAddress string,
	sessionStartHeight, sessionEndHeight int64,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Update session relay count (critical for claim submission).
	// Terminal state errors are expected for late relays — log and continue.
	// All other errors (Redis failures) are propagated so the relay is retried.
	if err := c.sessionStore.IncrementRelayCount(ctx, sessionID, computeUnits); err != nil {
		if errors.Is(err, ErrSessionTerminal) {
			c.logger.Info().
				Str(logging.FieldSessionID, sessionID).
				Msg("relay arrived for terminal session, skipping count update")
			return nil
		}
		return fmt.Errorf("failed to update session relay count: %w", err)
	}

	return nil
}

// OnSessionCreated should be called when a new session is created.
//
// This method is the authoritative session-create entry point. It uses
// SessionStore.CreateIfAbsent as a first-write-wins gate so that N
// concurrent callers racing on the same fresh sessionID produce EXACTLY
// ONE callback invocation and EXACTLY ONE RecordSessionCreated metric.
// Callers that lose the race get (nil error, no callback) and proceed as
// if the session already existed.
func (c *SessionCoordinator) OnSessionCreated(
	ctx context.Context,
	sessionID string,
	supplierAddress, serviceID, applicationAddress string,
	startHeight, endHeight int64,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Create session snapshot in Redis — first writer wins.
	snapshot := &SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: supplierAddress,
		ServiceID:               serviceID,
		ApplicationAddress:      applicationAddress,
		SessionStartHeight:      startHeight,
		SessionEndHeight:        endHeight,
		State:                   SessionStateActive,
		RelayCount:              0,
		TotalComputeUnits:       0,
		ClaimedRootHash:         nil,
	}

	created, err := c.sessionStore.CreateIfAbsent(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("failed to create session snapshot: %w", err)
	}

	if !created {
		// Another caller won the race; the session already exists.
		// Do NOT fire the session-created callback or metric again.
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSupplier, supplierAddress).
			Msg("session already existed; skipping duplicate create callback")
		return nil
	}

	c.logger.Info().
		Str(logging.FieldSessionID, sessionID).
		Str(logging.FieldService, serviceID).
		Str(logging.FieldSupplier, supplierAddress).
		Int64("start_height", startHeight).
		Int64("end_height", endHeight).
		Msg("session created on first relay")

	// Record session creation metric once, only on the winning path.
	RecordSessionCreated(supplierAddress, serviceID)

	// Invoke callback if registered (first-write-wins guarantees exactly once).
	c.mu.Lock()
	callback := c.onSessionCreated
	c.mu.Unlock()

	if callback != nil {
		if err := callback(ctx, snapshot); err != nil {
			c.logger.Warn().
				Err(err).
				Str(logging.FieldSessionID, sessionID).
				Msg("session created callback failed")
		}
	}

	return nil
}

// OnSessionClaimed should be called when a session's claim is submitted.
// It updates the session state to claimed and stores the claim root hash and TX hash.
func (c *SessionCoordinator) OnSessionClaimed(
	ctx context.Context,
	sessionID string,
	claimRootHash []byte,
	claimTxHash string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Get current snapshot
	snapshot, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session snapshot: %w", err)
	}
	if snapshot == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Update with claim root hash and TX hash
	snapshot.ClaimedRootHash = claimRootHash
	snapshot.ClaimTxHash = claimTxHash
	snapshot.State = SessionStateClaimed

	if err := c.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session snapshot: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Str("claim_tx_hash", claimTxHash).
		Int("root_hash_len", len(claimRootHash)).
		Msg("session claimed")

	return nil
}

// ErrClaimRootUnusable reports that a claim observed on-chain came with a root
// that cannot be stored or proved from. It is PERMANENT — a malformed root does
// not become well-formed on the next block — so callers must stop retrying
// rather than hold the observation open until it times out.
var ErrClaimRootUnusable = errors.New("claimed root hash is unusable")

// OnClaimObservedOnChain is called when the InclusionReconciler has OBSERVED
// this session's claim on-chain — which can happen after the broadcast
// reported failure and the session was already marked claim_tx_error.
//
// That combination is not academic: it is the path that turns a lost reward
// into a SLASH. The self-heal persists the built MsgCreateClaim on any
// broadcast failure, the reconciler re-sends it, the claim lands, and without
// this edge nobody tells the lifecycle — so the session stays terminal, the
// proof never goes out, and the chain penalises a claim we did submit.
//
// A state is terminal only when it rests on an OBSERVATION of the chain.
// claim_tx_error rests on a broadcast REPORT, and a report can be wrong; this
// method is where the observation overrides it.
//
// The write is a single guarded round-trip (see ReactivateClaimed), and the
// snapshot is RE-READ afterwards before re-tracking: TrackSession saves what
// it is handed, and Save HDELs empty optional fields, so handing it anything
// but the stored snapshot would erase the claim fields just written.
func (c *SessionCoordinator) OnClaimObservedOnChain(
	ctx context.Context,
	sessionID string,
	claimedRootHash []byte,
	claimTxHash string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	createdCallback := c.onSessionCreated
	c.mu.Unlock()

	// Without a well-formed root the session is unprovable, and it fails LATE
	// and quietly: decodeSnapshot drops a wrong-length root (it would panic the
	// smt library on import), so the session would come back `claimed` with no
	// root, resolveClaimedRoot would fall back to the SMST, and a rehydration
	// miss ends in proof_tx_error. Refuse loudly instead of reactivating
	// something that cannot produce a proof.
	if len(claimedRootHash) != SMSTRootLen {
		return fmt.Errorf(
			"%w: session %s root is %d bytes, want %d",
			ErrClaimRootUnusable, sessionID, len(claimedRootHash), SMSTRootLen,
		)
	}

	reactivated, err := c.sessionStore.ReactivateClaimed(ctx, sessionID, claimedRootHash, claimTxHash)
	if err != nil {
		return fmt.Errorf("failed to reactivate session %s: %w", sessionID, err)
	}
	if !reactivated {
		// Already at or past claimed. Not an error and not a no-op worth
		// logging above Debug: a failed clear in the reconciler re-delivers
		// the same observation on the next block.
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Msg("claim observed on-chain but session already at or past claimed")
		return nil
	}

	if createdCallback == nil {
		// Redis says claimed but nothing re-tracks it in memory. The row is
		// recoverable by loadExistingSessions on the next start/handoff, so
		// this is degraded, not silent.
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Msg("session reactivated in Redis but no lifecycle callback is wired; proof depends on a restart")
		return nil
	}

	snapshot, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to re-read reactivated session %s: %w", sessionID, err)
	}
	if snapshot == nil {
		return fmt.Errorf("reactivated session %s vanished before re-tracking", sessionID)
	}

	if err := createdCallback(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to re-track reactivated session %s: %w", sessionID, err)
	}

	return nil
}

// OnProofSubmitted should be called when a session's proof TX is broadcast.
// It stores the proof TX hash for deduplication and tracking.
func (c *SessionCoordinator) OnProofSubmitted(
	ctx context.Context,
	sessionID string,
	proofTxHash string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Get current snapshot
	snapshot, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session snapshot: %w", err)
	}
	if snapshot == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Update with proof TX hash (state is already set to Proving by state machine)
	snapshot.ProofTxHash = proofTxHash

	if err := c.sessionStore.Save(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to save session snapshot: %w", err)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Str("proof_tx_hash", proofTxHash).
		Msg("proof TX hash stored")

	return nil
}

// OnSessionProved should be called when a session proof is successfully submitted.
// It updates the session state to proved.
func (c *SessionCoordinator) OnSessionProved(
	ctx context.Context,
	sessionID string,
) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	// Update state to proved
	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProved); err != nil {
		c.logger.Warn().
			Err(err).
			Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to proved")
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProved)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProved)
	}

	c.logger.Debug().
		Str(logging.FieldSessionID, sessionID).
		Msg("session proved")

	return nil
}

// OnClaimWindowClosed marks session as failed due to claim window timeout.
// Updates state immediately in Redis for HA compatibility.
func (c *SessionCoordinator) OnClaimWindowClosed(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateClaimWindowClosed); err != nil {
		if errors.Is(err, ErrClaimAlreadyOnChain) {
			c.logger.Debug().Err(err).Str(logging.FieldSessionID, sessionID).
				Msg("not marking claim_window_closed: the session holds its claim")
			return err
		}
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to claim_window_closed")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateClaimWindowClosed)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateClaimWindowClosed)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("claim window closed")
	return nil
}

// OnClaimTxError marks session as failed due to claim transaction error.
// Updates state immediately in Redis for HA compatibility.
//
// IMPORTANT: This method checks the current Redis state before overwriting.
// In multi-miner setups, another miner may have already successfully claimed
// the session. Overwriting SessionStateClaimed with SessionStateClaimTxError
// would kill the session and prevent proof submission, causing lost rewards.
func (c *SessionCoordinator) OnClaimTxError(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	// Check current state in Redis before overwriting — another miner may have
	// already claimed this session successfully. Marking a successfully-claimed
	// session as ClaimTxError would prevent proof submission and lose rewards.
	current, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to read session state before marking claim_tx_error")
		// Fall through — better to risk a no-op than to skip the error marking entirely
	} else if current != nil && (current.State == SessionStateClaimed || current.ClaimTxHash != "") {
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Str("current_state", string(current.State)).
			Str("claim_tx_hash", current.ClaimTxHash).
			Msg("NOT marking claim_tx_error: session already claimed by another miner")
		return nil
	}

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateClaimTxError); err != nil {
		if errors.Is(err, ErrClaimAlreadyOnChain) {
			c.logger.Debug().Err(err).Str(logging.FieldSessionID, sessionID).
				Msg("not marking claim_tx_error: the session holds its claim")
			return err
		}
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to claim_tx_error")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateClaimTxError)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateClaimTxError)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("claim tx error")
	return nil
}

// OnClaimMissing marks the session as terminally failed because no on-chain
// claim exists for it at proof time. Recorded by the pre-proof GetClaim guard
// (and, once wired, by the claim reconciler).
//
// IMPORTANT: This method checks the current Redis state before overwriting.
// If another miner in the HA set has already transitioned the session to a
// successful terminal state, do not overwrite — that would hide a success.
func (c *SessionCoordinator) OnClaimMissing(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	current, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to read session state before marking claim_missing")
	} else if current != nil && current.State.IsSuccess() {
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Str("current_state", string(current.State)).
			Msg("NOT marking claim_missing: session already settled successfully by another miner")
		return nil
	}

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateClaimMissing); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to claim_missing")
		return err
	}

	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateClaimMissing)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateClaimMissing)
	}

	c.logger.Info().Str(logging.FieldSessionID, sessionID).
		Msg("claim missing on-chain — proof skipped, session terminal")
	return nil
}

// OnProofWindowClosed marks session as failed due to proof window timeout.
// Updates state immediately in Redis for HA compatibility.
func (c *SessionCoordinator) OnProofWindowClosed(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProofWindowClosed); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to proof_window_closed")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProofWindowClosed)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProofWindowClosed)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("proof window closed")
	return nil
}

// OnProofTxError marks session as failed due to proof transaction error.
// Updates state immediately in Redis for HA compatibility.
//
// IMPORTANT: This method checks the current Redis state before overwriting.
// In multi-miner setups, another miner may have already successfully proved
// the session. Overwriting SessionStateProved with SessionStateProofTxError
// would incorrectly mark a successful session as failed.
func (c *SessionCoordinator) OnProofTxError(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	// Check current state in Redis before overwriting — another miner may have
	// already proved this session successfully.
	current, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to read session state before marking proof_tx_error")
	} else if current != nil && (current.State == SessionStateProved || current.State == SessionStateProbabilisticProved || current.ProofTxHash != "") {
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Str("current_state", string(current.State)).
			Str("proof_tx_hash", current.ProofTxHash).
			Msg("NOT marking proof_tx_error: session already proved by another miner")
		return nil
	}

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProofTxError); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to proof_tx_error")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProofTxError)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProofTxError)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("proof tx error")
	return nil
}

// OnProofDeferred returns a session to SessionStateClaimed after a proof
// attempt was abandoned for a reason that will not still be true next block
// — Redis unreachable while reading the claimed root, or a shutdown cancel.
//
// It is OnProofTxError minus the terminal callback, and that omission is the
// whole point. onSessionTerminal is wired to RemoveSession
// (supplier_manager.go), so marking a session terminal drops it from
// activeSessions and nothing looks at it again; its claim is already on
// chain, and a required proof that never arrives costs the entire claim plus
// a flat slash.
//
// Writing claimed rather than leaving the session in proving is also
// deliberate: a session in proving can only leave through
// proof_window_closed (session_lifecycle.go), which is the same money lost,
// just more quietly. From claimed, checkSessionTransition returns Proving
// again on every block while currentHeight is inside the proof window, and
// stops on its own at proofWindowClose. No new loop, no new state.
func (c *SessionCoordinator) OnProofDeferred(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	c.mu.Unlock()

	// Same guard as OnProofTxError: another miner may already have proved
	// this session, and rewinding it to claimed would make this miner
	// submit a duplicate proof.
	current, err := c.sessionStore.Get(ctx, sessionID)
	if err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to read session state before deferring proof")
	} else if current != nil && (current.State == SessionStateProved || current.State == SessionStateProbabilisticProved || current.ProofTxHash != "") {
		c.logger.Warn().
			Str(logging.FieldSessionID, sessionID).
			Str("current_state", string(current.State)).
			Str("proof_tx_hash", current.ProofTxHash).
			Msg("NOT deferring proof: session already proved by another miner")
		return nil
	}

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateClaimed); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to return session to claimed after deferring proof")
		return err
	}

	c.logger.Info().
		Str(logging.FieldSessionID, sessionID).
		Msg("proof deferred: session returned to claimed, will retry next block inside the proof window")
	return nil
}

// OnProbabilisticProved marks session as probabilistically proved (no proof required).
// Updates state immediately in Redis for HA compatibility.
func (c *SessionCoordinator) OnProbabilisticProved(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateProbabilisticProved); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to probabilistic_proved")
		return err
	}

	// Notify in-memory state update (if callback registered)
	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateProbabilisticProved)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateProbabilisticProved)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("probabilistic proved (no proof required)")
	return nil
}

// OnClaimSkipped marks a session as intentionally skipped by the economic
// viability check (reward would not cover claim_fee + proof_fee). This is a
// terminal, non-failure state — the miner chose not to spend fees on an
// unprofitable session.
func (c *SessionCoordinator) OnClaimSkipped(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("session coordinator is closed")
	}
	terminalCallback := c.onSessionTerminal
	c.mu.Unlock()

	if err := c.sessionStore.UpdateState(ctx, sessionID, SessionStateClaimSkipped); err != nil {
		c.logger.Warn().Err(err).Str(logging.FieldSessionID, sessionID).
			Msg("failed to update session state to claim_skipped")
		return err
	}

	if terminalCallback != nil {
		c.logger.Debug().
			Str(logging.FieldSessionID, sessionID).
			Str(logging.FieldSessionState, string(SessionStateClaimSkipped)).
			Msg("session_coordinator_terminal: invoking terminal callback")
		terminalCallback(sessionID, SessionStateClaimSkipped)
	}

	c.logger.Debug().Str(logging.FieldSessionID, sessionID).Msg("claim skipped (economic viability)")
	return nil
}

// Close gracefully shuts down the session coordinator.
func (c *SessionCoordinator) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	c.logger.Info().Msg("session coordinator closed")
	return nil
}
