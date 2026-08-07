package relay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	poktclient "github.com/pokt-network/poktroll/pkg/client"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// fakeBlockSource reports a height the test controls.
type fakeBlockSource struct {
	mu     sync.Mutex
	height int64
	block  poktclient.Block // when set, returned as-is (used for the nil-block case)
	nilOut bool
}

func (f *fakeBlockSource) setHeight(h int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.height = h
}

func (f *fakeBlockSource) LastBlock(context.Context) poktclient.Block {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.nilOut {
		return nil
	}
	if f.block != nil {
		return f.block
	}

	return client.NewSimpleBlock(f.height, nil, time.Time{})
}

// fakeSessionRenewer stands in for the relay client. It records what the
// monitor installed, so the test can assert the cache is never left empty.
type fakeSessionRenewer struct {
	mu sync.Mutex

	// sessionAtHeight is what GetSessionAtHeight returns; fetchErr wins if set.
	sessionAtHeight func(height int64) *sessiontypes.Session
	fetchErr        error

	fetchedHeights []int64
	installed      []*sessiontypes.Session
	cleared        int

	// installedCh signals every ReplaceCachedSession call.
	installedCh chan struct{}
}

func newFakeSessionRenewer() *fakeSessionRenewer {
	return &fakeSessionRenewer{installedCh: make(chan struct{}, 16)}
}

func (f *fakeSessionRenewer) GetSessionAtHeight(_ context.Context, serviceID string, height int64) (*sessiontypes.Session, error) {
	f.mu.Lock()
	f.fetchedHeights = append(f.fetchedHeights, height)
	err := f.fetchErr
	build := f.sessionAtHeight
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if build == nil {
		return nil, errors.New("fake has no session configured")
	}

	session := build(height)
	session.Header.ServiceId = serviceID

	return session, nil
}

func (f *fakeSessionRenewer) ReplaceCachedSession(session *sessiontypes.Session) {
	f.mu.Lock()
	f.installed = append(f.installed, session)
	f.mu.Unlock()

	select {
	case f.installedCh <- struct{}{}:
	default:
	}
}

func (f *fakeSessionRenewer) ClearSessionCache() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared++
}

func (f *fakeSessionRenewer) snapshot() (installed []*sessiontypes.Session, fetched []int64, cleared int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*sessiontypes.Session(nil), f.installed...),
		append([]int64(nil), f.fetchedHeights...),
		f.cleared
}

// sessionEndingAt builds the session a chain query at height would return.
func sessionEndingAt(sessionID string, endHeight int64) *sessiontypes.Session {
	return &sessiontypes.Session{
		Header: &sessiontypes.SessionHeader{
			SessionId:             sessionID,
			SessionEndBlockHeight: endHeight,
		},
	}
}

// waitForInstall blocks until the monitor installs a session, or fails the test.
func waitForInstall(t *testing.T, f *fakeSessionRenewer) {
	t.Helper()

	select {
	case <-f.installedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the monitor to install a session")
	}
}

// TestRenewSessionOnRollover_InstallsTheNewSession is the regression test for
// the load-test failure: at the boundary the monitor must hand the freshly
// fetched session to the client. Clearing the cache instead let the next relay
// re-read the expired session from the query layer, and everything after the
// boundary was rejected with "session expired ... (grace period elapsed)".
func TestRenewSessionOnRollover_InstallsTheNewSession(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	blocks := &fakeBlockSource{height: 302}
	renewer := newFakeSessionRenewer()
	renewer.sessionAtHeight = func(int64) *sessiontypes.Session {
		return sessionEndingAt("session-b", 310)
	}

	tracker := &sessionEndTracker{}
	tracker.set(300)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		renewSessionOnRollover(ctx, logger, renewer, blocks, tracker, time.Millisecond)
	}()

	waitForInstall(t, renewer)
	cancel()
	<-done

	installed, fetched, cleared := renewer.snapshot()

	require.NotEmpty(t, installed, "the monitor must install the new session")
	require.NotNil(t, installed[0], "the cache must never be left empty at a boundary")
	require.Equal(t, "session-b", installed[0].Header.SessionId)
	require.Equal(t, int64(310), installed[0].Header.SessionEndBlockHeight)

	require.Equal(t, 0, cleared,
		"a rollover replaces the session; clearing leaves a window where a stale read wins")

	require.NotEmpty(t, fetched)
	require.Equal(t, int64(302), fetched[0],
		"the new session must be fetched at an explicit height, not at height 0")

	require.Equal(t, int64(310), tracker.get(),
		"the tracker must advance so the monitor does not re-fire until the next boundary")
}

