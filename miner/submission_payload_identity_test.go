//go:build test

package miner

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// reconcilerInTheWindow is a block client whose every LastBlock first runs a
// resend through the SAME supplier client the lifecycle uses -- what the
// inclusion reconciler does when it re-signs a stored message. In production
// the two are separate goroutines woken by the same block event with nothing
// ordering them; here the resend is placed deterministically, because the
// failure path evaluates LastBlock as an argument of the very call that
// persists the signed bytes, so a resend there lands between the lifecycle's
// last send and the moment it decides which bytes to keep.
type reconcilerInTheWindow struct {
	heightedBlocks
	resend func()
	fired  atomic.Int32
}

func (r *reconcilerInTheWindow) LastBlock(ctx context.Context) pocktclient.Block {
	if r.resend != nil {
		r.resend()
		r.fired.Add(1)
	}
	return r.heightedBlocks.LastBlock(ctx)
}

// resentHeader is the header of ANOTHER session of the same supplier, the one
// the reconciler is re-signing while the lifecycle submits its own.
func resentHeader(sessionID string) *sessiontypes.SessionHeader {
	return &sessiontypes.SessionHeader{
		SessionId:               sessionID,
		SessionStartBlockHeight: 61,
		SessionEndBlockHeight:   80,
		ApplicationAddress:      "pokt1otherapp",
		ServiceId:               "svc",
	}
}

// assertOwnSignedBytes reads the rebroadcast entry the failure path stored for
// ownSession and checks WHOSE transaction it holds. The session IDs travel as
// plain strings inside the signed protobuf, so containment is an identity check
// on the bytes themselves, not on a hash the test would have to recompute.
func assertOwnSignedBytes(t *testing.T, store *RebroadcastStore, phase RebroadcastPhase, supplier, ownSession, resentSession string, fired int32) {
	t.Helper()

	// The resend has to have RUN, or a green below would only say the window
	// was never crossed.
	if fired == 0 {
		t.Fatal("the reconciler's resend never ran: the window this test exists to cross was not crossed")
	}

	pending, err := store.List(context.Background(), phase, supplier, 100)
	if err != nil {
		t.Fatalf("listing the rebroadcast store: %v", err)
	}
	raw, ok := pending[ownSession]
	if !ok {
		t.Fatalf("the failed submission stored no entry for %q; stored = %d entries", ownSession, len(pending))
	}
	entry, err := unmarshalRebroadcastEntry(raw)
	if err != nil {
		t.Fatalf("stored entry for %q will not decode: %v", ownSession, err)
	}
	if len(entry.SignedBytes) == 0 {
		t.Fatalf("every attempt failed at BROADCAST, after signing, so %q must keep signed bytes to re-inject", ownSession)
	}
	if bytes.Contains(entry.SignedBytes, []byte(resentSession)) {
		t.Fatalf("the entry of %q holds the transaction of %q: re-injecting it sends ANOTHER session's signed tx and leaves %q without its own",
			ownSession, resentSession, ownSession)
	}
	if !bytes.Contains(entry.SignedBytes, []byte(ownSession)) {
		t.Fatalf("the entry of %q holds signed bytes that are not its own transaction", ownSession)
	}
}

