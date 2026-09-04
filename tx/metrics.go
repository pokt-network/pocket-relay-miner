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
			Help:      "Total number of successful transaction broadcasts (a failed broadcast increments claim/proof error counters instead)",
		},
		[]string{"supplier"},
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

	// txBroadcastRejections counts CheckTx rejections by the chain's OWN
	// classification. res.TxResponse carries Codespace as well as Code and the
	// broadcast path used to discard both into a formatted string, so the only
	// way to ask "why is the chain refusing our transactions" was to read logs.
	//
	// Codespace is what makes Code meaningful: code 18 in codespace "sdk" is
	// ErrInvalidRequest, and for OUR transactions it covers exactly TWO
	// rejections, both of them our own defect:
	//
	//   - the unordered nonce was already used (sigverify.go:461-465);
	//   - "unordered tx ttl exceeds 10m0s" (sigverify.go:440-446).
	//
	// It is NOT three. The obvious third -- a deadline already passed -- never
	// reaches code 18: TxTimeoutHeightDecorator sits at position 4 of the ante
	// chain (x/auth/ante/ante.go:48) and rejects that with ErrTxTimeout, which
	// is code 42 (types/errors/errors.go:147); sigverify's equivalent check is
	// seven decorators later and is never reached. And the fourth -- a sequence
	// set on an unordered tx -- cannot happen here: signTx forces sequence 0.
	//
	// DURABILITY: this holds GIVEN the decorator order of the cosmos-sdk this
	// module pins. A version that reorders them turns the 42 back into an 18,
	// and this comment is then wrong rather than merely stale.
	//
	// Cardinality is bounded by the chain's own registry, not by our input.
	txBroadcastRejections = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			// No "tx_" prefix: the subsystem already is "tx", and no neighbour
			// in this file repeats it (broadcasts_total, claim_errors_total).
			Name: "broadcast_rejections_total",
			Help: "CheckTx rejections by tx type and the chain's codespace/code",
		},
		[]string{"tx_type", "codespace", "code"},
	)

	txClaimErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_errors_total",
			Help:      "Total number of claim submission errors",
		},
		[]string{"supplier"},
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
)
