//go:build test

package miner

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// settleSpyStore records every UpdateState call. The embedded interface is nil
// on purpose: any method this test does not expect panics with the method name
// rather than returning a zero value that a weak assertion could pass over.
type settleSpyStore struct {
	SessionStore
	mu      sync.Mutex
	updated map[string]SessionState
}

func (s *settleSpyStore) UpdateState(_ context.Context, sessionID string, newState SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updated == nil {
		s.updated = map[string]SessionState{}
	}
	s.updated[sessionID] = newState
	return nil
}

// partialProofCallback settles exactly the session IDs it is given and reports
// an error for the rest, which is the shape a multi-group cycle produces: one
// group reached the chain, another did not.
type partialProofCallback struct {
	SessionLifecycleCallback
	settle []string
	err    error

	mu    sync.Mutex
	prove []string
}

func (c *partialProofCallback) OnSessionsNeedProof(_ context.Context, _ []*SessionSnapshot) (ProofCycleResult, error) {
	res := ProofCycleResult{Settled: map[string]struct{}{}}
	for _, id := range c.settle {
		res.Settled[id] = struct{}{}
	}
	return res, c.err
}

func (c *partialProofCallback) OnSessionProved(_ context.Context, s *SessionSnapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prove = append(c.prove, s.SessionID)
	return nil
}

// TestExecuteBatchedProofTransition_OnlySettledSessionsAreProved pins the
// contract this commit introduces: a session the cycle did not settle is left
// alone, even when the cycle also returns an error.
//
// The assertion names WHICH sessions, not how many. Counting would pass on the
// exact inversion this guards against -- settling "b" and leaving "a" untouched
// is one update either way -- and the defect being prevented is a session
// recorded as Proved with no proof on-chain.
func TestExecuteBatchedProofTransition_OnlySettledSessionsAreProved(t *testing.T) {
	store := &settleSpyStore{}
	cb := &partialProofCallback{
		settle: []string{"a"},
		err:    errors.New("group for b failed"),
	}
	m := &SessionLifecycleManager{
		logger:              logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sessionStore:        store,
		callback:            cb,
		config:              SessionLifecycleConfig{SupplierAddress: "pokt1test"},
		activeSessions:      xsync.NewMap[string, *SessionSnapshot](),
		resumedUnsentClaims: xsync.NewMap[string, struct{}](),
	}

	m.executeBatchedProofTransition(context.Background(), []*SessionSnapshot{
		{SessionID: "a", State: SessionStateProving},
		{SessionID: "b", State: SessionStateProving},
	})

	if got, ok := store.updated["a"]; !ok || got != SessionStateProved {
		t.Fatalf("settled session a: want state %q written, got %q (present=%v)", SessionStateProved, got, ok)
	}
	if got, ok := store.updated["b"]; ok {
		t.Fatalf("unsettled session b must not be transitioned, but state %q was written", got)
	}
	if len(cb.prove) != 1 || cb.prove[0] != "a" {
		t.Fatalf("OnSessionProved must run for the settled session only, got %v", cb.prove)
	}
}

// TestBuildProofGroups_StableUnderTiedEndHeights covers the case the real
// network actually produces. Sessions are anchored to a global grid, so every
// group carries the SAME end height and the sort comparison is a tie on every
// pair -- meaning the ordering is decided entirely by the tiebreak, not by the
// sort key. Ordering by end height alone would leave this as undefined as the
// map it replaced.
func TestBuildProofGroups_StableUnderTiedEndHeights(t *testing.T) {
	snapshots := []*SessionSnapshot{
		{SessionID: "s1", SessionEndHeight: 100},
		{SessionID: "s2", SessionEndHeight: 100},
		{SessionID: "s3", SessionEndHeight: 100},
	}

	// Each snapshot is its own group now, so the ORDER of groups is observable;
	// when they merged into one group there was nothing to order.
	first := groupOnePerSession(snapshots)
	if len(first) != 3 {
		t.Fatalf("want 3 per-session groups, got %d", len(first))
	}
	want := []string{"s1", "s2", "s3"}
	for i, id := range want {
		if first[i][0].SessionID != id {
			t.Fatalf("group %d: want %q, got %q", i, id, first[i][0].SessionID)
		}
	}

	// Repeat: a map-backed implementation randomises iteration, so the same
	// input must keep producing the same sequence.
	for run := 0; run < 50; run++ {
		got := groupOnePerSession(snapshots)
		for i, id := range want {
			if got[i][0].SessionID != id {
				t.Fatalf("run %d, group %d: order not stable, want %q got %q", run, i, id, got[i][0].SessionID)
			}
		}
	}
}

// TestBuildProofGroups_EarlierWindowFirst covers the other half: when end
// heights DIFFER, the group whose proof window closes first must go first.
func TestBuildProofGroups_EarlierWindowFirst(t *testing.T) {
	groups := groupOnePerSession([]*SessionSnapshot{
		{SessionID: "late", SessionEndHeight: 200},
		{SessionID: "early", SessionEndHeight: 100},
	})

	if len(groups) != 2 {
		t.Fatalf("want 2 groups by end height, got %d", len(groups))
	}
	if groups[0][0].SessionID != "early" {
		t.Fatalf("group with the earlier window must be first, got %q", groups[0][0].SessionID)
	}
}

// TestBuildProofGroups_SessionsSharingAnEndHeightGetSeparateTransactions pins
// the property the unconditional split exists for: one group is one
// transaction, so N sessions must never share one.
//
// It needs its own name and its own test because the only case that
// DISCRIMINATES is sessions at the SAME end height -- with different heights,
// grouping and not grouping produce the same partition, so a test built from
// distinct heights stays green while proofs are being batched again. Same
// height is not an edge case here: it is the ordinary one, since sessions are
// anchored to a global grid.
//
// Until this existed the cardinality was held by the entry guard of a test
// named for ordering, which failed with "order not stable" -- a red that names
// a neighbouring symptom instead of the property, in a line that is there so
// the rest of the test makes sense rather than as the thing under test.
func TestBuildProofGroups_SessionsSharingAnEndHeightGetSeparateTransactions(t *testing.T) {
	const sharedEndHeight = 909320 // one height, the way the chain's grid produces them

	snapshots := []*SessionSnapshot{
		{SessionID: "s1", SessionEndHeight: sharedEndHeight},
		{SessionID: "s2", SessionEndHeight: sharedEndHeight},
		{SessionID: "s3", SessionEndHeight: sharedEndHeight},
	}

	groups := groupOnePerSession(snapshots)

	if len(groups) != len(snapshots) {
		t.Fatalf("sessions sharing an end height must each get their own transaction: want %d groups, got %d",
			len(snapshots), len(groups))
	}
	// Each group carries exactly one session: a partition of the right SIZE can
	// still put two sessions in one group and leave another empty.
	seen := map[string]bool{}
	for i, g := range groups {
		if len(g) != 1 {
			t.Fatalf("group %d must hold exactly one session, holds %d", i, len(g))
		}
		seen[g[0].SessionID] = true
	}
	// And every session is present: three groups of one that name the same
	// session twice would pass everything above.
	for _, s := range snapshots {
		if !seen[s.SessionID] {
			t.Fatalf("session %q got no transaction of its own", s.SessionID)
		}
	}
}
