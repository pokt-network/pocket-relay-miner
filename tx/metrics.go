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

	// txPermitWait is how long a broadcast waited for a concurrency permit.
	//
	// This -- not broadcast latency -- is the metric that would justify moving
	// MaxConcurrent: if nobody waits, the cap costs nothing and raising it buys
	// nothing. A wait in the tail is the evidence, and it is also the moment to
	// ask whether the answer is a bigger cap or a healthier node.
	txPermitWait = observability.MinerFactory.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "permit_wait_seconds",
			Help:      "Time a broadcast waited for a transaction-concurrency permit",
			// From 100us (an uncontended acquire is immediate) outwards: a
			// contended one waits for a whole broadcast to finish.
			Buckets: prometheus.ExponentialBuckets(0.0001, 3, 12),
		},
	)

	// txPermitSaturatedTotal counts calls that never reached the chain because
	// no broadcast permit was free.
	//
	// It exists because the alternative was silence. The permit-wait histogram
	// cannot carry this: a caller that refuses to queue -- the reconciler's
	// resend -- waits zero, so it would land in the same bucket as a healthy
	// uncontended acquire. Saturation is exactly the condition the resend
	// exists for, and a starving safety net has to be audible.
	txPermitSaturatedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "permit_saturated_total",
			Help:      "Broadcasts that never reached the chain because no concurrency permit was free",
		},
		[]string{"waited"},
	)

	// txConnVerified is 1 while the transaction path is known to carry RPCs and
	// 0 once no connection can. A counter cannot answer "has this EVER worked
	// since the process started", and that is the question an operator asks
	// first when no claim has landed.
	//
	// With a pool it aggregates: 1 means at least one member answered, which is
	// the honest reading of "can we still submit". It deliberately does NOT say
	// whether the pool is degraded -- conn_member_verified below is what
	// answers which member died, and this one keeps meaning what the alerts
	// built on it already assume.
	txConnVerified = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "conn_verified",
			Help:      "1 if the last transaction-connection probe succeeded, 0 if it failed",
		},
	)

	// txConnMemberVerified is txConnVerified per pool member, and it exists
	// because the aggregate cannot answer "which one".
	//
	// With one connection a bare gauge was enough. With a pool it reads 1 or 0
	// depending on which member happened to be probed last -- an observation
	// that cannot distinguish the case it is watched for. The label is the
	// member index, which is stable for the life of the process because
	// members are appended and never removed or reordered, and its cardinality
	// is the pool size: one series per eighty suppliers, against the many
	// metrics in this process that already carry one series per supplier.
	txConnMemberVerified = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "conn_member_verified",
			Help:      "1 if the last probe of this pool connection succeeded, 0 if it failed",
		},
		[]string{"conn"},
	)

	// txConnProbeFailures counts failed probes. reason is bounded to
	// startup|tick|warmup: the same failure at startup and at hour six mean
	// different things -- one is a bad config, the other is a connection that
	// died while idle -- and warmup is a third, a connection the pool added for
	// new leases that never came up.
	txConnProbeFailures = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "conn_probe_failures_total",
			Help:      "Failed transaction-connection probes, by when the probe ran",
		},
		[]string{"reason"},
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

	// txProofNotRequired counts the times the chain refused a proof the local
	// oracle had decided was required. That divergence has never been measured:
	// until now it was swallowed and reported as a successful submission, so
	// there is no number for how often it happens or whether it concentrates in
	// one supplier.
	//
	// No EagerCounterChildren call for this one, and that is not an oversight:
	// the label is the supplier address, which cannot be enumerated at init.
	// Same as the three counters around it.
	txProofNotRequired = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_not_required_total",
			Help:      "Proof submissions the chain refused as not required, which the local oracle had judged required",
		},
		[]string{"supplier"},
	)

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

// The failure counters above exist to make a silent failure audible, so they
// must not be silent themselves: a *Vec with no children exports nothing, and
// "the series is absent" reads as "not instrumented" rather than "it never
// happened". See observability.EagerCounterChildren for the full reasoning --
// written once there rather than repeated at each declaration.
func init() {
	observability.EagerCounterChildren(txConnProbeFailures, probeReasonStartup, probeReasonTick, probeReasonWarmup)
	observability.EagerCounterChildren(txPermitSaturatedTotal, permitWaited, permitDidNotWait)
}
