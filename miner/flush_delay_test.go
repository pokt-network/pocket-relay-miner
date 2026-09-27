//go:build test

package miner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// TestStreamMsgID_ParseAndCompare pins the numeric ordering a lexicographic
// comparison would get backwards: "999-0" sorts AFTER "1000-0" as a string.
func TestStreamMsgID_ParseAndCompare(t *testing.T) {
	small, err := parseStreamMsgID("999-0")
	require.NoError(t, err)
	big, err := parseStreamMsgID("1000-0")
	require.NoError(t, err)

	require.True(t, small.before(big), "999-0 must sort before 1000-0 numerically")
	require.False(t, big.before(small))
	require.True(t, big.atLeast(small))
	require.False(t, small.atLeast(big))

	sameMsHigherSeq, err := parseStreamMsgID("1000-5")
	require.NoError(t, err)
	require.True(t, sameMsHigherSeq.atLeast(big), "same ms, higher seq must compare >=")
	require.True(t, big.atLeast(big), "a value is always atLeast itself")

	_, err = parseStreamMsgID("not-an-id-at-all-x")
	require.Error(t, err, "malformed id must not parse silently")
	_, err = parseStreamMsgID("noseparator")
	require.Error(t, err)
}

// TestHandleStreamMessage_ReclaimNeverMovesTheWatermark pins the third
// revision's fix: only IsReclaim==false deliveries may advance
// maxNonReclaimHandledMsgID. Reclaimed and self-pending redeliveries carry
// OLDER ids by construction (they are re-reads of something already in the
// stream), and if they were allowed to write the watermark, an old reclaim
// landing after a new live delivery would walk it backwards.
func TestHandleStreamMessage_ReclaimNeverMovesTheWatermark(t *testing.T) {
	f := newDrainFixture(t, 2)
	f.mgr.onRelay = func(context.Context, string, *transport.StreamMessage) error { return nil }

	// msgs[0].ID < msgs[1].ID: XAdd assigns strictly increasing ids.
	acked := f.mgr.handleStreamMessage(context.Background(), f.state, f.msgs[0])
	require.True(t, acked)

	handled, ok := f.state.loadMaxNonReclaimHandledMsgID()
	require.True(t, ok)
	wantFirst, err := parseStreamMsgID(f.msgs[0].ID)
	require.NoError(t, err)
	require.Equal(t, wantFirst, handled, "the watermark must equal the one live delivery processed so far")

	// Now a RECLAIM of the second (higher-ID) message. It must NOT advance
	// the watermark, even though its own ID is numerically higher than what
	// is currently recorded -- reclaims are excluded categorically, not just
	// when they would move it backwards.
	reclaimed := f.msgs[1]
	reclaimed.IsReclaim = true
	_ = f.mgr.handleStreamMessage(context.Background(), f.state, reclaimed)

	stillHandled, ok := f.state.loadMaxNonReclaimHandledMsgID()
	require.True(t, ok)
	require.Equal(t, wantFirst, stillHandled,
		"a reclaimed message must never advance the watermark, regardless of its own id")
}

// TestHandleStreamMessage_WatermarkAdvancesEvenOnTransientFailure pins the
// second revision's fix: the watermark write is a defer registered before any
// of handleStreamMessage's early returns, so a transient processing failure
// (handed back for retry, not acked) still advances it. Without the defer,
// only the success path at the bottom of the function would update it, and a
// backlog of failing relays would never let the flush-delay watermark move.
func TestHandleStreamMessage_WatermarkAdvancesEvenOnTransientFailure(t *testing.T) {
	f := newDrainFixture(t, 1)
	f.mgr.onRelay = func(context.Context, string, *transport.StreamMessage) error {
		return errors.New("transient backend hiccup")
	}

	acked := f.mgr.handleStreamMessage(context.Background(), f.state, f.msgs[0])
	require.False(t, acked, "a transient failure must not be acked")

	handled, ok := f.state.loadMaxNonReclaimHandledMsgID()
	require.True(t, ok, "the watermark must advance even though the relay was handed back for retry")
	want, err := parseStreamMsgID(f.msgs[0].ID)
	require.NoError(t, err)
	require.Equal(t, want, handled)
}

// TestRecordNonReclaimHandled_OlderIDAfterNewerDoesNotMoveItBack pins the CAS
// comparison itself, not the IsReclaim guard around it: recordNonReclaimHandled
// must refuse to move the watermark backwards even when called directly with
// an older id after a newer one, regardless of which caller decided to call
// it.
func TestRecordNonReclaimHandled_OlderIDAfterNewerDoesNotMoveItBack(t *testing.T) {
	state := &SupplierState{}
	newer, err := parseStreamMsgID("2000-0")
	require.NoError(t, err)
	older, err := parseStreamMsgID("1000-0")
	require.NoError(t, err)

	state.recordNonReclaimHandled(newer)
	state.recordNonReclaimHandled(older)

	handled, ok := state.loadMaxNonReclaimHandledMsgID()
	require.True(t, ok)
	require.Equal(t, newer, handled, "an older id arriving after a newer one must not move the watermark back")
}

