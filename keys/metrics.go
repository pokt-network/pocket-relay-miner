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

	// keyringUndecodableRecords is the count of .info records the keyring listed
	// but could not decode. It exists because load_errors_total cannot answer
	// the question an operator has here: that counter moves for every kind of
	// key failure, and it is a COUNTER, so "three records are broken right now"
	// and "three transient blips happened this hour" look the same. This is the
	// standing condition, and a supplier is losing service for every unit of it
	// until someone removes or repairs the file.
	keyringUndecodableRecords = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "undecodable_records",
			Help:      "Key records present on disk that the keyring could not decode",
		},
		[]string{"provider"},
	)

	// keysLastSuccessfulReload is the wall-clock time of the last reload that
	// completed. Without it a FROZEN key manager and a healthy idle one have the
	// same signature: Reload returns before it touches supplier_keys_active, so
	// that gauge holds its last good value, and reloads_total counts only
	// reloads that CHANGED something, which is flat on a quiet fleet either way.
	// The staleness of this series is the only thing that separates them.
	//
	// It is only alertable where reloads actually happen: with hot reload
	// disabled nothing drives Reload on a timer, so the series is stamped once
	// at startup and then stands still forever, which is indistinguishable from
	// the freeze. An alert on its age has to be scoped to fleets that run with
	// hot reload on.
	keysLastSuccessfulReload = observability.SharedFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "last_successful_reload_timestamp_seconds",
			Help:      "Unix timestamp of the last key reload that completed without abandoning",
		},
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