// TestRenewSessionOnRollover_FiresOncePerBoundary proves the monitor does not
// re-fetch on every tick while sitting inside the same session.
func TestRenewSessionOnRollover_FiresOncePerBoundary(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	blocks := &fakeBlockSource{height: 302}
	renewer := newFakeSessionRenewer()
	renewer.sessionAtHeight = func(int64) *sessiontypes.Session {
		return sessionEndingAt("session-b", 310)
	}

	tracker := &sessionEndTracker{}
	tracker.set(300)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		renewSessionOnRollover(ctx, logger, renewer, blocks, tracker, time.Millisecond)
	}()

	waitForInstall(t, renewer)

	// Many ticks pass at a height that is still inside the new session; none
	// of them may renew again.
	blocks.setHeight(305)
	require.Never(t, func() bool {
		installed, _, _ := renewer.snapshot()
		return len(installed) > 1
	}, 50*time.Millisecond, time.Millisecond)

	cancel()
	<-done

	installed, fetched, _ := renewer.snapshot()
	require.Len(t, installed, 1, "one boundary must produce exactly one renewal")
	require.Len(t, fetched, 1, "no chain query while inside the same session")
}

// TestRenewSessionOnRollover_KeepsTheOldSessionWhenTheFetchFails proves a failed
// renewal does not blank the cache: relays keep going against the old session
// (some may be rejected) instead of every one failing to build, and the monitor
// retries on the next tick.
func TestRenewSessionOnRollover_KeepsTheOldSessionWhenTheFetchFails(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	blocks := &fakeBlockSource{height: 302}
	renewer := newFakeSessionRenewer()
	renewer.fetchErr = errors.New("chain unreachable")

	tracker := &sessionEndTracker{}
	tracker.set(300)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		renewSessionOnRollover(ctx, logger, renewer, blocks, tracker, time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		_, fetched, _ := renewer.snapshot()
		return len(fetched) >= 2
	}, 5*time.Second, time.Millisecond, "the monitor must retry after a failed fetch")

	cancel()
	<-done

	installed, _, cleared := renewer.snapshot()
	require.Empty(t, installed, "a failed fetch must not install anything")
	require.Equal(t, 0, cleared, "a failed fetch must not empty the cache")
	require.Equal(t, int64(300), tracker.get(), "the tracker must not advance on failure")
}

// TestRenewSessionOnRollover_IgnoresAMissingBlock proves the monitor tolerates a
// block source that has not seen a block yet.
func TestRenewSessionOnRollover_IgnoresAMissingBlock(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	blocks := &fakeBlockSource{nilOut: true}
	renewer := newFakeSessionRenewer()

	tracker := &sessionEndTracker{}
	tracker.set(300)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	renewSessionOnRollover(ctx, logger, renewer, blocks, tracker, time.Millisecond)

	installed, fetched, cleared := renewer.snapshot()
	require.Empty(t, installed)
	require.Empty(t, fetched)
	require.Equal(t, 0, cleared)
}

// TestRenewSessionOnRollover_StopsOnContextCancel proves the monitor exits so
// the load test's WaitGroup can complete.
func TestRenewSessionOnRollover_StopsOnContextCancel(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	blocks := &fakeBlockSource{height: 100}
	renewer := newFakeSessionRenewer()

	tracker := &sessionEndTracker{}
	tracker.set(300)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		renewSessionOnRollover(ctx, logger, renewer, blocks, tracker, time.Millisecond)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor did not exit on context cancellation")
	}
}
