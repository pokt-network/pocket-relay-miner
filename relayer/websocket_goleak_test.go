//go:build test

package relayer

import (
	"testing"
	"time"

	"go.uber.org/goleak"
)

// verifyNoBridgeGoroutines is the mechanical net this area was missing.
//
// Every defect this file's bridge has produced so far had the same tell -- a
// goroutine still running, or blocked forever, after the bridge was supposed to
// be gone -- and NO test asserted anything about goroutines. A readLoop wedged
// forever on a send into its 100-slot channel is invisible to every assertion
// about close codes and metrics, which is why it took three rounds of human
// review to find one instance and not the class.
//
// It is the check here that depends least on somebody predicting the failure
// mode first -- but it is not free of that dependency, and the ignore list is
// where the dependency lives. Every ignore below names a CONCRETE goroutine.
//
// Measured 2026-09-03: this list used to carry
// runtime's own". It was neither the runtime's nor harmless. IgnoreTopFunction
// matches the FIRST frame of the stack, and every goroutine parked in a socket
// read has exactly that frame on top -- including a leaked bridge readLoop
// sitting in conn.ReadMessage(), which is the canonical shape of a leaked
// connection and the shape this file exists to catch. So the net was blind in
// the half that mattered, in all 14 tests that use it.
//
// What it was actually covering, found by deleting it and reading the failure:
// ONE goroutine, github.com/godbus/dbus (*Conn).inWorker, created by Auth in
// goroutine 1 at process init -- it arrives through the keyring dependency, not
// through anything this package starts. It is named directly below.
func verifyNoBridgeGoroutines(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		// The bridge tears down asynchronously (closeWithReason cancels a
		// context that other goroutines observe), so a bare VerifyNone races
		// the teardown rather than the leak. goleak retries internally; this
		// only widens its patience for the 100ms close settle.
		goleak.VerifyNone(t,
			goleak.IgnoreTopFunction("github.com/redis/go-redis/v9/internal/pool.(*ConnPool).reaper"),
			// The D-Bus connection the keyring dependency opens at process init.
			// Named explicitly: a top-function ignore for its socket read
			// would also hide every leaked bridge readLoop.
			goleak.IgnoreAnyFunction("github.com/godbus/dbus.(*Conn).inWorker"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
			// A package-init timer wheel from a transitive dependency: one
			// goroutine for the life of the process, started before any test.
			goleak.IgnoreAnyFunction("github.com/desertbit/timer.timerRoutine"),
			// zerolog's async diode writer. NewLoggerFromConfig starts one per
			// logger and nothing ever Closes it -- checked, and it is NOT a
			// production leak: the only callers are process startup (cmd_relayer
			// twice, cmd_miner, the CLI) and there is no logging hot-reload, so
			// it is a fixed handful for the life of the process. In tests it is
			// one per helper call, which is noise, not signal.
			goleak.IgnoreAnyFunction("github.com/rs/zerolog/diode.Writer.poll"),
		)
	})
	// Give an already-registered t.Cleanup(bridge.Close) room to run first:
	// cleanups are LIFO, so this one registered FIRST runs LAST.
	_ = time.Millisecond
}
