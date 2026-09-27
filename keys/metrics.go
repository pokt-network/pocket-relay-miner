package keys

import (
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "keys"
)

var (
	// supplierKeysActive is driven only by MultiProviderKeyManager.Reload, which
	// both binaries now go through. It used to read a hard 0 on the relayer --
	// measured 2026-08-21: 0 reported against 17 keys actually loaded -- because
	// the relayer loaded its keys straight from the providers and never built a
	// manager. It does now (see cmd_relayer.go), so there is one writer and no
	// exported setter. A metric that reads "no keys" on a healthy relayer is
	// worse than no metric, because an operator believes it.
	supplierKeysActive = observability.SharedFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_keys_active",
			Help:      "Number of active supplier signing keys",
		},
	)

	// keyReloadsTotal counts reloads that CHANGED the key set -- a key added,
	// removed, or rotated -- not reload attempts. Reloads can be driven on a
	// timer over sources that cannot be watched, so counting attempts would
	// count ticks: a number that grows at a fixed rate on a fleet where nothing
	// happened, and cannot be alerted on. Nothing read this series when the
	// meaning was narrowed (grepped 2026-08-22: declared and incremented, no
	// dashboard, no alert).
	keyReloadsTotal = observability.SharedFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reloads_total",
			Help:      "Total number of key reloads that changed the key set (added, removed or rotated)",
		},
	)

	keyChangesTotal = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "changes_total",
			Help:      "Total number of key changes",
		},
		[]string{"type"}, // type: added, removed
	)

	keyLoadErrors = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "load_errors_total",
			Help:      "Total number of key load errors",
		},
		[]string{"provider"},
	)
)
