package logging

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// PanicRecoveriesTotal tracks panic recoveries by component.
	// Exported to allow other packages (e.g., middleware, interceptors) to increment it.
	PanicRecoveriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Name:      "panic_recoveries_total",
			Help:      "Total number of panic recoveries by component",
		},
		[]string{"component"},
	)
)

// RecoverGoRoutine wraps a goroutine with panic recovery and structured logging.
// Use this for ALL spawned goroutines to prevent crashes from propagating.
//
// The function logs panic details including
// - Component name
// - Panic value
// - Full stack trace
// - Prometheus metric increment
//
// Example usage:
//
//	go RecoverGoRoutine(logger, "cache_refresher", func(ctx context.Context) {
//	    // goroutine work here
//	    doWork(ctx)
//	})(ctx)
//
// The returned function takes a context parameter, allowing you to pass context
// at the goroutine spawn site rather than capturing it in the closure.
func RecoverGoRoutine(logger Logger, component string, fn func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		defer func() {
			if r := recover(); r != nil {
				PanicRecoveriesTotal.WithLabelValues(component).Inc()

				logger.Error().
					Str(FieldComponent, component).
					Str("panic_value", fmt.Sprintf("%v", r)).
					Str("stack_trace", string(debug.Stack())).
					Msg("PANIC RECOVERED in goroutine")
			}
		}()

		fn(ctx)
	}
}

// RecoverWithLogger wraps arbitrary functions with panic recovery and logging.
// Use this for non-goroutine code paths that need panic protection.
//
// Unlike RecoverGoRoutine, this is for synchronous code that you want to protect
// without spawning a goroutine.
//
// Example usage:
//
//	err := RecoverWithLogger(logger, "session_builder", "build_session", func() error {
//	    return buildSession(sessionID)
//	})
//	if err != nil {
//	    // Handle error (could be from panic recovery or function error)
//	}
//
// The function returns the original error from fn(), or creates a new error if
// a panic occurred.
func RecoverWithLogger(logger Logger, component string, operation string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			PanicRecoveriesTotal.WithLabelValues(component).Inc()

			logger.Error().
				Str(FieldComponent, component).
				Str(FieldOperation, operation).
				Str("panic_value", fmt.Sprintf("%v", r)).
				Str("stack_trace", string(debug.Stack())).
				Msg("PANIC RECOVERED")

			// Convert panic to error
			err = fmt.Errorf("panic recovered: %v", r)
		}
	}()

	return fn()
}
