//go:build test

package miner

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A rejection tells the operator that the chain refused the proof, and nothing
// about WHY: the reason lives in the FailureReason of an EndBlocker event, and
// the claim carries four fields with no room for it. The one question answerable
// from here is whether the root the chain holds for that claim is the root this
// miner stored, and it splits the seven causes into two classes with different
// responses -- a mismatch means what we hold is not what we claimed and no proof
// built from it can pass, a match means the claim was right and the proof failed
// on construction or a signature.
//
// The three cases are driven through the real closure the proof phase is wired
// to, not a copy of it, so what is measured is what production runs.
func TestDiagnoseProofRejection_TheRootsSplitTheCauses(t *testing.T) {
	const (
		supplier  = "pokt1diagnose"
		sessionID = "sess-rejected"
	)
	// EXACTLY SMSTRootLen bytes, because the session store drops a persisted
	// ClaimedRootHash of any other length on read -- a wrong-length root panics
	// the smt library on import. A shorter literal here would come back nil and
	// every case would quietly diagnose root_unknown, which is a test measuring
	// its own fixture.
	ourRoot := bytes.Repeat([]byte("a"), SMSTRootLen)

	tests := []struct {
		name        string
		storedRoot  []byte // nil = no snapshot written at all
		onChainRoot []byte
		// breakRedis makes the snapshot READ fail, which is a different thing
		// from the snapshot being absent: one is "there is nothing to compare
		// against", the other is "I could not find out".
		breakRedis bool
		wantCause  string
		why        string
	}{
		{
			name:        "roots agree",
			storedRoot:  ourRoot,
			onChainRoot: ourRoot,
			wantCause:   rejectionRootMatch,
			why:         "the claim committed to the tree we hold: the fault is in building or signing the proof",
		},
		{
			name:        "roots differ",
			storedRoot:  bytes.Repeat([]byte("b"), SMSTRootLen),
			onChainRoot: ourRoot,
			wantCause:   rejectionRootMismatch,
			why:         "what we hold is not what we claimed, so no proof built from it can ever pass",
		},
		{
			name:        "no snapshot left to compare against",
			storedRoot:  nil,
			onChainRoot: ourRoot,
			wantCause:   rejectionRootUnknown,
			why: "the session record expires on SessionTTL and both that and the block time are " +
				"configurable, so its absence is a case and not an error -- reporting a match or a " +
				"mismatch here would be inventing one",
		},
		{
			name:        "the snapshot could not be read",
			storedRoot:  ourRoot,
			onChainRoot: ourRoot,
			breakRedis:  true,
			wantCause:   rejectionRootUnknown,
			why: "a FAILED read is not a mismatch. The roots here are identical, so anything " +
				"but root_unknown would be inventing a verdict out of an outage -- and " +
				"root_mismatch specifically would send the operator to audit a tree that is " +
				"perfectly fine. That is the same shape S10 exists to remove, one level in: a " +
				"thing we do not know, reported as a thing we found out",
		},
		{
			name:        "the chain carried no root",
			storedRoot:  ourRoot,
			onChainRoot: nil,
			wantCause:   rejectionRootUnknown,
			why:         "nothing to compare against on the other side either",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, _ := newTestRedis(t)
			failRedis := testredis.NewFailSwitch(rc)
			buf := &syncBuf{}

			lg := logging.NewLoggerFromConfig(logging.DefaultConfig())
			store := NewRedisSessionStore(lg, rc, SessionStoreConfig{SessionTTL: time.Hour})
			t.Cleanup(func() { _ = store.Close() })

			m := &SupplierManager{
				logger:    zerolog.New(buf).Level(zerolog.TraceLevel),
				suppliers: xsync.NewMap[string, *SupplierState](),
				config: SupplierManagerConfig{
					RedisClient:           rc,
					ProofQueryClient:      inclusionProbe{},
					BlockClient:           &mockBlockClient{},
					SubmissionTrackingTTL: time.Hour,
				},
			}
			m.suppliers.Store(supplier, &SupplierState{SessionStore: store})
			m.ensureSharedTrackers()
			t.Cleanup(func() {
				if m.reconcilerCancel != nil {
					m.reconcilerCancel()
				}
				m.reconcilerWG.Wait()
			})

			if tt.storedRoot != nil {
				require.NoError(t, store.Save(context.Background(), &SessionSnapshot{
					SessionID:               sessionID,
					SupplierOperatorAddress: supplier,
					ClaimedRootHash:         tt.storedRoot,
				}))
			}

			before := testutil.ToFloat64(proofRejectionDiagnosisTotal.WithLabelValues(tt.wantCause))

			if tt.breakRedis {
				// Break every command AFTER the snapshot is written, so the read
				// is what fails and not the setup.
				failRedis.Fail("LOADING Redis is loading the dataset in memory")
				t.Cleanup(failRedis.Clear)
			}

			diagnose := m.inclusionReconciler.proofPhase.diagnoseRejection
			require.NotNil(t, diagnose, "the proof phase must carry the diagnosis, or nothing runs it")
			diagnose(context.Background(), rebroadcastEntry{TxHash: "tx-1"}, supplier, hEnd, sessionID, testMid, tt.onChainRoot)

			require.Equal(t, before+1,
				testutil.ToFloat64(proofRejectionDiagnosisTotal.WithLabelValues(tt.wantCause)),
				"%s", tt.why)

			logged := buf.String()
			require.Contains(t, logged, tt.wantCause, "the cause has to reach the operator, not only Prometheus")
			require.Contains(t, logged, "will not be re-sent",
				"the log must say the resends stopped: an operator who reads only 'rejected' "+
					"will wait for a retry that is never coming")
			require.True(t, strings.Contains(logged, `"level":"warn"`),
				"Warn and not Debug: once per rejected session, bounded by the defect, and it is money; got %s", logged)
		})
	}
}
