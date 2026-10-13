//go:build test

package miner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A proof can land after a broadcast that reported failure: the session sat in
// proof_tx_error, its money in `unresolved`, and the dashboards showed a failed
// session the chain had paid. These pin the correction: the chain's verdict
// moves the session to proved, once, and the money settles once.

var allSessionStates = []SessionState{
	SessionStateActive, SessionStateClaiming, SessionStateClaimed,
	SessionStateClaimWindowClosed, SessionStateClaimTxError, SessionStateClaimMissing,
	SessionStateClaimSkipped, SessionStateProving, SessionStateProved,
	SessionStateProbabilisticProved, SessionStateProofWindowClosed, SessionStateProofTxError,
}

func TestLuaCanReactivateProvedMatchesGo(t *testing.T) {
	client, _ := newTestRedis(t)
	for _, s := range allSessionStates {
		for _, hash := range []string{"", "PROOFTX"} {
			got, err := client.Eval(context.Background(),
				luaCanReactivateProved+"\nreturn can_reactivate_proved(ARGV[1], ARGV[2]) and 1 or 0", nil,
				string(s), hash).Int64()
			require.NoError(t, err)
			require.Equalf(t, canReactivateProved(s, hash), got == 1, "state %q hash %q: Lua and Go disagree", s, hash)
		}
	}
}

func seedProvable(t *testing.T, store SessionStore, id, service string, state SessionState, proofTxHash string) {
	t.Helper()
	created, err := store.CreateIfAbsent(context.Background(), &SessionSnapshot{
		SessionID: id, SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: state,
		ClaimTxHash: "CLAIMTX", ProofTxHash: proofTxHash,
		RelayCount: 3, TotalComputeUnits: 3_000_000,
	})
	require.NoError(t, err)
	require.True(t, created)
}

func TestOnProofObservedOnChain_FlipsOnlyWhatTheChainCanCorrect(t *testing.T) {
	cases := []struct {
		name      string
		state     SessionState
		proofHash string
		wantFlip  bool
	}{
		{"proof_tx_error: the broadcast reported failure, the proof landed", SessionStateProofTxError, "", true},
		{"proving: local state lagged", SessionStateProving, "", true},
		{"claimed: killed before the proving write", SessionStateClaimed, "", true},
		{"proof_window_closed with a hash", SessionStateProofWindowClosed, "SENT", true},
		{"proof_window_closed without a hash was counted lost", SessionStateProofWindowClosed, "", false},
		{"proved is a no-op", SessionStateProved, "SENT", false},
		{"probabilistic_proved is not rewritten", SessionStateProbabilisticProved, "", false},
		{"claim_tx_error is upstream: the claim side decides it", SessionStateClaimTxError, "", false},
		{"active is upstream", SessionStateActive, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
				ctx := context.Background()
				seedProvable(t, store, "sess", service, tc.state, tc.proofHash)
				var terminal []SessionState
				var mu sync.Mutex
				coord.SetOnSessionTerminalCallback(func(_ string, s SessionState) {
					mu.Lock()
					terminal = append(terminal, s)
					mu.Unlock()
				})

				r, err := coord.OnProofObservedOnChain(ctx, "sess", "FOUNDTX")
				require.NoError(t, err)
				got, err := store.Get(ctx, "sess")
				require.NoError(t, err)
				mu.Lock()
				defer mu.Unlock()
				if !tc.wantFlip {
					require.Equal(t, Reactivation{}, r, "nothing written, nothing reported")
					require.Equal(t, tc.state, got.State)
					require.Equal(t, tc.proofHash, got.ProofTxHash)
					require.Empty(t, terminal)
					return
				}
				require.Equal(t, tc.state, r.From)
				require.Equal(t, tc.proofHash, r.ProofTxHash, "the hash it carried before")
				require.Equal(t, "CLAIMTX", r.ClaimTxHash)
				require.Equal(t, SessionStateProved, got.State)
				want := tc.proofHash
				if want == "" {
					want = "FOUNDTX"
				}
				require.Equal(t, want, got.ProofTxHash, "a missing hash is filled, a present one kept")
				require.Equal(t, []SessionState{SessionStateProved}, terminal)

				again, err := coord.OnProofObservedOnChain(ctx, "sess", "FOUNDTX")
				require.NoError(t, err)
				require.Equal(t, Reactivation{}, again, "a second observation finds it proved")
			})
		})
	}
}

