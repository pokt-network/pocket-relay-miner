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
	supplierKeysActive = observability.SharedFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_keys_active",
			Help:      "Number of active supplier signing keys",
		},
	)

	keyReloadsTotal = observability.SharedFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "reloads_total",
			Help:      "Total number of key reloads",
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
