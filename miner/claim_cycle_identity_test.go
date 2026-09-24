//go:build test

package miner

import (
	"context"
	"sync"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// identitySpyStore records the state written for each session ID.
type identitySpyStore struct {
	SessionStore
	mu      sync.Mutex
	written map[string]SessionState
}

// Get answers with what the caller passed in: executeBatchedClaimTransition
// refreshes every session from Redis before calling the callback, to catch a
// session another miner already claimed. Returning nil here means "no fresher
// copy", which keeps the in-memory snapshot and leaves this test about the
// transition, not about the refresh.
func (s *identitySpyStore) Get(_ context.Context, _ string) (*SessionSnapshot, error) {
	return nil, nil
}

func (s *identitySpyStore) UpdateState(_ context.Context, sessionID string, newState SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written == nil {
		s.written = map[string]SessionState{}
	}
	s.written[sessionID] = newState
	return nil
}

// middleSkippedCallback claims the first and third sessions and skips the
// middle one, which is the shape that broke: the old slice was filled by a
// counter that only advanced for sessions that submitted, so two claims landed
// in positions 0 and 1 and the caller transitioned sessions 0 and 1 -- the
// skipped one among them.
type middleSkippedCallback struct {
	SessionLifecycleCallback
	claimed []string
}

func (c *middleSkippedCallback) OnSessionsNeedClaim(_ context.Context, _ []*SessionSnapshot) (ClaimCycleResult, error) {
	res := ClaimCycleResult{Claimed: map[string]struct{}{}}
	for _, id := range c.claimed {
		res.Claimed[id] = struct{}{}
	}
	return res, nil
}

func (c *middleSkippedCallback) OnSessionProved(_ context.Context, _ *SessionSnapshot) error {
	return nil
}

// TestExecuteBatchedClaimTransition_SkippedSessionIsNotResurrected pins the
// property Jorge set: measuring session A with session B must not be possible.
//
// The middle session is skipped by the callback, which means it already carries
// a TERMINAL state (claim_skipped) and its SMST is gone. UpdateState does not
// consult IsTerminal, so writing Claimed over it brings back a session that
// cannot be claimed -- and under the old positional slice that is exactly what
// happened, because the two real claims left-packed into positions 0 and 1.
//
// The assertions are by session ID, never by count: "two sessions were claimed"
// is true both when the right two are claimed and when the wrong two are.
//
// This pins the CONSUMER only: that the caller transitions exactly what it was
// told. Whether the producer names the RIGHT id is a separate property, covered
// by TestOnSessionsNeedClaim_EachSubmittedSessionIsNamedWithItsOwnID, which
// enters the real callback -- measured: naming validSnapshots[0].SessionID N
// times leaves THIS test green and that one red.
func TestExecuteBatchedClaimTransition_SkippedSessionIsNotResurrected(t *testing.T) {
	store := &identitySpyStore{}
	cb := &middleSkippedCallback{claimed: []string{"first", "third"}}
	m := &SessionLifecycleManager{
		logger:              logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sessionStore:        store,
		callback:            cb,
		config:              SessionLifecycleConfig{SupplierAddress: "pokt1identity"},
		activeSessions:      xsync.NewMap[string, *SessionSnapshot](),
		resumedUnsentClaims: xsync.NewMap[string, struct{}](),
	}

	sessions := []*SessionSnapshot{
		{SessionID: "first", State: SessionStateClaiming},
		{SessionID: "middle", State: SessionStateClaimSkipped},
		{SessionID: "third", State: SessionStateClaiming},
	}

	m.executeBatchedClaimTransition(context.Background(), sessions)

	// The skipped session keeps its terminal state: nothing was written for it.
	if got, ok := store.written["middle"]; ok {
		t.Fatalf("the skipped session is terminal and must not be transitioned, but %q was written", got)
	}
	// And the two real claims are the two that got written -- by name.
	for _, id := range []string{"first", "third"} {
		if got, ok := store.written[id]; !ok || got != SessionStateClaimed {
			t.Fatalf("session %q must be claimed, got %q (present=%v)", id, got, ok)
		}
	}
}
