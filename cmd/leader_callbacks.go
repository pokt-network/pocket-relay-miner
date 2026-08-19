package cmd

import (
	"context"
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// leaderControllerLifecycle is the slice of miner.LeaderController the leader
// election callbacks need, so the wiring can be tested with a controller
// whose Start/Close fail on demand.
type leaderControllerLifecycle interface {
	Start(ctx context.Context) error
	Close() error
}

// leaderCallbackRegistrar is the slice of leader.GlobalLeaderElector used by
// registerLeaderCallbacks.
type leaderCallbackRegistrar interface {
	OnElected(callback leader.LeadershipCallback)
	OnLost(callback leader.LeadershipCallback)
}

// registerLeaderCallbacks wires leader election to the controller lifecycle.
//
// The callbacks run in goroutines (GlobalLeaderElector invokes them via
// `go logging.RecoverGoRoutine(...)`), so a failure must never logger.Fatal:
// zerolog's Fatal calls os.Exit, which skips every deferred cleanup and the
// graceful shutdown path. Instead:
//   - a Start failure is sent to errCh (non-blocking; capacity 1 is enough —
//     the first error already shuts the process down) so the main goroutine
//     can stop in order and a standby can take over;
//   - a Close failure after losing leadership is logged and swallowed: the
//     process stays useful in standby (SupplierWorker keeps mining regardless
//     of leadership), so trading that for a close error would turn a leak
//     into an outage.
func registerLeaderCallbacks(
	logger logging.Logger,
	elector leaderCallbackRegistrar,
	controller leaderControllerLifecycle,
	errCh chan<- error,
) {
	elector.OnElected(func(ctx context.Context) {
		logger.Info().Msg("starting leader controller (became leader)")
		if startErr := controller.Start(ctx); startErr != nil {
			select {
			case errCh <- fmt.Errorf("failed to start leader controller: %w", startErr):
			default:
				// The main goroutine is already shutting down on an earlier
				// error; drop the duplicate rather than block this callback
				// (a blocked callback wedges the elector's WaitGroup).
				logger.Error().Err(startErr).Msg("leader controller start failed again during shutdown")
			}
		}
	})

	elector.OnLost(func(ctx context.Context) {
		logger.Info().Msg("stopping leader controller (lost leadership)")
		if closeErr := controller.Close(); closeErr != nil {
			logger.Error().Err(closeErr).Msg("failed to close leader controller after losing leadership - continuing in standby")
		}
	})
}
