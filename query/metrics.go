package query

import (
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/prometheus/client_golang/prometheus"
)

// Query metrics (reserved for future instrumentation)
var (
	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "queries_total",
			Help:      "Total number of chain queries",
		},
		[]string{"client", "method"},
	)

	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "query_errors_total",
			Help:      "Total number of query errors",
		},
		[]string{"client", "method"},
	)

	_ = observability.SharedFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "query_latency_seconds",
			Help:      "Query latency in seconds",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"client", "method"},
	)

	// Cache metrics (reserved for future instrumentation)
	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "cache_hits_total",
			Help:      "Total number of cache hits",
		},
		[]string{"client", "cache_type"},
	)

	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "cache_misses_total",
			Help:      "Total number of cache misses",
		},
		[]string{"client", "cache_type"},
	)

	_ = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "cache_size",
			Help:      "Current cache size (number of entries)",
		},
		[]string{"client", "cache_type"},
	)
)
