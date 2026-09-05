//go:build test

package miner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// heightSpyShared records the session end heights OnSessionsNeedProof asks
// params for, and fails every one of them. Failing all of them is what keeps
// this harness to three fields: every group dies at the loop's FIRST hop, so no
// block client, proof checker, supplier client or worker pool is ever reached.
//
// The embedded interface is nil deliberately: any other method panics by name
// instead of quietly returning a zero value.
type heightSpyShared struct {
	pocktclient.SharedQueryClient
	mu     sync.Mutex
	asked  []int64
	failWi error
}

func (s *heightSpyShared) GetParamsAtHeight(_ context.Context, height int64) (*sharedtypes.Params, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, height)
	return nil, s.failWi
}

func (s *heightSpyShared) askedHeights() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.asked...)
}

// TestOnSessionsNeedProof_AGroupFailingDoesNotAbandonTheGroupsBehindIt is the
// harness the earlier tests did not have: it enters OnSessionsNeedProof itself,
// whose control flow IS the fix. buildProofGroups is a pure function and the
// caller was covered with a double; neither reaches the ten aborts that decide
// whether a failing group ends the cycle.
//
// The assertion is about WHICH heights were asked for, not how many calls
// happened. A count is satisfied by retrying the first group twice; only the
// identity of the second height shows the loop got past the first failure.
func TestOnSessionsNeedProof_AGroupFailingDoesNotAbandonTheGroupsBehindIt(t *testing.T) {
	spy := &heightSpyShared{failWi: errors.New("params unavailable at this height")}
	lc := &LifecycleCallback{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sharedClient: spy,
	}

	// Two groups, because the property is about the SECOND one surviving the
	// first one's failure. Distinct end heights keep them in separate groups
	// without depending on the per-session workaround.
	result, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{
		{SessionID: "early", SessionEndHeight: 100, State: SessionStateProving},
		{SessionID: "late", SessionEndHeight: 200, State: SessionStateProving},
	})

	asked := spy.askedHeights()
	if len(asked) != 2 || asked[0] != 100 || asked[1] != 200 {
		t.Fatalf("both groups must be attempted in window order despite the first failing; asked heights = %v", asked)
	}

	// The cycle settled nothing, and says so rather than reporting success.
	if len(result.Settled) != 0 {
		t.Fatalf("no group reached the chain, so Settled must be empty, got %v", result.Settled)
	}
	if err == nil {
		t.Fatal("every group failed, so the cycle must return an error")
	}

	// errors.Join must carry BOTH failures: reporting only the first is the
	// shape that made the old code look like it had handled the cycle.
	msg := err.Error()
	if strings.Count(msg, "params unavailable at this height") != 2 {
		t.Fatalf("the aggregated error must name every failed group, got: %s", msg)
	}
	if !strings.Contains(msg, "100") || !strings.Contains(msg, "200") {
		t.Fatalf("the aggregated error must identify which heights failed, got: %s", msg)
	}
}