func TestOnProofObservedOnChain_UnknownSessionIsAnError(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, _ SessionStore, _ string) {
		_, err := coord.OnProofObservedOnChain(context.Background(), "sess-absent", "FOUNDTX")
		require.Error(t, err)
	})
}

func TestOnProofObservedOnChain_ConcurrentObserversFlipOnce(t *testing.T) {
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedProvable(t, store, "sess", service, SessionStateProofTxError, "")
		var flips atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if r, err := coord.OnProofObservedOnChain(ctx, "sess", "FOUNDTX"); err == nil && r.From != "" {
					flips.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		require.Equal(t, int32(1), flips.Load(), "exactly one observer flips, so the money settles once")
	})
}

// provedReconcilerManager builds a SupplierManager owning pokt1test, with the
// real reconciler phases, over a Redis session store.
func provedReconcilerManager(t *testing.T) (*SupplierManager, SessionStore) {
	t.Helper()
	coord, store, client := setupTestCoordinator(t)
	coord.SetPricer(perMillionPricer{})
	m := &SupplierManager{
		logger:    zerolog.Nop(),
		suppliers: xsync.NewMap[string, *SupplierState](),
		config: SupplierManagerConfig{
			RedisClient:           client,
			ProofQueryClient:      inclusionProbe{},
			BlockClient:           &mockBlockClient{},
			SubmissionTrackingTTL: time.Hour,
			Pricer:                perMillionPricer{},
		},
	}
	state := &SupplierState{OperatorAddr: "pokt1test", SessionStore: store, SessionCoordinator: coord}
	state.StoreStatus(SupplierStatusActive)
	m.suppliers.Store("pokt1test", state)
	m.ensureSharedTrackers()
	t.Cleanup(func() {
		if m.reconcilerCancel != nil {
			m.reconcilerCancel()
		}
		m.reconcilerWG.Wait()
	})
	require.NotNil(t, m.inclusionReconciler)
	return m, store
}

// A proof_tx_error proof the reconciler finds on chain: the session becomes
// proved, the unresolved balance closes and proved is credited, each once,
// though the observation is delivered twice (a clear that failed).
func TestRecordProofOutcome_FoundMovesAFailedProofToProvedOnce(t *testing.T) {
	m, store := provedReconcilerManager(t)
	ctx := context.Background()
	service := t.Name()
	phase := string(RebroadcastPhaseProof)
	seedProvable(t, store, "sess", service, SessionStateProofTxError, "")
	provedBefore := upoktProved("pokt1test", service)
	closedBefore := unresolvedClosed("pokt1test", service, phase)

	failed := rebroadcastEntry{ServiceID: service, TxHash: "REBROADCAST"} // OrigTxHash "": never confirmed
	for i := 0; i < 2; i++ {
		require.NoError(t, m.inclusionReconciler.proofPhase.recordOutcome(
			ctx, failed, "pokt1test", 110, "sess", inclusionFound, 120))
	}

	got, err := store.Get(ctx, "sess")
	require.NoError(t, err)
	require.Equal(t, SessionStateProved, got.State, "the chain validated the proof: proved, not proof_tx_error")
	require.Equal(t, "REBROADCAST", got.ProofTxHash)
	require.Equal(t, provedBefore+3, upoktProved("pokt1test", service), "proved credited once")
	require.Equal(t, closedBefore+3, unresolvedClosed("pokt1test", service, phase), "unresolved closed once")
}

// A supplier this replica no longer owns keeps the entry for its new owner.
func TestRecordProofOutcome_FoundOnAnUnownedSupplierKeepsTheEntry(t *testing.T) {
	m, _ := provedReconcilerManager(t)
	err := m.inclusionReconciler.proofPhase.recordOutcome(context.Background(),
		rebroadcastEntry{ServiceID: "svc", TxHash: "REBROADCAST"}, "pokt1other", 110, "sess", inclusionFound, 120)
	require.Error(t, err, "an error keeps the entry")
}
