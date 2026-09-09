//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// managerWithStore builds the smallest SupplierManager that can answer the
// re-injection question: a store and a logger, nothing else.
func managerWithStore(t *testing.T, store RebroadcastStorage) *SupplierManager {
	t.Helper()
	return &SupplierManager{logger: testLogger(), rebroadcastStore: store}
}

// The decision that stands between re-injecting and signing again.
//
// Getting it wrong is not symmetric. Signing when we could have re-injected
// costs a signature and puts a second live transaction for one claim into the
// gossip -- which behind a load balancer can land on a node that saw neither of
// the others. Re-injecting bytes that have EXPIRED costs the attempt outright:
// the ante handler refuses them, and the resend that could have landed did not
// happen. So the table below is the feature, and every row is a different way
// of not knowing.
func TestSupplierManager_ReusableSignedTx(t *testing.T) {
	ctx := context.Background()
	const hash = "TX-REUSE"
	deadline := time.Unix(1_700_000_600, 0)

	for _, tt := range []struct {
		name      string
		store     func(t *testing.T) RebroadcastStorage
		chainNow  time.Time
		txHash    string
		wantReuse bool
	}{
		{
			name: "alive: the chain clock is still before the sealed deadline",
			store: func(t *testing.T) RebroadcastStorage {
				s := newMemRebroadcastStore()
				require.NoError(t, s.PutSignedTx(ctx, hash, []byte("signed"), deadline, 4321))
				return s
			},
			chainNow: deadline.Add(-time.Minute), txHash: hash, wantReuse: true,
		},
		{
			// The ordinary end of a window rather than an edge case: the derived
			// budget sits just under the SDK ceiling while the window is barely
			// longer, so bytes expire BEFORE their window closes.
			name: "expired: the deadline is sealed in the bytes and cannot be moved",
			store: func(t *testing.T) RebroadcastStorage {
				s := newMemRebroadcastStore()
				require.NoError(t, s.PutSignedTx(ctx, hash, []byte("signed"), deadline, 4321))
				return s
			},
			chainNow: deadline.Add(time.Second), txHash: hash, wantReuse: false,
		},
		{
			name: "exactly at the deadline is NOT alive",
			store: func(t *testing.T) RebroadcastStorage {
				s := newMemRebroadcastStore()
				require.NoError(t, s.PutSignedTx(ctx, hash, []byte("signed"), deadline, 4321))
				return s
			},
			chainNow: deadline, txHash: hash, wantReuse: false,
		},
		{
			// An unknown clock must never authorise re-injection: nobody can say
			// whether these bytes are alive, and guessing spends the attempt.
			name: "unknown chain clock: sign rather than guess",
			store: func(t *testing.T) RebroadcastStorage {
				s := newMemRebroadcastStore()
				require.NoError(t, s.PutSignedTx(ctx, hash, []byte("signed"), deadline, 4321))
				return s
			},
			chainNow: time.Time{}, txHash: hash, wantReuse: false,
		},
		{
			name:     "nothing cached: the first attempt always signs",
			store:    func(t *testing.T) RebroadcastStorage { return newMemRebroadcastStore() },
			chainNow: deadline.Add(-time.Minute), txHash: hash, wantReuse: false,
		},
		{
			// An entry that never reached the network has no original hash, so
			// there is nothing to look bytes up by.
			name: "no hash: nothing to look up",
			store: func(t *testing.T) RebroadcastStorage {
				s := newMemRebroadcastStore()
				require.NoError(t, s.PutSignedTx(ctx, hash, []byte("signed"), deadline, 4321))
				return s
			},
			chainNow: deadline.Add(-time.Minute), txHash: "", wantReuse: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := managerWithStore(t, tt.store(t))
			payload, ok := m.reusableSignedTx(ctx, tt.chainNow, tt.txHash)
			require.Equal(t, tt.wantReuse, ok)
			if !tt.wantReuse {
				require.Empty(t, payload.Bytes, "a refusal must hand back nothing to send")
				return
			}
			require.Equal(t, []byte("signed"), payload.Bytes,
				"the bytes must be handed back UNCHANGED: anything else is a "+
					"different transaction with a different nonce")
			require.Equal(t, hash, payload.Hash)
			require.Equal(t, int64(4321), payload.TimeoutHeight,
				"the height sealed into the bytes travels with them, so the "+
					"rejection log names what the transaction actually holds")
		})
	}
}

// A store that cannot be read is not the same as an empty one.
//
// Both end in signing, so the behaviour is identical and the DISTINCTION is the
// point: an outage reported as "nothing cached" would hide itself behind extra
// signatures forever, which is the same failure as a metric that reads zero
// because nobody is publishing it.
func TestSupplierManager_AnUnreadableStoreSignsRatherThanReinjects(t *testing.T) {
	m := managerWithStore(t, failingSignedTxStore{newMemRebroadcastStore()})
	_, ok := m.reusableSignedTx(context.Background(), time.Now().Add(time.Hour), "ANY")
	require.False(t, ok, "an unreadable cache must fall back to signing, never re-inject blindly")
}

type failingSignedTxStore struct{ RebroadcastStorage }

func (failingSignedTxStore) GetSignedTx(context.Context, string) ([]byte, time.Time, int64, error) {
	return nil, time.Time{}, 0, context.DeadlineExceeded
}
