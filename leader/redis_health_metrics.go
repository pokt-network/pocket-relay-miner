package leader

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

var (
	redisUsedMemoryBytes = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "redis_used_memory_bytes",
			Help:      "Current Redis memory usage in bytes (from INFO MEMORY used_memory)",
		},
	)

	redisMaxMemoryBytes = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "redis_max_memory_bytes",
			Help:      "Configured Redis maxmemory in bytes (0 means no limit)",
		},
	)

	redisMemoryUsageRatio = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "redis_memory_usage_ratio",
			Help:      "Ratio of used_memory to maxmemory (0.0-1.0, -1 if maxmemory is not set)",
		},
	)
)
