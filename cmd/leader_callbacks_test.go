package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/leader"
)

// fakeElector records the callbacks the wiring registers so the tests can
// invoke them directly, the same way GlobalLeaderElector does (in a goroutine).
type fakeElector struct {
	onElected []leader.LeadershipCallback
	onLost    []leader.LeadershipCallback
}

func (f *fakeElector) OnElected(cb leader.LeadershipCallback) { f.onElected = append(f.onElected, cb) }
func (f *fakeElector) OnLost(cb leader.LeadershipCallback)    { f.onLost = append(f.onLost, cb) }

// fakeController fails Start/Close on demand.
type fakeController struct {
	startErr   error
	closeErr   error
	startCalls int
	closeCalls int
}

func (f *fakeController) Start(_ context.Context) error {
	f.startCalls++
	return f.startErr
}

func (f *fakeController) Close() error {
	f.closeCalls++
	return f.closeErr
}

// TestRegisterLeaderCallbacks_StartFailureReachesTheErrorChannel proves a
// leader-controller start failure is propagated to the main goroutine via the
// error channel instead of killing the process: the callback runs in a
// goroutine (leader/global_leader.go:289), where logger.Fatal would os.Exit
// and skip every deferred cleanup.
func TestRegisterLeaderCallbacks_StartFailureReachesTheErrorChannel(t *testing.T) {
	elector := &fakeElector{}
	sentinel := errors.New("redis exploded")
	controller := &fakeController{startErr: sentinel}
	errCh := make(chan error, 1)

	registerLeaderCallbacks(zerolog.Nop(), elector, controller, errCh)

	require.Len(t, elector.onElected, 1)
	require.Len(t, elector.onLost, 1)

	elector.onElected[0](context.Background())

	require.Equal(t, 1, controller.startCalls)
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, sentinel, "the channel must carry the start error, wrapped")
	default:
		t.Fatal("start failure never reached the error channel")
	}
}

// TestRegisterLeaderCallbacks_SecondStartFailureDoesNotBlock proves the
// callback never blocks on a full channel: if the main goroutine is already
// shutting down on an earlier error, a second failure is dropped, not
// deadlocked — a blocked callback would wedge GlobalLeaderElector's WaitGroup
// and hang its Close.
func TestRegisterLeaderCallbacks_SecondStartFailureDoesNotBlock(t *testing.T) {
	elector := &fakeElector{}
	controller := &fakeController{startErr: errors.New("still broken")}
	errCh := make(chan error, 1)

	registerLeaderCallbacks(zerolog.Nop(), elector, controller, errCh)

	elector.onElected[0](context.Background())
	// The channel (capacity 1) is now full. A second invocation must return
	// instead of blocking; run it synchronously so a block fails the test by
	// timeout rather than leaking a goroutine.
	elector.onElected[0](context.Background())

	require.Equal(t, 2, controller.startCalls)
	require.Len(t, errCh, 1, "the duplicate error must be dropped, not queued")
}

// TestRegisterLeaderCallbacks_CloseFailureKeepsTheProcessAlive proves a close
// failure after losing leadership is logged and swallowed: the process stays
// alive in standby (SupplierWorker keeps mining regardless of leadership), so
// nothing may reach the error channel and nothing may exit.
func TestRegisterLeaderCallbacks_CloseFailureKeepsTheProcessAlive(t *testing.T) {
	elector := &fakeElector{}
	controller := &fakeController{closeErr: errors.New("close exploded")}
	errCh := make(chan error, 1)

	registerLeaderCallbacks(zerolog.Nop(), elector, controller, errCh)

	elector.onLost[0](context.Background())

	require.Equal(t, 1, controller.closeCalls)
	require.Empty(t, errCh, "a close failure must not shut the process down")
}

// TestRegisterLeaderCallbacks_HappyPathSendsNothing pins that clean Start and
// Close leave the error channel empty.
func TestRegisterLeaderCallbacks_HappyPathSendsNothing(t *testing.T) {
	elector := &fakeElector{}
	controller := &fakeController{}
	errCh := make(chan error, 1)

	registerLeaderCallbacks(zerolog.Nop(), elector, controller, errCh)

	elector.onElected[0](context.Background())
	elector.onLost[0](context.Background())

	require.Equal(t, 1, controller.startCalls)
	require.Equal(t, 1, controller.closeCalls)
	require.Empty(t, errCh)
}