// TestClaimFailureKeepsItsOwnSignedBytes pins that a claim batch which failed
// every attempt stores, for re-injection, the transaction IT signed -- not the
// one a concurrent resend of another session signed on the same client.
//
// The client is the production HASupplierClient on a mock node, not a double:
// the defect lived in the shared state of that client, and a double never
// reached it, which is how it survived the existing suite.
func TestClaimFailureKeepsItsOwnSignedBytes(t *testing.T) {
	const (
		supplier      = "pokt1payloadidentityclaim"
		ownSession    = "session-own-claim-that-failed-0000"
		resentSession = "session-claim-the-reconciler-resends"
	)

	node := tx.NewTestSupplierNode(t, supplier)
	node.FailBroadcasts(errors.New("connection reset by the full node"))

	redisClient, _ := newTestRedis(t)
	store := NewRebroadcastStore(redisClient, time.Hour)

	resent := &prooftypes.MsgCreateClaim{
		SupplierOperatorAddress: supplier,
		SessionHeader:           resentHeader(resentSession),
		RootHash:                make([]byte, SMSTRootLen),
	}
	blocks := &reconcilerInTheWindow{}
	blocks.currentHeight = 103
	blocks.resend = func() {
		_, _, _ = node.Client.CreateClaimsReturningHash(context.Background(), 1000, resent)
	}

	lc := &LifecycleCallback{
		logger:           logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:     &defaultParamsShared{},
		blockClient:      blocks,
		smstManager:      smstStub{},
		supplierClient:   node.Client,
		serviceClient:    erroringService{},
		rebroadcastStore: store,
		config: LifecycleCallbackConfig{
			ClaimRetryAttempts: 2,
			ClaimRetryDelay:    time.Millisecond,
		},
	}

	_, err := lc.OnSessionsNeedClaim(context.Background(), []*SessionSnapshot{{
		SessionID:               ownSession,
		SessionEndHeight:        100,
		SessionStartHeight:      81,
		SupplierOperatorAddress: supplier,
		ApplicationAddress:      "pokt1ownapp",
		ServiceID:               "svc",
		RelayCount:              10,
		TotalComputeUnits:       100,
		State:                   SessionStateClaiming,
	}})
	if err == nil {
		t.Fatal("every broadcast failed, so the claim cycle must report the failure")
	}

	assertOwnSignedBytes(t, store, RebroadcastPhaseClaim, supplier, ownSession, resentSession, blocks.fired.Load())
}

// TestProofFailureKeepsItsOwnSignedBytes is the proof twin. The proof cycle is
// written as a copy of the claim cycle and read the same shared slot, so the
// defect existed on both sides and each side needs its own guard.
//
// The height is a precondition, not setup: a session ending at 100 under
// default params has its proof window at 106..110.
func TestProofFailureKeepsItsOwnSignedBytes(t *testing.T) {
	const (
		supplier      = "pokt1payloadidentityproof"
		ownSession    = "session-own-proof-that-failed-0000"
		resentSession = "session-proof-the-reconciler-resends"
	)

	node := tx.NewTestSupplierNode(t, supplier)
	node.FailBroadcasts(errors.New("connection reset by the full node"))

	redisClient, _ := newTestRedis(t)
	store := NewRebroadcastStore(redisClient, time.Hour)

	resent := &prooftypes.MsgSubmitProof{
		SupplierOperatorAddress: supplier,
		SessionHeader:           resentHeader(resentSession),
		Proof:                   []byte("proof-of-the-other-session"),
	}
	blocks := &reconcilerInTheWindow{}
	blocks.currentHeight = 108
	blocks.resend = func() {
		_, _, _ = node.Client.SubmitProofsReturningHash(context.Background(), 1000, resent)
	}

	lc := &LifecycleCallback{
		logger:           logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient:     &defaultParamsShared{},
		blockClient:      blocks,
		smstManager:      provingSMST{},
		supplierClient:   node.Client,
		serviceClient:    erroringService{},
		rebroadcastStore: store,
		config: LifecycleCallbackConfig{
			ProofRetryAttempts: 2,
			ProofRetryDelay:    time.Millisecond,
		},
	}

	_, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{{
		SessionID:               ownSession,
		SessionEndHeight:        100,
		SessionStartHeight:      81,
		SupplierOperatorAddress: supplier,
		ApplicationAddress:      "pokt1ownapp",
		ServiceID:               "svc",
		RelayCount:              10,
		TotalComputeUnits:       100,
		State:                   SessionStateProving,
		ClaimedRootHash:         make([]byte, SMSTRootLen),
	}})
	if err == nil {
		t.Fatal("every broadcast failed, so the proof cycle must report the failure")
	}

	assertOwnSignedBytes(t, store, RebroadcastPhaseProof, supplier, ownSession, resentSession, blocks.fired.Load())
}
