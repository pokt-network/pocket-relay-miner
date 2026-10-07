package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

// side is one of the two components standalone runs: serve is serveMiner or
// serveRelayer with its config bound.
type side struct {
	name  string
	serve func(ctx context.Context, hooks sideHooks) error
}

// runningSide is a side started on its own goroutine.
type runningSide struct {
	name     string
	started  chan struct{} // closed when the side calls hooks.started
	stop     chan os.Signal
	stopOnce sync.Once
	done     chan error // receives once, when serve returns

	// abort cancels the side's context. Used only for a signal that arrives
	// while the side is still starting, the one moment requestStop cannot reach
	// it; a serving side is stopped through requestStop, so its components
	// close before their context ends, as in the subcommands.
	abort context.CancelFunc
}

func (r *runningSide) requestStop() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// startSide runs the side's serve on a goroutine. hooks.started is replaced:
// it reports the side as serving and hands it the channel requestStop closes.
// A panic in serve is turned into the error done reports, so a side that dies
// takes the process down with it rather than leaving the other side running.
func startSide(parent context.Context, logger logging.Logger, s side, hooks sideHooks) *runningSide {
	ctx, abort := context.WithCancel(parent)
	r := &runningSide{
		name:    s.name,
		started: make(chan struct{}),
		stop:    make(chan os.Signal),
		done:    make(chan error, 1),
		abort:   abort,
	}
	var startedOnce sync.Once
	hooks.started = func() <-chan os.Signal {
		startedOnce.Do(func() { close(r.started) })
		return r.stop
	}
	go logging.RecoverGoRoutine(logger, "standalone_"+s.name, func(c context.Context) {
		var err error
		defer func() {
			// Recovered here, not by RecoverGoRoutine, so the panic becomes the
			// error that stops the other side; logged the way it logs one.
			if p := recover(); p != nil {
				logging.PanicRecoveriesTotal.WithLabelValues("standalone_" + s.name).Inc()
				logger.Error().
					Str(logging.FieldComponent, "standalone_"+s.name).
					Str("panic_value", fmt.Sprintf("%v", p)).
					Str("stack_trace", string(debug.Stack())).
					Msg("PANIC RECOVERED in a standalone side: stopping the process")
				err = fmt.Errorf("%s panic: %v", s.name, p)
			}
			abort()
			r.done <- err
		}()
		err = s.serve(c, hooks)
	})(ctx)
	return r
}

// finish waits for a side that was asked to stop, or stopped on its own.
func (r *runningSide) finish(err error, stoppedOnItsOwn bool) error {
	if err != nil {
		return fmt.Errorf("%s: %w", r.name, err)
	}
	if stoppedOnItsOwn {
		return fmt.Errorf("%s stopped without being asked to", r.name)
	}
	return nil
}

// runSides runs first, then second once first is serving, until sigCh
// delivers or either side stops. It then stops second and waits for it, and
// only then stops first: the relayer drains what it serves before the miner it
// hands relays to goes away. A side that fails to start stops the one already
// running; a side that fails while serving stops the other. A signal while a
// side is still starting cancels that side's startup and stops the other in
// order; the process then exits cleanly.
func runSides(ctx context.Context, logger logging.Logger, first, second side, hooks sideHooks, sigCh <-chan os.Signal) error {
	a := startSide(ctx, logger, first, hooks)
	select {
	case <-a.started:
	case err := <-a.done:
		return a.finish(err, true)
	case <-sigCh:
		logger.Info().Str("side", a.name).Msg("shutdown signal received during startup, cancelling it")
		// Both: a side still starting sees its context end; one that started
		// in the meantime is stopped like any serving side.
		a.abort()
		a.requestStop()
		<-a.done
		return nil
	}

	b := startSide(ctx, logger, second, hooks)
	select {
	case <-b.started:
	case err := <-b.done:
		a.requestStop()
		return errors.Join(b.finish(err, true), a.finish(<-a.done, false))
	case <-sigCh:
		logger.Info().Str("side", b.name).Msg("shutdown signal received during startup, cancelling it")
		// Both: a side still starting sees its context end; one that started
		// in the meantime is stopped like any serving side.
		b.abort()
		b.requestStop()
		<-b.done
		a.requestStop()
		return a.finish(<-a.done, false)
	}

	var runErr error
	aDone, bDone := false, false
	select {
	case <-sigCh:
		logger.Info().Msg("shutdown signal received, stopping standalone...")
	case err := <-a.done:
		aDone = true
		runErr = a.finish(err, true)
	case err := <-b.done:
		bDone = true
		runErr = b.finish(err, true)
	}

	if !bDone {
		b.requestStop()
		runErr = errors.Join(runErr, b.finish(<-b.done, false))
	}
	if !aDone {
		a.requestStop()
		runErr = errors.Join(runErr, a.finish(<-a.done, false))
	}
	return runErr
}

// standaloneGatherer serves the miner, relayer and shared registries from one
// endpoint. Both side registries carry the Go and process collectors
// (observability/registry.go), and a family gathered twice fails the scrape,
// so the relayer's copy is dropped here; the two subcommands, which serve one
// side each, are unaffected.
func standaloneGatherer() prometheus.Gatherer {
	return prometheus.Gatherers{
		observability.MinerRegistry,
		dropFamilies{g: observability.RelayerRegistry, prefixes: []string{"go_", "process_"}},
		observability.SharedRegistry,
	}
}

// dropFamilies gathers g without the families whose name starts with a prefix.
type dropFamilies struct {
	g        prometheus.Gatherer
	prefixes []string
}

func (d dropFamilies) Gather() ([]*dto.MetricFamily, error) {
	families, err := d.g.Gather()
	kept := families[:0]
	for _, f := range families {
		drop := false
		for _, p := range d.prefixes {
			if strings.HasPrefix(f.GetName(), p) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, f)
		}
	}
	return kept, err
}
