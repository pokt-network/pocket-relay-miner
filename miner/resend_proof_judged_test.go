//go:build test

package miner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// A resend that signs a NEW proof transaction asks the chain, uncached, whether
// the claim's proof was already judged. poktroll deletes a proof once judged and
// SubmitProof does not read the verdict, so a second proof is charged again.

type judgedStates struct {
	query.ProofQueryClient
	states map[string]query.SessionClaim
	err    error
}

func (j *judgedStates) GetSupplierSessionStates(context.Context, string) (map[string]query.SessionClaim, error) {
	return j.states, j.err
}

type signingProbe struct {
	mu     sync.Mutex
	proofs int
}

func (p *signingProbe) LatestBlockTime() time.Time { return time.Unix(1_700_000_000, 0) }

func (p *signingProbe) BroadcastRawReturningHash(context.Context, string, tx.SignedTxPayload) (string, error) {
	panic("nothing cached: this resend must sign, not re-inject")
}

func (p *signingProbe) CreateClaimsReturningHash(context.Context, int64, ...pocktclient.MsgCreateClaim) (string, tx.SignedTxPayload, error) {
	panic("this test resends a proof")
}

func (p *signingProbe) SubmitProofsReturningHash(context.Context, int64, ...pocktclient.MsgSubmitProof) (string, tx.SignedTxPayload, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.proofs++
	return "proof-hash", tx.SignedTxPayload{Bytes: []byte("signed")}, nil
}

func (p *signingProbe) signed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.proofs
}

func resendProof(t *testing.T, q query.ProofQueryClient) (*signingProbe, error) {
	t.Helper()
	const supplier, sessionID = "pokt1judged", "sess-judged"
	probe := &signingProbe{}
	m := &SupplierManager{
		logger:    zerolog.Nop(),
		suppliers: xsync.NewMap[string, *SupplierState](),
		config:    SupplierManagerConfig{ProofQueryClient: q},
	}
	m.suppliers.Store(supplier, &SupplierState{SupplierClient: probe})
	msg := &prooftypes.MsgSubmitProof{
		SupplierOperatorAddress: supplier,
		SessionHeader:           &sessiontypes.SessionHeader{SessionId: sessionID},
	}
	b, err := msg.Marshal()
	require.NoError(t, err)
	_, _, err = m.ResubmitMessage(context.Background(), RebroadcastPhaseProof, supplier, b, tx.SignedTxPayload{}, 500, time.Minute, tx.TimeoutRegimeWindow)
	return probe, err
}

func TestResubmitProof_AJudgedProofIsNotSignedAgain(t *testing.T) {
	for _, st := range []query.SessionProofState{query.SessionProofValidated, query.SessionProofRejected} {
		probe, err := resendProof(t, &judgedStates{states: map[string]query.SessionClaim{"sess-judged": {ProofState: st}}})
		require.Truef(t, errors.Is(err, ErrProofAlreadyJudged), "state %v: err=%v", st, err)
		require.Zerof(t, probe.signed(), "state %v: a judged proof must not be charged a second time", st)
	}
}

func TestResubmitProof_APendingProofIsSent(t *testing.T) {
	probe, err := resendProof(t, &judgedStates{states: map[string]query.SessionClaim{"sess-judged": {ProofState: query.SessionProofPending}}})
	require.NoError(t, err)
	require.Equal(t, 1, probe.signed(), "control: a proof the chain has not judged is sent")
}

func TestResubmitProof_AnUnansweredQuestionIsNotAVerdict(t *testing.T) {
	probe, err := resendProof(t, &judgedStates{err: errors.New("connection refused")})
	require.NoError(t, err)
	require.Equal(t, 1, probe.signed(), "a failed read must not be taken as validated: a missed proof costs the claim")
}

func TestReconciler_AJudgedProofDropsTheEntry(t *testing.T) {
	h := newReconcilerHarness(t, 1)
	h.resub.failWith = ErrProofAlreadyJudged
	h.seedNeverSent(t, hSupplier, hEnd, "s1", testSubmit)

	for height := testSubmit; height < testWindowClose; height++ {
		h.r.OnBlock(height)
	}

	require.Equal(t, 1, h.resub.attemptCount(), "one question, then nothing left to resend")
	require.Zero(t, h.pendingCount(t, hSupplier, hEnd), "the entry is gone")
}
