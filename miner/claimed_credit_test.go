//go:build test

package miner

import (
	"context"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func claimedProbe(service string) float64 {
	return testutil.ToFloat64(upoktClaimedTotal.WithLabelValues("pokt1test", service))
}

func seedClaimable(t *testing.T, store SessionStore, id, service string, state SessionState, txHash, verdict string) {
	t.Helper()
	created, err := store.CreateIfAbsent(context.Background(), &SessionSnapshot{
		SessionID: id, SupplierOperatorAddress: "pokt1test", ServiceID: service,
		SessionStartHeight: 100, SessionEndHeight: 110, State: state, ClaimTxHash: txHash,
		ClaimMissingVerdict: verdict, RelayCount: 3, TotalComputeUnits: 3_000_000,
	})
	require.NoError(t, err)
	require.True(t, created)
}

// The money of a claim enters upokt_claimed_total once, at the flip into
// claimed, whichever path flips it: the submission, the window close asking the
// chain after a process died between its broadcast and its claimed write, or
// the inclusion reconciler. A path that finds the session already claimed
// counts nothing.
func TestClaimedIsCreditedOnceByTheFlipIntoClaimed(t *testing.T) {
	root := make([]byte, SMSTRootLen)
	cases := []struct {
		name          string
		state         SessionState
		txHash        string
		verdict       string
		wantCredited  bool
		viaSubmission bool
	}{
		{"submission from claiming", SessionStateClaiming, "", "", true, true},
		{"window close after a kill before the claimed write", SessionStateClaiming, "", "", true, false},
		{"active session observed on chain", SessionStateActive, "", "", true, false},
		{"reconciler finds a claim_tx_error claim", SessionStateClaimTxError, "", "", true, false},
		{"claim_window_closed without a hash (forgone)", SessionStateClaimWindowClosed, "", "", true, false},
		{"claim_window_closed with a hash (already claimed)", SessionStateClaimWindowClosed, "TX", "", false, false},
		{"claim_missing forgone", SessionStateClaimMissing, "TX", ClaimMissingForgone, true, false},
		{"claim_missing lost (already claimed)", SessionStateClaimMissing, "TX", ClaimMissingLost, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
				ctx := context.Background()
				seedClaimable(t, store, "sess", service, tc.state, tc.txHash, tc.verdict)
				before := claimedProbe(service)

				flip := func() error {
					if tc.viaSubmission {
						return coord.OnSessionClaimed(ctx, "sess", root, "TXSUBMIT")
					}
					return coord.OnClaimObservedOnChain(ctx, "sess", root, "TXFOUND")
				}
				require.NoError(t, flip())
				require.NoError(t, flip(), "a second flip finds the session claimed")
				require.NoError(t, coord.OnSessionClaimed(ctx, "sess", root, "TXLATE"), "nor does a late submission write")

				want := before
				if tc.wantCredited {
					want += 3 // 3,000,000 compute units at one uPOKT per million
				}
				require.Equal(t, want, claimedProbe(service))
				got, err := store.Get(ctx, "sess")
				require.NoError(t, err)
				require.Equal(t, SessionStateClaimed, got.State)
			})
		})
	}
}

// The submission and the window close observe the same claim concurrently:
// the flip lets one through, and the money is counted once.
func TestClaimedIsCreditedOnceWhenTheSubmissionAndTheObservationRace(t *testing.T) {
	root := make([]byte, SMSTRootLen)
	claimMissingStores(t, func(t *testing.T, coord *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedClaimable(t, store, "sess-race", service, SessionStateClaiming, "", "")
		before := claimedProbe(service)

		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func() { defer wg.Done(); errs <- coord.OnSessionClaimed(ctx, "sess-race", root, "TXSUBMIT") }()
			go func() { defer wg.Done(); errs <- coord.OnClaimObservedOnChain(ctx, "sess-race", root, "TXFOUND") }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, before+3, claimedProbe(service), "sixteen observers, one credit")
	})
}

// observedBeforeFlip is a session store where another path observes the claim
// on chain, and flips the session, between the submission's read and its flip.
type observedBeforeFlip struct {
	SessionStore
	observe func(ctx context.Context, id string) error
}

func (s observedBeforeFlip) ReactivateClaimed(ctx context.Context, id string, root []byte, tx string) (Reactivation, error) {
	if err := s.observe(ctx, id); err != nil {
		return Reactivation{}, err
	}
	return s.SessionStore.ReactivateClaimed(ctx, id, root, tx)
}

// The submission credits by what its own flip moved, not by what it read
// before: when the window close observed the claim in between, that flip
// credited it and the submission's finds the session claimed.
func TestClaimedIsNotCreditedTwiceWhenAnObservationLandsBetweenTheReadAndTheFlip(t *testing.T) {
	root := make([]byte, SMSTRootLen)
	claimMissingStores(t, func(t *testing.T, observer *SessionCoordinator, store SessionStore, service string) {
		ctx := context.Background()
		seedClaimable(t, store, "sess-between", service, SessionStateClaiming, "", "")
		before := claimedProbe(service)
		observed := false
		submitter := NewSessionCoordinator(testLogger(), observedBeforeFlip{store, func(ctx context.Context, id string) error {
			if observed {
				return nil
			}
			observed = true
			return observer.OnClaimObservedOnChain(ctx, id, root, "TXFOUND")
		}}, SMSTRecoveryConfig{SupplierAddress: "pokt1test"})
		submitter.SetPricer(perMillionPricer{})

		require.NoError(t, submitter.OnSessionClaimed(ctx, "sess-between", root, "TXSUBMIT"))
		require.True(t, observed, "premise: the observation ran between the read and the flip")
		require.Equal(t, before+3, claimedProbe(service), "credited once, by the observation's flip")
	})
}
