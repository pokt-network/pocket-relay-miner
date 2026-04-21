package tx

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "tx"
)

var (
	// Transaction broadcast metrics
	txBroadcastsTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "broadcasts_total",
			Help:      "Total number of transaction broadcasts",
		},
		[]string{"supplier", "status"},
	)

	txBroadcastLatency = observability.MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "broadcast_latency_seconds",
			Help:      "Transaction broadcast latency in seconds",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"supplier"},
	)

	// Claim metrics
	txClaimsSubmitted = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_submitted_total",
			Help:      "Total number of claims submitted",
		},
		[]string{"supplier"},
	)

	txClaimErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_errors_total",
			Help:      "Total number of claim submission errors",
		},
		[]string{"supplier", "error_type"},
	)

	// Proof metrics
	txProofsSubmitted = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proofs_submitted_total",
			Help:      "Total number of proofs submitted",
		},
		[]string{"supplier"},
	)

	txProofErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_errors_total",
			Help:      "Total number of proof submission errors",
		},
		[]string{"supplier", "error_type"},
	)

	// Account query metrics (reserved for future instrumentation)
	_ = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "account_queries_total",
			Help:      "Total number of account queries",
		},
		[]string{"supplier", "source"},
	)

	// Sequence tracking (reserved for future instrumentation)
	_ = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sequence_number",
			Help:      "Current sequence number for each supplier",
		},
		[]string{"supplier"},
	)

	// NOTE: Gas tracking metrics (txGasUsed, txGasWanted, txActualFeeUpokt) removed
	// because we use SYNC broadcast mode which returns after CheckTx only.
	// These metrics would require BLOCK mode which waits for TX execution.

	txInsufficientBalanceErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "insufficient_balance_errors_total",
			Help:      "Total number of transactions failed due to insufficient balance",
		},
		[]string{"supplier"},
	)

	// txInclusionOutcomeTotal records the real on-chain fate of each tx after
	// broadcast. Because broadcasts use BROADCAST_MODE_SYNC (mempool-only), the
	// txBroadcastsTotal "success" label only captures CheckTx acceptance. This
	// metric captures DeliverTx execution via an async GetTx poll. Outcomes
	// (bounded enum):
	//   included_success — tx landed in a block with code==0
	//   included_failure — tx landed in a block with code!=0 (DeliverTx rejected)
	//   mempool_timeout  — poll horizon elapsed before inclusion
	//   poll_error       — GetTx poll itself failed (e.g. RPC offline)
	txInclusionOutcomeTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_outcome_total",
			Help:      "Post-broadcast on-chain tx inclusion outcome (labeled by supplier, tx_type, outcome)",
		},
		[]string{"supplier", "tx_type", "outcome"},
	)

	// txInclusionPollLatency measures the time from broadcast to poll resolution.
	// Short latencies with included_success are the normal path; long latencies
	// with mempool_timeout indicate chain congestion or node issues.
	txInclusionPollLatency = observability.MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_poll_latency_seconds",
			Help:      "Time from broadcast to poll resolution",
			Buckets:   []float64{1, 2, 5, 10, 20, 30, 60, 120, 180, 300},
		},
		[]string{"supplier", "tx_type", "outcome"},
	)
)
