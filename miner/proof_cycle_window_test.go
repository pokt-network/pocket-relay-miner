//go:build test

package miner

import (
	"context"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// heightedBlocks is mockBlockClient plus the one method waitForBlock needs to
// take its already-past-target shortcut: getBlockAtHeight type-asserts for
// GetBlockAtHeight and errors out when it is missing, which would abort the
// cycle at the wait instead of reaching the branch under test.
type heightedBlocks struct {
	mockBlockClient
}

func (h *heightedBlocks) GetBlockAtHeight(_ context.Context, height int64) (pocktclient.Block, error) {
	return &mockBlock{height: height, hash: []byte("seed-hash-for-proof-requirement")}, nil
}

// defaultParamsShared answers every height with the chain's default params, so
// the window arithmetic in the callback is the real one.
type defaultParamsShared struct {
	pocktclient.SharedQueryClient
	mu sync.Mutex
}

func (s *defaultParamsShared) GetParamsAtHeight(_ context.Context, _ int64) (*sharedtypes.Params, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := sharedtypes.DefaultParams()
	return &p, nil
}

// TestOnSessionsNeedProof_LateGroupStillGetsItsVerdictAfterAnEarlierOneClosed
// covers the abort that DISCRIMINATES between groups, which the params harness
// deliberately does not: a params failure at one height fails identically for
// everyone, so aborting there loses nothing that was not lost anyway. The
// window check reads the CURRENT height, so a later group can find its window
// shut precisely because the groups ahead of it consumed the time -- and that
// is the failure mode multiplying the groups makes reachable.
//
// The assertion is that the LATE group reaches a verdict, not merely that the
// loop continued. A group that is attempted and then dies leaving no state
// satisfies "the loop kept going" and still breaks the property: no session
// may end the cycle mute.
func TestOnSessionsNeedProof_LateGroupStillGetsItsVerdictAfterAnEarlierOneClosed(t *testing.T) {
	const supplier = "pokt1lategroup"

	blocks := &heightedBlocks{}
	blocks.currentHeight = 1_000_000 // far past both windows: both are closed

	lc := &LifecycleCallback{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient: &defaultParamsShared{},
		blockClient:  blocks,
	}

	// Distinct service IDs are what give the metric per-session identity: the
	// failure counter carries {supplier, service_id, reason} and no session
	// label, so without them "one group got a verdict" and "both did" read the
	// same on a bare count.
	// The IDs are SHORT on purpose: at 5 characters they are exactly what
	// panicked the bare SessionID[:16] slice this cycle used to carry. Long
	// IDs would step around the defect instead of standing on it.
	early := &SessionSnapshot{
		SessionID: "early", SessionEndHeight: 100,
		SupplierOperatorAddress: supplier, ServiceID: "svc-early",
		State: SessionStateProving,
	}
	late := &SessionSnapshot{
		SessionID: "late", SessionEndHeight: 200,
		SupplierOperatorAddress: supplier, ServiceID: "svc-late",
		State: SessionStateProving,
	}

	before := func(svc string) float64 {
		return testutil.ToFloat64(sessionsFailedTotal.WithLabelValues(supplier, svc, "proof_window_closed"))
	}
	earlyBefore, lateBefore := before("svc-early"), before("svc-late")

	result, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{early, late})

	if got := before("svc-early") - earlyBefore; got != 1 {
		t.Fatalf("the early group must be marked proof_window_closed, delta = %v", got)
	}
	if got := before("svc-late") - lateBefore; got != 1 {
		t.Fatalf("the LATE group must still reach its verdict after the earlier group closed; delta = %v", got)
	}
	if len(result.Settled) != 0 {
		t.Fatalf("a closed window settles nothing, got %v", result.Settled)
	}
	if err == nil {
		t.Fatal("both groups failed, so the cycle must report it")
	}
}

// TestFirstSessionIDsForLog covers the helper directly, because it is shared by
// the claim cycle and the proof cycle and only the proof one has a test that
// walks through it. Without this, the claim call site is guarded by code that
// nothing asserts.
func TestFirstSessionIDsForLog(t *testing.T) {
	sixteen := "0123456789abcdef"        // exactly the prefix length
	long := sixteen + "0123456789abcdef" // longer, must be truncated

	cases := []struct {
		name string
		ids  []string
		want []string
	}{
		{"shorter than the prefix is returned whole", []string{"early"}, []string{"early"}},
		{"empty id survives", []string{""}, []string{""}},
		{"exactly the prefix is not truncated", []string{sixteen}, []string{sixteen}},
		{"longer than the prefix is truncated", []string{long}, []string{sixteen + "..."}},
		{"no snapshots yields nothing", nil, []string{}},
		{
			"at most five, in order",
			[]string{"a", "b", "c", "d", "e", "f"},
			[]string{"a", "b", "c", "d", "e"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshots := make([]*SessionSnapshot, 0, len(tc.ids))
			for _, id := range tc.ids {
				snapshots = append(snapshots, &SessionSnapshot{SessionID: id})
			}

			got := firstSessionIDsForLog(snapshots)
			if len(got) != len(tc.want) {
				t.Fatalf("want %d ids %v, got %d: %v", len(tc.want), tc.want, len(got), got)
			}
			// Compared element by element: the rendered value is the point, and
			// a length check passes on every wrong truncation.
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("id %d: want %q, got %q", i, tc.want[i], got[i])
				}
			}
		})
	}
}