// newFlushDelayManager builds a SessionLifecycleManager with only the fields
// awaitFlushWatermark reads, bypassing NewSessionLifecycleManager -- the same
// pattern claim_cycle_identity_test.go uses for executeBatchedClaimTransition.
func newFlushDelayManager(bc *mockBlockClient, handled func() (streamMsgID, bool), lastGen func(context.Context) (streamMsgID, bool, error)) *SessionLifecycleManager {
	return &SessionLifecycleManager{
		logger:                          logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:                          SessionLifecycleConfig{SupplierAddress: "pokt1flushdelay"},
		blockClient:                     bc,
		maxNonReclaimHandledMsgIDLookup: handled,
		lastGeneratedMsgIDLookup:        lastGen,
		flushDelay:                      FlushDelayConfig{PollInterval: 5 * time.Millisecond},
	}
}

func sessionsWithServiceID(serviceID string) []*SessionSnapshot {
	return []*SessionSnapshot{{SessionID: "s1", ServiceID: serviceID}}
}

// TestAwaitFlushWatermark_UnwiredIsANoOp pins that a flush delay that never
// got wired (nil lookups) behaves exactly as if it did not exist, rather
// than guessing.
func TestAwaitFlushWatermark_UnwiredIsANoOp(t *testing.T) {
	m := &SessionLifecycleManager{logger: logging.NewLoggerFromConfig(logging.DefaultConfig())}
	proceed := m.awaitFlushWatermark(context.Background(), sessionsWithServiceID("svc"), 100)
	require.True(t, proceed, "an unwired flush delay must never block a claim transition")
}

// TestAwaitFlushWatermark_Rule1_AlreadyPastCapSealsImmediately covers a miner
// that picks up a batch late (HA handoff, a restart, a skipped check): the
// live height is already at or past windowOpenHeight+2 when the wait starts,
// so it must seal with ZERO polling, never waiting the cap out from scratch.
func TestAwaitFlushWatermark_Rule1_AlreadyPastCapSealsImmediately(t *testing.T) {
	bc := &mockBlockClient{currentHeight: 102} // windowOpenHeight(100) + 2
	genCalls := atomic.Int32{}
	m := newFlushDelayManager(bc,
		func() (streamMsgID, bool) { return streamMsgID{}, false },
		func(context.Context) (streamMsgID, bool, error) {
			genCalls.Add(1)
			return streamMsgID{}, false, nil
		},
	)

	serviceID := "svc-late"
	capped := claimFlushCapped.WithLabelValues(serviceID)
	before := testutil.ToFloat64(capped)

	proceed := m.awaitFlushWatermark(context.Background(), sessionsWithServiceID(serviceID), 100)

	require.True(t, proceed)
	// Structural, not timing-based (a wall-clock upper bound would be flaky
	// under the load of other sessions sharing this machine): zero calls to
	// lastGeneratedMsgIDLookup is only possible if the function returned
	// before ever reaching the point that would ask what is left to arrive,
	// i.e. it took rule 1's immediate branch and never entered the poll loop.
	require.Equal(t, int32(0), genCalls.Load(), "arriving already past the cap must not even ask what is left to arrive")
	require.Equal(t, before+1, testutil.ToFloat64(capped), "sealing via the height cap must count as capped")
}

// TestAwaitFlushWatermark_Rule2_NothingMoreToArriveSealsWithZeroPolling covers
// the idle-supplier case: nothing generated by the stream is left unhandled,
// so the wait must seal immediately, without a single sleep.
func TestAwaitFlushWatermark_Rule2_NothingMoreToArriveSealsWithZeroPolling(t *testing.T) {
	bc := &mockBlockClient{currentHeight: 100} // well below the cap (102)
	handled := streamMsgID{ms: 5000, seq: 0}
	handledCalls := atomic.Int32{}
	genCalls := atomic.Int32{}
	m := newFlushDelayManager(bc,
		func() (streamMsgID, bool) { handledCalls.Add(1); return handled, true },
		func(context.Context) (streamMsgID, bool, error) { genCalls.Add(1); return handled, true, nil }, // caught up
	)
	// A deliberately huge poll interval: if rule 2's shortcut were removed,
	// the function would fall into rule 3's loop and block on this. Paired
	// with the short ctx timeout below, that would surface as proceed==false
	// (ctx died first) rather than a flaky wall-clock race -- a real defect
	// signal, not a timing coincidence.
	m.flushDelay.PollInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	proceed := m.awaitFlushWatermark(ctx, sessionsWithServiceID("svc"), 100)

	require.True(t, proceed, "removing rule 2's shortcut would fall into the poll loop and time out instead")
	// Structural: rule 3's poll loop calls both lookups repeatedly. Seeing
	// each called exactly once proves the function took the immediate rule-2
	// branch and never entered that loop -- immune to system load, unlike a
	// wall-clock upper bound would be.
	require.Equal(t, int32(1), genCalls.Load())
	require.Equal(t, int32(1), handledCalls.Load())
}

