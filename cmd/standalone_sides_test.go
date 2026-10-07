package cmd

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// fakeSide records what happens to it, in one log shared by both sides.
type fakeSide struct {
	name      string
	log       *eventLog
	startErr  error         // returned before serving
	panicking bool          // panics before serving
	failWhile chan error    // a value makes it stop on its own while serving
	served    chan struct{} // closed once serving
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func newFake(name string, log *eventLog) *fakeSide {
	return &fakeSide{name: name, log: log, failWhile: make(chan error, 1), served: make(chan struct{})}
}

func (f *fakeSide) side() side {
	return side{name: f.name, serve: func(_ context.Context, hooks sideHooks) error {
		f.log.add(f.name + ":build")
		if f.panicking {
			panic("boom")
		}
		if f.startErr != nil {
			return f.startErr
		}
		stop := hooks.started()
		f.log.add(f.name + ":serving")
		close(f.served)
		select {
		case <-stop:
			f.log.add(f.name + ":stopped")
			return nil
		case err := <-f.failWhile:
			f.log.add(f.name + ":failed")
			return err
		}
	}}
}

type sidesRun struct {
	miner, relayer *fakeSide
	log            *eventLog
	sigCh          chan os.Signal
	result         chan error
}

func startSidesRun(t *testing.T, configure func(miner, relayer *fakeSide)) *sidesRun {
	t.Helper()
	log := &eventLog{}
	r := &sidesRun{
		miner: newFake("miner", log), relayer: newFake("relayer", log), log: log,
		sigCh: make(chan os.Signal, 1), result: make(chan error, 1),
	}
	if configure != nil {
		configure(r.miner, r.relayer)
	}
	go func() {
		r.result <- runSides(context.Background(), zerolog.Nop(), r.miner.side(), r.relayer.side(), sideHooks{}, r.sigCh)
	}()
	return r
}

func TestRunSides_StartsTheMinerFirstAndStopsTheRelayerFirst(t *testing.T) {
	r := startSidesRun(t, nil)
	<-r.relayer.served
	r.sigCh <- syscall.SIGTERM

	require.NoError(t, <-r.result)
	require.Equal(t, []string{
		"miner:build", "miner:serving",
		"relayer:build", "relayer:serving",
		"relayer:stopped", "miner:stopped",
	}, r.log.all())
}

func TestRunSides_AMinerFailureStopsTheRelayerAndIsReturned(t *testing.T) {
	leaderLost := errors.New("leader controller failed")
	r := startSidesRun(t, nil)
	<-r.relayer.served
	r.miner.failWhile <- leaderLost

	err := <-r.result
	require.ErrorIs(t, err, leaderLost, "the process exits non-zero, as the miner subcommand does")
	require.Equal(t, []string{
		"miner:build", "miner:serving",
		"relayer:build", "relayer:serving",
		"miner:failed", "relayer:stopped",
	}, r.log.all())
}

func TestRunSides_ARelayerThatFailsToStartStopsTheMiner(t *testing.T) {
	bad := errors.New("proxy failed to start")
	r := startSidesRun(t, func(_, relayer *fakeSide) { relayer.startErr = bad })

	err := <-r.result
	require.ErrorIs(t, err, bad)
	require.Equal(t, []string{"miner:build", "miner:serving", "relayer:build", "miner:stopped"}, r.log.all())
}

func TestRunSides_AMinerThatFailsToStartNeverStartsTheRelayer(t *testing.T) {
	bad := errors.New("redis refused")
	r := startSidesRun(t, func(miner, _ *fakeSide) { miner.startErr = bad })

	err := <-r.result
	require.ErrorIs(t, err, bad)
	require.Equal(t, []string{"miner:build"}, r.log.all())
}

func TestRunSides_APanicIsAnErrorThatStopsTheOtherSide(t *testing.T) {
	r := startSidesRun(t, func(_, relayer *fakeSide) { relayer.panicking = true })

	err := <-r.result
	require.ErrorContains(t, err, "relayer panic: boom")
	require.Equal(t, []string{"miner:build", "miner:serving", "relayer:build", "miner:stopped"}, r.log.all())
}

func TestRunSides_ASideThatStopsOnItsOwnIsAnError(t *testing.T) {
	r := startSidesRun(t, nil)
	<-r.relayer.served
	r.relayer.failWhile <- nil

	err := <-r.result
	require.ErrorContains(t, err, "relayer stopped without being asked to")
	require.Equal(t, []string{
		"miner:build", "miner:serving",
		"relayer:build", "relayer:serving",
		"relayer:failed", "miner:stopped",
	}, r.log.all())
}

// One scrape serves both sides: a family gathered twice would fail it.
func TestStandaloneGatherer_GathersEachFamilyOnce(t *testing.T) {
	families, err := standaloneGatherer().Gather()
	require.NoError(t, err)
	seen := map[string]int{}
	for _, f := range families {
		seen[f.GetName()]++
	}
	require.Equal(t, 1, seen["go_goroutines"], "the Go collector is served once")
	require.Equal(t, 1, seen["process_cpu_seconds_total"], "the process collector is served once")
}
