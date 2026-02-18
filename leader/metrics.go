package leader

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "miner"
)

// leaderMetrics holds the leader election metrics
type leaderMetrics struct {
	status              *prometheus.GaugeVec
	elections           *prometheus.CounterVec
	losses              *prometheus.CounterVec
	acquisitionFailures *prometheus.CounterVec
}

var (
	metrics     *leaderMetrics
	metricsOnce sync.Once
)

// initMetrics initializes leader election metrics (lazy-loaded, only when GlobalLeaderElector is created)
func initMetrics() *leaderMetrics {
	metricsOnce.Do(func() {
		metrics = &leaderMetrics{
			status: observability.MinerFactory.NewGaugeVec(
				prometheus.GaugeOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "leader_status",
					Help:      "Whether this instance is the leader (1=leader, 0=standby)",
				},
				[]string{"instance"},
			),
			elections: observability.MinerFactory.NewCounterVec(
				prometheus.CounterOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "leader_elections_total",
					Help:      "Total number of times this instance became leader",
				},
				[]string{"instance"},
			),
			losses: observability.MinerFactory.NewCounterVec(
				prometheus.CounterOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "leader_losses_total",
					Help:      "Total number of times this instance lost leadership",
				},
				[]string{"instance"},
			),
			acquisitionFailures: observability.MinerFactory.NewCounterVec(
				prometheus.CounterOpts{
					Namespace: metricsNamespace,
					Subsystem: metricsSubsystem,
					Name:      "leader_acquisition_failures_total",
					Help:      "Total number of failed leadership acquisition attempts due to Redis errors",
				},
				[]string{"instance", "reason"},
			),
		}
	})
	return metrics
}