// TestAwaitFlushWatermark_Rule2_StreamNeverHadAnEntrySealsImmediately is rule
// 2's other leg: lastGeneratedMsgIDLookup reporting hasTarget=false (the
// stream never had anything) must also seal at once.
func TestAwaitFlushWatermark_Rule2_StreamNeverHadAnEntrySealsImmediately(t *testing.T) {
	bc := &mockBlockClient{currentHeight: 100}
	genCalls := atomic.Int32{}
	m := newFlushDelayManager(bc,
		func() (streamMsgID, bool) { return streamMsgID{}, false },
		func(context.Context) (streamMsgID, bool, error) { genCalls.Add(1); return streamMsgID{}, false, nil },
	)
	m.flushDelay.PollInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	proceed := m.awaitFlushWatermark(ctx, sessionsWithServiceID("svc"), 100)
	require.True(t, proceed)
	require.Equal(t, int32(1), genCalls.Load(), "must ask exactly once, then seal -- never poll on an empty stream")
}

// TestAwaitFlushWatermark_Rule3_WaitsThenSealsWhenTrafficCatchesUp is the
// backlog case: there IS more to arrive when the wait starts (target ahead of
// handled), and it must not seal until handled reaches that CAPTURED target.
func TestAwaitFlushWatermark_Rule3_WaitsThenSealsWhenTrafficCatchesUp(t *testing.T) {
	bc := &mockBlockClient{currentHeight: 100}
	target := streamMsgID{ms: 5000, seq: 0}

	// Traffic "arrives" on the poll loop's 3rd call to the handled lookup: the
	// lookup itself decides when, on a known call count, so nothing here races
	// a background goroutine against a sleep.
	const catchUpOnCall = 3
	pollCount := atomic.Int32{}
	genCalls := atomic.Int32{}
	m := newFlushDelayManager(bc,
		func() (streamMsgID, bool) {
			if pollCount.Add(1) < catchUpOnCall {
				return streamMsgID{}, false
			}
			return target, true
		},
		func(context.Context) (streamMsgID, bool, error) { genCalls.Add(1); return target, true, nil },
	)

	proceed := m.awaitFlushWatermark(context.Background(), sessionsWithServiceID("svc"), 100)

	require.True(t, proceed)
	require.Equal(t, int32(catchUpOnCall), pollCount.Load(), "must have polled exactly until the traffic caught up on the 3rd call")
	require.Equal(t, int32(1), genCalls.Load(), "the target must be captured once at the start of the wait, never re-read while polling")
}

// TestAwaitFlushWatermark_Rule3_CapHeightEndsAWaitThatNeverDrains covers a
// backlog that never drains: the cap height, not elapsed wall-clock time,
// must be what ends the wait.
func TestAwaitFlushWatermark_Rule3_CapHeightEndsAWaitThatNeverDrains(t *testing.T) {
	// heightSequence: the 1st read is Rule 1's own pre-loop check (100, under
	// cap), the 2nd and 3rd are loop iterations still under cap, and the 4th
	// reaches 102 (windowOpenHeight(100)+2) -- a known call count, not a
	// background goroutine racing a sleep against the poll loop.
	bc := &mockBlockClient{heightSequence: []int64{100, 100, 100, 102}}
	target := streamMsgID{ms: 999999999, seq: 0} // never reached

	m := newFlushDelayManager(bc,
		func() (streamMsgID, bool) { return streamMsgID{}, false }, // nothing ever handled
		func(context.Context) (streamMsgID, bool, error) { return target, true, nil },
	)

	serviceID := "svc-neverdrains"
	capped := claimFlushCapped.WithLabelValues(serviceID)
	before := testutil.ToFloat64(capped)

	proceed := m.awaitFlushWatermark(context.Background(), sessionsWithServiceID(serviceID), 100)

	require.True(t, proceed)
	require.Equal(t, before+1, testutil.ToFloat64(capped), "ending via the height cap must be counted")
}

