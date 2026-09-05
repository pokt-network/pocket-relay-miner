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
	early := &SessionSnapshot{
		SessionID: "early-session-0000000000000001", SessionEndHeight: 100,
		SupplierOperatorAddress: supplier, ServiceID: "svc-early",
		State: SessionStateProving,
	}
	late := &SessionSnapshot{
		SessionID: "late-session-00000000000000002", SessionEndHeight: 200,
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
