//go:build test

package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// budgetProbe is a SupplierTxClient that answers the two questions this test
// needs -- what time the chain is at, and what context the resend arrived with.
type budgetProbe struct {
	mu     sync.Mutex
	now    time.Time
	gotCtx context.Context
	calls  int
}

func (p *budgetProbe) LatestBlockTime() time.Time { return p.now }

func (p *budgetProbe) BroadcastRawReturningHash(ctx context.Context, _ string, _ tx.SignedTxPayload) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gotCtx, p.calls = ctx, p.calls+1
	return "hash-resent", nil
}

func (p *budgetProbe) CreateClaimsReturningHash(context.Context, int64, ...pocktclient.MsgCreateClaim) (string, tx.SignedTxPayload, error) {
	panic("this test re-injects cached bytes; reaching the signing path means reusable() changed")
}

func (p *budgetProbe) SubmitProofsReturningHash(context.Context, int64, ...pocktclient.MsgSubmitProof) (string, tx.SignedTxPayload, error) {
	panic("this test re-injects cached bytes; reaching the signing path means reusable() changed")
}

func (p *budgetProbe) window(t *testing.T) (time.Duration, string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Equal(t, 1, p.calls, "the resend never reached the client, so there is no context to read")
	d, regime, ok := tx.TxWindowFrom(p.gotCtx)
	require.True(t, ok, "the context that reached the client carries NO budget: nothing was put on it")
	return d, regime
}

// managerWithProbe wires a manager owning one supplier whose tx client is the
// probe, which is what the SupplierTxClient seam exists for: ResubmitMessage
// returns early when the supplier has no client, so this branch is unreachable
// without a substitutable one.
func managerWithProbe(t *testing.T, supplier string) (*SupplierManager, *budgetProbe, tx.SignedTxPayload) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0).UTC()
	probe := &budgetProbe{now: now}

	m := &SupplierManager{
		logger:    zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.TraceLevel),
		suppliers: xsync.NewMap[string, *SupplierState](),
	}
	m.suppliers.Store(supplier, &SupplierState{SupplierClient: probe})

	// Re-injectable: bytes present and the sealed deadline still ahead of the
	// chain's clock, so the resend takes the cached path and never signs.
	cached := tx.SignedTxPayload{Bytes: []byte("signed-tx"), TimeoutAt: now.Add(2 * time.Minute)}
	return m, probe, cached
}

// 1 + 3: an ABSENT budget falls to the ceiling under the `unknown` regime, and
// that timeout actually reaches the client on the context. The regime is counted
// by tx where a transaction is signed (tx/tx_timeout_regime_test.go); this
// resend re-injects cached bytes, so what it must get right is the context.
func TestResubmit_AnAbsentBudgetFallsToTheCeilingAndTravels(t *testing.T) {
	m, probe, cached := managerWithProbe(t, "pokt1absent")

	hash, _, err := m.ResubmitMessage(context.Background(), RebroadcastPhaseClaim,
		"pokt1absent", nil, cached, 500, 0, "")
	require.NoError(t, err)
	require.Equal(t, "hash-resent", hash)

	ceiling, _ := tx.WindowTimeout(0, 0)
	gotTimeout, gotRegime := probe.window(t)
	require.Equal(t, ceiling, gotTimeout, "the ceiling was chosen but a different budget reached the client")
	require.Equal(t, tx.TimeoutRegimeUnknown, gotRegime)
}

// 2: an INHERITED budget is used as given. Without this, writing `unknown`
// unconditionally passes the test above and the suite certifies the opposite of
// the property.
func TestResubmit_AnInheritedBudgetIsUsedAsGiven(t *testing.T) {
	m, probe, cached := managerWithProbe(t, "pokt1inherited")

	_, _, err := m.ResubmitMessage(context.Background(), RebroadcastPhaseClaim,
		"pokt1inherited", nil, cached, 500, 100*time.Second, tx.TimeoutRegimeWindow)
	require.NoError(t, err)

	gotTimeout, gotRegime := probe.window(t)
	require.Equal(t, 100*time.Second, gotTimeout, "the resend must spend the budget it inherited, not a fresh one")
	require.Equal(t, tx.TimeoutRegimeWindow, gotRegime)
}