// TestAwaitFlushWatermark_CtxCancelledAbortsWithoutSealing pins that a
// cancelled ctx makes the caller ABORT the transition (return false), not
// proceed to flush and claim on a dying context.
func TestAwaitFlushWatermark_CtxCancelledAbortsWithoutSealing(t *testing.T) {
	bc := &mockBlockClient{currentHeight: 100}
	target := streamMsgID{ms: 999999999, seq: 0}
	ctx, cancel := context.WithCancel(context.Background())

	// cancel() fires from inside the handled lookup's 2nd call (the 1st is
	// Rule 2's own pre-loop check; the 2nd is the loop's first iteration) --
	// a known point in the wait, not a background goroutine racing a sleep.
	calls := atomic.Int32{}
	m := newFlushDelayManager(bc,
		func() (streamMsgID, bool) {
			if calls.Add(1) == 2 {
				cancel()
			}
			return streamMsgID{}, false
		},
		func(context.Context) (streamMsgID, bool, error) { return target, true, nil },
	)

	proceed := m.awaitFlushWatermark(ctx, sessionsWithServiceID("svc"), 100)
	require.False(t, proceed, "a cancelled ctx must abort, not seal")
}

// wiringOrderStore is the minimal SessionStore a Start() call needs when one
// session is already Active with its claim window already open: GetBySupplier
// loads it, UpdateState is called once when it transitions to Claiming.
type wiringOrderStore struct {
	SessionStore
	initial *SessionSnapshot
}

func (s *wiringOrderStore) GetBySupplier(context.Context) ([]*SessionSnapshot, error) {
	return []*SessionSnapshot{s.initial}, nil
}

func (s *wiringOrderStore) Get(_ context.Context, sessionID string) (*SessionSnapshot, error) {
	if sessionID != s.initial.SessionID {
		return nil, nil
	}
	return s.initial, nil
}

func (s *wiringOrderStore) UpdateState(context.Context, string, SessionState) error {
	return nil
}

// wiringOrderCallback records that the claim transition was actually reached.
type wiringOrderCallback struct {
	SessionLifecycleCallback
	claimed chan struct{}
}

func (c *wiringOrderCallback) OnSessionsNeedClaim(context.Context, []*SessionSnapshot) (ClaimCycleResult, error) {
	close(c.claimed)
	return ClaimCycleResult{}, nil
}

// TestWiringBeforeStart_NoRaceWithASessionAlreadyPastItsWindowOpen covers an
// instance that loads a session ALREADY Active with its claim window ALREADY
// open: it reaches its first transition check inside Start() itself (the
// "late session prioritization" branch), on the goroutine calling Start() --
// and the flush-delay wait it spawns runs on ANOTHER goroutine immediately
// after. If the flush-delay lookups were wired after Start() returned, that
// goroutine's read would race this test's write. Run with -race: a passing
// build here is the only proof this ordering held.
func TestWiringBeforeStart_NoRaceWithASessionAlreadyPastItsWindowOpen(t *testing.T) {
	params := sharedtypes.DefaultParams()
	const sessionEndHeight = 100
	windowOpen := sharedtypes.GetClaimWindowOpenHeight(&params, sessionEndHeight)

	bc := &mockBlockClient{currentHeight: windowOpen} // window just opened
	store := &wiringOrderStore{initial: &SessionSnapshot{
		SessionID:               "already-active",
		SupplierOperatorAddress: "pokt1wiring",
		ServiceID:               "svc",
		SessionStartHeight:      sessionEndHeight - 3,
		SessionEndHeight:        sessionEndHeight,
		State:                   SessionStateActive,
	}}
	cb := &wiringOrderCallback{claimed: make(chan struct{})}
	pool := pond.NewPool(4)
	defer pool.StopAndWait()

	m := NewSessionLifecycleManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		store,
		&mockSharedQueryClient{},
		bc,
		cb,
		SessionLifecycleConfig{SupplierAddress: "pokt1wiring", CheckIntervalBlocks: 1},
		pool,
	)
	defer func() { _ = m.Close() }()

	// Wired BEFORE Start(), exactly as supplier_manager.go now does it. Rule 2
	// (nothing more to arrive) fires at once, so the test does not depend on
	// the wait's polling cadence.
	m.SetMaxNonReclaimHandledMsgIDLookup(func() (streamMsgID, bool) { return streamMsgID{}, false })
	m.SetLastGeneratedMsgIDLookup(func(context.Context) (streamMsgID, bool, error) {
		return streamMsgID{}, false, nil
	})

	require.NoError(t, m.Start(context.Background()))

	select {
	case <-cb.claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("the already-open session never reached the claim callback")
	}
}
