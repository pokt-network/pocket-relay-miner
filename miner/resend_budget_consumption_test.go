//go:build test

package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
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

func regimeCount(phase, regime string) float64 {
	return testutil.ToFloat64(txTimeoutRegimeTotal.WithLabelValues(phase, regime))
}

// 1 + 3: an ABSENT budget falls to the ceiling under the `unknown` regime, it is
// COUNTED as such, and -- the half a metric cannot show -- that timeout actually
// reaches the client on the context.
//
// Asserting only the counter would certify that the branch DECIDED correctly and
// say nothing about whether the decision was USED: breaking the line that puts
// the budget on the context, while leaving the line that records the regime,
// keeps a counter-only test green with the defect alive.
func TestResubmit_AnAbsentBudgetFallsToTheCeilingAndTravels(t *testing.T) {
	m, probe, cached := managerWithProbe(t, "pokt1absent")

	beforeUnknown := regimeCount("resend", tx.TimeoutRegimeUnknown)
	beforeCeiling := regimeCount("resend", tx.TimeoutRegimeCeiling)

	hash, _, err := m.ResubmitMessage(context.Background(), RebroadcastPhaseClaim,
		"pokt1absent", nil, cached, 500, 0, "")
	require.NoError(t, err)
	require.Equal(t, "hash-resent", hash)

	require.Equal(t, beforeUnknown+1, regimeCount("resend", tx.TimeoutRegimeUnknown),
		"an absent budget must be counted as `unknown`, so the counter shows a defect instead of a plausible number")
	require.Equal(t, beforeCeiling, regimeCount("resend", tx.TimeoutRegimeCeiling),
		"the ceiling VALUE is used, but the regime that names it is `unknown`; counting it as `ceiling` would hide the defect")

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

	beforeWindow := regimeCount("resend", tx.TimeoutRegimeWindow)
	beforeUnknown := regimeCount("resend", tx.TimeoutRegimeUnknown)

	_, _, err := m.ResubmitMessage(context.Background(), RebroadcastPhaseClaim,
		"pokt1inherited", nil, cached, 500, 100*time.Second, tx.TimeoutRegimeWindow)
	require.NoError(t, err)

	require.Equal(t, beforeWindow+1, regimeCount("resend", tx.TimeoutRegimeWindow))
	require.Equal(t, beforeUnknown, regimeCount("resend", tx.TimeoutRegimeUnknown),
		"the budget was inherited, so nothing was unknown; degrading anyway loses the window property the whole design rests on")

	gotTimeout, gotRegime := probe.window(t)
	require.Equal(t, 100*time.Second, gotTimeout, "the resend must spend the budget it inherited, not a fresh one")
	require.Equal(t, tx.TimeoutRegimeWindow, gotRegime)
}
