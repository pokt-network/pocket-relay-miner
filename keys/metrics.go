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

// SetSupplierKeysActive publishes how many supplier signing keys this process
// holds.
//
// It is exported because supplier_keys_active lives on the SHARED metric
// factory, so both binaries expose the series, but only the miner drives it
// through MultiProviderKeyManager.Reload. The relayer loads its signing keys
// straight from the KeyProviders (see buildKeyProviders in cmd_relayer.go) and
// never constructs a manager, so without this the relayer published a hard 0
// while holding a full key set -- measured 2026-08-21: 0 reported against 17
// keys actually loaded. A metric that reads "no keys" on a healthy relayer is
// worse than no metric, because an operator believes it.
func SetSupplierKeysActive(n int) {
	supplierKeysActive.Set(float64(n))
}
