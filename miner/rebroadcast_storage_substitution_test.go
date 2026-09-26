//go:build test

package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
)

// memRebroadcastStore implements RebroadcastStorage with maps and nothing else.
//
// It is the answer to "is RebroadcastStorage really an interface, or is it
// documentation?" in the only form that cannot drift: a second implementation
// that shares no code, no client and no dependency with the Redis one, driving
// the reconciler through a real pass.
//
// It reproduces the two contract details that are easy to lose when
// reimplementing, and that the reconciler relies on every block: List answers a
// group that does not exist with an EMPTY MAP and no error, and Delete of
// something absent is a no-op. An implementation that made either an error would
// satisfy the interface and break the caller.
type memRebroadcastStore struct {
	mu     sync.Mutex
	groups map[string]map[string][]byte
	// meta keeps the group tuple beside its key so ActiveGroups can answer
	// without parsing the key back apart. The Redis store reads its groups from
	// an index set for the same reason: a key is a place to put something, not a
	// record of what it was.
	meta map[string]RebroadcastGroup
	kind map[string]RebroadcastPhase
}

func newMemRebroadcastStore() *memRebroadcastStore {
	return &memRebroadcastStore{
		groups: map[string]map[string][]byte{},
		meta:   map[string]RebroadcastGroup{},
		kind:   map[string]RebroadcastPhase{},
	}
}

func (m *memRebroadcastStore) key(phase RebroadcastPhase, supplier string, sessionEnd int64) string {
	return fmt.Sprintf("%s|%s|%d", phase, supplier, sessionEnd)
}

func (m *memRebroadcastStore) Put(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(phase, supplier, sessionEnd)
	if m.groups[k] == nil {
		m.groups[k] = map[string][]byte{}
		m.meta[k] = RebroadcastGroup{Supplier: supplier, SessionEnd: sessionEnd}
		m.kind[k] = phase
	}
	m.groups[k][sessionID] = payload
	return nil
}

func (m *memRebroadcastStore) List(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]byte{}
	for id, v := range m.groups[m.key(phase, supplier, sessionEnd)] {
		out[id] = v
	}
	return out, nil
}

func (m *memRebroadcastStore) Delete(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.groups[m.key(phase, supplier, sessionEnd)], sessionID)
	return nil
}

func (m *memRebroadcastStore) CleanupIfEmpty(_ context.Context, phase RebroadcastPhase, supplier string, sessionEnd int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(phase, supplier, sessionEnd)
	if len(m.groups[k]) == 0 {
		delete(m.groups, k)
		delete(m.meta, k)
		delete(m.kind, k)
	}
	return nil
}

func (m *memRebroadcastStore) ActiveGroups(_ context.Context, phase RebroadcastPhase) ([]RebroadcastGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RebroadcastGroup
	for k, g := range m.meta {
		if m.kind[k] == phase {
			out = append(out, g)
		}
	}
	return out, nil
}

// The reconciler completes a resend against a store with no Redis in it.
//
// This is the checkable half of Jorge's question -- "if tomorrow we write one on
// an embedded DB, or in memory, would it work?" -- and the reason it is a test
// and not a claim is that the interface can be correct while the WIRING stays
// tied to the concrete type. That is precisely the state this commit found:
// every signature already spoke in domain types, and the constructor still
// demanded *RebroadcastStore, so nothing else could ever be handed in.
//
// If this stops compiling, someone tied the seam back to Redis.
func TestReconciler_RunsAgainstANonRedisStore(t *testing.T) {
	mem := newMemRebroadcastStore()
	resub := &mockResubmitter{}
	const supplier, sessionEnd, sessionID = "pokt1mem", int64(120), "s1"
	const submitHeight = int64(100)

	entry, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:     []byte("msg"),
		SubmitHeight: submitHeight,
		TxHash:       "tx-s1",
		OrigTxHash:   "tx-s1",
	})
	require.NoError(t, err)
	require.NoError(t, mem.Put(context.Background(), RebroadcastPhaseProof, supplier, sessionEnd, sessionID, entry))

	var resent []string
	var mu sync.Mutex
	mkPhase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return 200 },
			verdict:           phaseVerdict(p),
			recordOutcome: func(context.Context, rebroadcastEntry, string, int64, string, string, int64) error {
				return nil
			},
			recordRebroadcast: func(_, _, result string) {
				mu.Lock()
				defer mu.Unlock()
				resent = append(resent, result)
			},
		}
	}

	cfg := DefaultInclusionReconcilerConfig()
	cfg.MaxConcurrent = 2
	cfg.PerGroupTimeout = 2 * time.Second

	r := NewInclusionReconciler(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		&mockSharedQueryClient{},
		mem, // <- the whole point: not a *RebroadcastStore
		resub,
		mkPhase(RebroadcastPhaseClaim),
		mkPhase(RebroadcastPhaseProof),
		func(context.Context, string) (map[string]query.SessionClaim, error) {
			// Nothing on chain: the entry is missing, so the pass must resend.
			return map[string]query.SessionClaim{}, nil
		},
		cfg,
	)
	t.Cleanup(func() { _ = r.Close() })

	r.OnBlock(submitHeight + 1)

	require.Equal(t, 1, resub.attemptCount(),
		"the reconciler must have driven a resend reading its state from a store "+
			"that has never heard of Redis")
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, resent, "and it must have reported the attempt")
}
