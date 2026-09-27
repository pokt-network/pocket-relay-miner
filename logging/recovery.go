package logging

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
)

// PanicRecoveriesTotal tracks panic recoveries by component.
// Exported to allow other packages (e.g., middleware, interceptors) to
// increment it. NOT created through promauto: that would register it in
// the prometheus DEFAULT registry, which no binary in this repo serves —
// the panic signal would be written into a registry nobody scrapes.
// The observability package registers this collector into its
// SharedRegistry (served by both binaries) at init; logging cannot do
// that itself without an import cycle.
var PanicRecoveriesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "ha",
		Name:      "panic_recoveries_total",
		Help:      "Total number of panic recoveries by component",
	},
	[]string{"component"},
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
