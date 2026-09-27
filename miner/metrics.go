package miner

import (
	"context"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "miner"
)

var (
	// Relay flow tracking metrics (for debugging SMST sealing issues)
	relaysConsumedFromStream = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_consumed_from_stream_total",
			Help:      "Total number of relays consumed from Redis Streams (relayer → miner)",
		},
		[]string{"supplier", "service_id"},
	)

	// relaysAddedToSMST counts UpdateTree CALLS that returned nil — i.e.,
	// the relay's bytes were written into the SMST backing store. It does
	// NOT count unique SMST leaves: when two relays share the same
	// RelayHash (dedup by protocol-key), UpdateTree succeeds for both but
	// only one leaf survives in the claimed root. For the number of
	// billable leaves at claim time, see ha_miner_claim_num_leaves.
	relaysAddedToSMST = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_added_to_smst_total",
			Help:      "SMST UpdateTree successes (NOT unique leaves — see claim_num_leaves for billable count)",
		},
		// session_id deliberately excluded: it is an unbounded value on a
		// Counter (never DeleteLabelValues'd) → TSDB OOM. Per-session detail
		// lives in logs and the claim_num_leaves gauges (which DO clean up).
		[]string{"supplier", "service_id"},
	)

	// orphanedStreams is the number of relay streams whose supplier this
	// deployment has no record of, as counted by the leader.
	//
	// It is a Gauge because it is a level, not an event: the same orphan is
	// counted again on every sweep. Zero labels on purpose -- the supplier
	// address is the obvious thing to want here and is exactly what must not be
	// a label, since it is unbounded; the addresses go to the log line the sweep
	// emits, and to `redis streams --orphaned`.
	orphanedStreams = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "orphaned_streams",
			Help:      "Relay streams whose supplier is no longer known to this deployment (leader-reported; bookkeeping, not lost relays)",
		},
	)

	// shutdownDrainedRelays counts relays pulled out of the delivery buffer during a
	// GRACEFUL shutdown and processed to completion, and those abandoned because the
	// drain window closed first.
	//
	// Abandoned is not the same as lost: an abandoned entry is still in the consumer
	// group's pending list, so the reclaim on a surviving or restarted miner picks it
	// up once it passes claim_idle_timeout. What IS lost is only what a SIGKILL or a
	// crash leaves behind, because no drain runs at all there -- which is the whole
	// reason the graceful path bothers to drain.
	shutdownDrainedRelays = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "shutdown_drained_relays_total",
			Help:      "Relays drained from the delivery buffer and processed during graceful shutdown",
		},
		[]string{"supplier"},
	)

	shutdownAbandonedRelays = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "shutdown_abandoned_relays_total",
			Help:      "Relays still buffered when the graceful drain window closed; they stay pending and are recovered by the reclaim, not lost",
		},
		[]string{"supplier"},
	)

	// claimNumLeaves is the number of distinct SMST leaves in the claim
	// being submitted (equals EventClaimCreated.num_relays on-chain).
	// Paired with claimRelayAttempts below, the delta exposes collapses
	// caused by identical-bytes relays sharing a key (e.g. a subscription
	// fan-out where every event body is byte-identical).
	claimNumLeaves = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_num_leaves",
			Help:      "Distinct SMST leaves in the claim being submitted (matches on-chain num_relays)",
		},
		[]string{"supplier", "service_id", "session_id"},
	)

	// claimRelayAttempts is the session coordinator's RelayCount at claim
	// time — how many relays the relayer successfully mined into the
	// session before sealing. Compare against claim_num_leaves to detect
	// dedup-by-key collapses without tailing logs.
	claimRelayAttempts = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_relay_attempts",
			Help:      "Session coordinator RelayCount at claim time (compare with claim_num_leaves to detect collapse)",
		},
		[]string{"supplier", "service_id", "session_id"},
	)

	// claimLeafCollapseTotal fires once per claim whose SMST leaf count
	// was strictly less than the coordinator's RelayCount — i.e., the
	// relayer processed N relays but only M < N made it into the claim
	// because their relay bytes collided on the SMST key. Graph this
	// against 0 in prod; any increase means legitimate work is not being
	// paid out (or a replay fraud attempt was correctly deduped).
	claimLeafCollapseTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_leaf_collapse_total",
			Help:      "Claims whose SMST leaf count was less than the coordinator RelayCount (dedup collapse)",
		},
		[]string{"supplier", "service_id"},
	)

	relaysFailedSMST = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_failed_smst_total",
			Help:      "Total number of relays that failed to add to SMST tree",
		},
		// session_id excluded (unbounded on a Counter); reason is bounded.
		[]string{"supplier", "service_id", "reason"},
	)

	// Relay consumption metrics
	relaysRejected = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_rejected_total",
			Help:      "Total number of relays rejected due to errors",
		},
		[]string{"supplier", "reason", "service_id"},
	)

	relayProcessingLatency = observability.MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_processing_latency_seconds",
			Help:      "Time to process a single relay",
			Buckets:   []float64{0.05, 0.1, 0.5, 1, 2, 3, 5, 7, 10},
		},
		[]string{"supplier", "service_id", "status_code"},
	)

	// ====== OPERATOR-FOCUSED METRICS ======

	// Claim timing metrics - helps operators verify timing spread
	claimScheduledHeight = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_scheduled_height",
			Help:      "Block height when claim is scheduled to be submitted",
		},
		[]string{"supplier", "service_id", "session_id"},
	)

	claimSubmissionLatencyBlocks = observability.MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_submission_latency_blocks",
			Help:      "Blocks after claim window opened when claim was submitted",
			Buckets:   []float64{0, 1, 2, 3, 4, 5, 10, 15, 20},
		},
		[]string{"supplier"},
	)

	// Proof timing metrics
	proofScheduledHeight = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_scheduled_height",
			Help:      "Block height when proof is scheduled to be submitted",
		},
		[]string{"supplier", "service_id", "session_id"},
	)

	proofSubmissionLatencyBlocks = observability.MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_submission_latency_blocks",
			Help:      "Blocks after proof window opened when proof was submitted",
			Buckets:   []float64{0, 1, 2, 3, 4, 5, 10, 15, 20},
		},
		[]string{"supplier"},
	)

	// Session lifecycle totals - useful for SLIs/SLOs
	sessionsCreatedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_created_total",
			Help:      "Total number of sessions created",
		},
		[]string{"supplier", "service_id"},
	)

	sessionsProvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_proved_total",
			Help:      "Total sessions explicitly proved (proof TX submitted)",
		},
		[]string{"supplier", "service_id"},
	)

	sessionsProbabilisticProvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_probabilistic_proved_total",
			Help:      "Total sessions probabilistically proved (no proof required)",
		},
		[]string{"supplier", "service_id"},
	)

	sessionsFailedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_failed_total",
			Help:      "Total sessions failed by specific reason (claim_window_closed, claim_tx_error, proof_window_closed, proof_tx_error)",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// ====== REVENUE TRACKING METRICS ======
	// These metrics track the complete lifecycle of revenue: claimed -> proved -> lost
	// Available in 3 views: Compute Units (protocol), uPOKT (revenue), Relays (workload)

	// Compute Units - Protocol's unit of work
	computeUnitsClaimedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_claimed_total",
			Help:      "Total compute units successfully claimed (claim tx accepted on-chain)",
		},
		[]string{"supplier", "service_id"},
	)

	computeUnitsProvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_proved_total",
			Help:      "Total compute units successfully proved (proof tx accepted on-chain or proof not required)",
		},
		[]string{"supplier", "service_id"},
	)

	computeUnitsLostTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_lost_total",
			Help:      "Total compute units lost due to claim/proof failures",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// uPOKT - Revenue view (compute units = uPOKT, 1:1 mapping)
	upoktClaimedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_claimed_total",
			Help:      "Total uPOKT successfully claimed (compute units * service rate)",
		},
		[]string{"supplier", "service_id"},
	)

	upoktProvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_proved_total",
			Help:      "Total uPOKT successfully proved (revenue that will be settled)",
		},
		[]string{"supplier", "service_id"},
	)

	upoktLostTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_lost_total",
			Help:      "Total uPOKT lost due to claim/proof failures (compute units = uPOKT, 1:1)",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// Relays - Workload view (number of relays processed)
	relaysClaimedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_claimed_total",
			Help:      "Total relays successfully claimed",
		},
		[]string{"supplier", "service_id"},
	)

	relaysProvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_proved_total",
			Help:      "Total relays successfully proved",
		},
		[]string{"supplier", "service_id"},
	)

	relaysLostTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_lost_total",
			Help:      "Total relays lost due to claim/proof failures",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// claimsSkippedTotal tracks claim submissions that were intentionally
	// dropped before going to the chain. reason values (extend as new skip
	// paths land):
	//   - "unprofitable"     → reward < claim_fee + proof_fee
	//   - "already_submitted" (future) → dedup hit
	//   - "zero_relays"      (future) → session had nothing to claim
	//   - "zero_compute"     (future) → relays present but CUs=0
	claimsSkippedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_skipped_total",
			Help:      "Claims intentionally not submitted. reason carries the cause (e.g. unprofitable, zero_relays).",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// Deduplication metrics. The deduplicator only runs on the reclaim path
	// (redelivery of an entry stranded in a dead consumer's pending list), so
	// hit volume is expected to be near-zero in normal operation and non-zero
	// only after consumer crashes.
	// session_id deliberately excluded from all dedup counters: it is an
	// unbounded value on Counters (never DeleteLabelValues'd) and
	// dedupMisses/dedupMarked fire on every new relay (hot path) → TSDB OOM.
	// Aggregate rates are the actionable signal; per-session goes to logs.
	dedupRedisCacheHits = observability.MinerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_redis_cache_hits_total",
			Help:      "Total number of reclaimed relays detected as already-processed (prevented double-count)",
		},
	)

	dedupMisses = observability.MinerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_misses_total",
			Help:      "Total number of deduplication cache misses (new relays)",
		},
	)

	dedupMarked = observability.MinerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_marked_total",
			Help:      "Total number of relays marked as processed",
		},
	)

	dedupErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_errors_total",
			Help:      "Total number of deduplication errors",
		},
		// session_id excluded; operation is bounded (redis_check/redis_mark/redis_batch_mark).
		[]string{"operation"},
	)

	// Claim and proof metrics
	claimsCreated = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_created_total",
			Help:      "Total claims built into a submission batch (attempts; see claims_submitted_total / claim_errors_total)",
		},
		[]string{"supplier", "service_id"},
	)

	claimsSubmitted = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_submitted_total",
			Help:      "Total number of claims submitted on-chain",
		},
		[]string{"supplier", "service_id"},
	)

	claimErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_errors_total",
			Help:      "Total number of claim errors",
		},
		[]string{"supplier", "reason"},
	)

	proofsCreated = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proofs_created_total",
			Help:      "Total proofs built into a submission batch (attempts; see proofs_submitted_total)",
		},
		[]string{"supplier", "service_id"},
	)

	proofsSubmitted = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proofs_submitted_total",
			Help:      "Total number of proofs submitted on-chain",
		},
		[]string{"supplier", "service_id"},
	)

	proofErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_errors_total",
			Help:      "Total number of proof errors",
		},
		[]string{"supplier", "reason"},
	)

	// Proof requirement metrics - tracks probabilistic proof selection
	proofRequirementChecks = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_requirement_checks_total",
			Help:      "Total number of proof requirement checks performed",
		},
		[]string{"supplier"},
	)

	proofRequirementRequired = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_requirement_required_total",
			Help:      "Total number of proofs determined to be required (threshold or probabilistic)",
		},
		[]string{"supplier", "reason"},
	)

	proofRequirementSkipped = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_requirement_skipped_total",
			Help:      "Total number of proofs skipped (not required)",
		},
		[]string{"supplier"},
	)

	proofRequirementErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_requirement_errors_total",
			Help:      "Total number of errors during proof requirement checking",
		},
		[]string{"supplier", "operation"},
	)

	// proofSkippedTotal counts proofs NOT submitted because of a pre-submission
	// condition (not because of proof-requirement probability). Reason enum:
	//   claim_missing_on_chain — pre-proof GetClaim guard found no on-chain claim
	proofSkippedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_skipped_total",
			Help:      "Total number of proofs skipped due to a pre-submission condition (labeled by reason)",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// claimInclusionOutcomeTotal records the real on-chain fate of each
	// broadcast claim as observed by the inclusion reconciler. It
	// polls GetClaim(supplier, sessionID) until the claim appears or the
	// claim window closes — independent of the Tendermint tx indexer, so it
	// works on nodes configured with tx_index=null.
	//
	// Outcomes (bounded enum):
	//   on_chain_found   — claim is on-chain
	//   on_chain_missing — claim window closed without the claim landing
	//   poll_error       — GetClaim kept erroring through the poll horizon
	//   poll_dropped     — inclusion pool queue saturated; no record taken
	claimInclusionOutcomeTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_inclusion_outcome_total",
			Help:      "Post-broadcast on-chain claim inclusion outcome (labeled by supplier, service_id, outcome)",
		},
		[]string{"supplier", "service_id", "outcome"},
	)

	// proofInclusionOutcomeTotal records the real on-chain fate of each
	// broadcast proof as observed by the inclusion reconciler. It polls
	// GetProof(supplier, sessionID) until the proof appears or the proof
	// window closes — the proof-side analogue of claimInclusionOutcomeTotal.
	//
	// Outcomes (bounded enum):
	//   on_chain_found   — proof is on-chain (possibly after a rebroadcast)
	//   on_chain_missing — proof window closed without the proof landing
	//   poll_error       — GetProof kept erroring through the poll horizon
	//   poll_dropped     — inclusion pool queue saturated; no record taken
	proofInclusionOutcomeTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_inclusion_outcome_total",
			Help:      "Post-broadcast on-chain proof inclusion outcome (labeled by supplier, service_id, outcome)",
		},
		[]string{"supplier", "service_id", "outcome"},
	)

	// proofRebroadcastsTotal counts in-window proof re-submissions triggered
	// by the inclusion reconciler when a proof was CheckTx-accepted but not
	// yet on-chain and the proof window was still open.
	proofRebroadcastsTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_rebroadcasts_total",
			Help:      "In-window proof re-submissions attempted by the inclusion reconciler (labeled by supplier, service_id, result)",
		},
		[]string{"supplier", "service_id", "result"},
	)

	// claimRebroadcastsTotal counts in-window claim re-submissions triggered
	// by the inclusion reconciler when a claim was CheckTx-accepted but not
	// yet on-chain and the claim window was still open. Claim-side analogue of
	// proofRebroadcastsTotal (claims previously had inclusion observation but
	// no rebroadcast).
	claimRebroadcastsTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_rebroadcasts_total",
			Help:      "In-window claim re-submissions attempted by the inclusion reconciler (labeled by supplier, service_id, result)",
		},
		[]string{"supplier", "service_id", "result"},
	)

	// Block height
	currentBlockHeight = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "current_block_height",
			Help:      "Current block height as seen by the miner",
		},
	)

	// sessionBlockProcessingLag is how many blocks the per-supplier session
	// lifecycle processor advanced in a single pass (latest height minus the
	// previously processed height). Steady state is 1. A sustained value > 1
	// means a processing pass is taking longer than block time and the loop is
	// coalescing block events to stay at the chain head — an early signal that
	// the supplier's work (or Redis/RPC behind it) can't keep up, before it
	// shows as missed claim/proof windows.
	sessionBlockProcessingLag = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_block_processing_lag",
			Help:      "Blocks advanced per session-lifecycle processing pass (1 = keeping up; >1 = coalescing to head under load)",
		},
		[]string{"supplier"},
	)

	// sessionTransitionQueueDepth is the number of tasks waiting in a supplier's
	// transition worker subpool (claim/proof build+submit work that the lifecycle
	// dispatched but no worker has picked up yet). The subpool queue is unbounded
	// by design — it absorbs window-open bursts rather than blocking ingestion —
	// so this gauge is the backpressure signal: a steadily growing depth means
	// work is being produced faster than it settles (Redis/RPC/tx throughput
	// behind it), and is the metric that makes an eventual OOM explainable. Pairs
	// with session_block_processing_lag (which signals tick-processing falling
	// behind) to separate "can't keep up reading blocks" from "can't keep up doing
	// the work".
	sessionTransitionQueueDepth = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_transition_queue_depth",
			Help:      "Tasks waiting in a supplier's transition worker subpool (unbounded; growth = work outpacing settlement, the OOM early-warning)",
		},
		[]string{"supplier"},
	)

	// Block health metrics
	configuredBlockTimeSeconds = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "configured_block_time_seconds",
			Help:      "Configured expected block time in seconds",
		},
	)

	currentBlockIntervalSeconds = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "current_block_interval_seconds",
			Help:      "Actual time between the last two blocks in seconds",
		},
	)

	fullnodeSlowBlocksTotal = observability.MinerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "fullnode_slow_blocks_total",
			Help:      "Total number of slow blocks detected (block time > configured time × threshold)",
		},
	)

	fullnodeSlowBlocksConsecutive = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "fullnode_slow_blocks_consecutive",
			Help:      "Number of consecutive slow blocks currently detected (resets when block time normalizes)",
		},
	)

	// Session store metrics
	sessionSnapshotsSaved = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_snapshots_saved_total",
			Help:      "Total number of session snapshots saved to Redis",
		},
		[]string{"supplier"},
	)

	sessionSnapshotsLoaded = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_snapshots_loaded_total",
			Help:      "Total number of session snapshots loaded from Redis",
		},
		[]string{"supplier"},
	)

	sessionSnapshotsSkippedAtStartup = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_snapshots_skipped_at_startup_total",
			Help:      "Total number of session snapshots skipped at startup (expired or settled)",
		},
		[]string{"supplier", "state"},
	)

	sessionStoreErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_store_errors_total",
			Help:      "Total number of session store errors",
		},
		[]string{"supplier", "operation"},
	)

	sessionStateTransitions = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_state_transitions_total",
			Help:      "Total number of session state transitions",
		},
		[]string{"supplier", "service_id", "from_state", "to_state"},
	)

	// Supplier manager metrics
	supplierManagerSuppliersActive = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_manager_suppliers_active",
			Help:      "Number of active suppliers in the supplier manager",
		},
	)

	// supplierCacheWriteSkipped counts how many times the supplier manager
	// deliberately skipped a SetSupplierState write because the source data
	// was unreliable (e.g. chain query failed). Used to detect transient
	// fullnode outages that would otherwise cause the supplier cache to
	// drift to Staked:true + Services:[] and break all relays for a
	// supplier until a full restart.
	supplierCacheWriteSkipped = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_cache_write_skipped_total",
			Help:      "Number of times a supplier cache write was skipped to preserve existing state (e.g. chain query error)",
		},
		[]string{"reason"}, // reason: chain_query_error
	)

	// supplierBootServicesFallback counts supplier-services RESOLUTIONS (one
	// per supplier per pass, across warmup and reconcile) that used the
	// denormalized snapshot because no block height was observed yet. A boot
	// produces one short burst (~number of suppliers); a SUSTAINED rate
	// afterwards means Redis block events are not reaching this miner.
	// Deployments without a BlockClient (tooling) never increment it.
	supplierBootServicesFallback = observability.MinerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_boot_services_fallback_total",
			Help:      "Supplier service resolutions that fell back to the denormalized snapshot because block height was unknown (boot burst expected; sustained rate = block events not flowing)",
		},
	)

	// Supplier registry metrics
	supplierRegistryUpdatesTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_registry_updates_total",
			Help:      "Total number of supplier registry updates",
		},
		[]string{"action"},
	)

	// Balance monitor metrics
	supplierBalanceUpokt = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_balance_upokt",
			Help:      "Current account balance in uPOKT for each supplier",
		},
		[]string{"supplier"},
	)

	supplierStakeUpokt = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_stake_upokt",
			Help:      "Current staked amount in uPOKT for each supplier",
		},
		[]string{"supplier"},
	)

	supplierBalanceHealthStatus = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_balance_health_status",
			Help:      "Balance health status: 0=critical (below threshold), 1=warning, 2=healthy",
		},
		[]string{"supplier"},
	)

	supplierStakeHealthRatio = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_stake_health_ratio",
			Help:      "Ratio of current stake to minimum required stake (higher is better)",
		},
		[]string{"supplier"},
	)

	supplierBalanceCriticalAlerts = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_balance_critical_alerts_total",
			Help:      "Total number of critical balance alerts (below threshold)",
		},
		[]string{"supplier"},
	)

	supplierBalanceWarningAlerts = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_balance_warning_alerts_total",
			Help:      "Total number of balance warning alerts",
		},
		[]string{"supplier"},
	)

	supplierStakeWarningAlerts = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_stake_warning_alerts_total",
			Help:      "Total number of stake warning alerts (close to auto-unstake threshold)",
		},
		[]string{"supplier"},
	)

	supplierStakeCriticalAlerts = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_stake_critical_alerts_total",
			Help:      "Total number of stake critical alerts (very close to auto-unstake threshold)",
		},
		[]string{"supplier"},
	)

	supplierMonitorErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_monitor_errors_total",
			Help:      "Total number of errors during balance/stake monitoring",
		},
		[]string{"supplier", "error_type"}, // error_type: balance_query, stake_query
	)

	// Note: late-arriving relays are counted via ha_miner_relays_rejected_total
	// with reason="session_sealed", emitted by the SMST manager's two-phase
	// seal in supplier_worker.go. The older session_late_relays metric was
	// removed because GetPendingRelayCount was a global supplier-stream count,
	// not per-session, and produced systematic false positives.

	// ====== SUPPLIER CLAIMER METRICS ======

	// supplierClaimedTotal tracks how many times a supplier was claimed by an instance.
	supplierClaimedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_claimed_total",
			Help:      "Total number of supplier claim events per supplier and instance",
		},
		[]string{"supplier", "instance"},
	)

	// supplierReleasedTotal tracks how many times a supplier was released by an instance.
	supplierReleasedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_released_total",
			Help:      "Total number of supplier release events per supplier and instance",
		},
		[]string{"supplier", "instance"},
	)

	// supplierClaimedGauge tracks current number of suppliers claimed by each instance.
	supplierClaimedGauge = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_claimed_count",
			Help:      "Current number of suppliers claimed by this instance",
		},
		[]string{"instance"},
	)

	// supplierFairShareGauge tracks the calculated fair share for each instance.
	supplierFairShareGauge = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_fair_share",
			Help:      "Calculated fair share of suppliers for this instance",
		},
		[]string{"instance"},
	)

	// supplierDrainDecisionTotal tracks every drain decision with on-chain verification result.
	// Labels: drain_reason (rebalance_release, key_removal, claim_expiry),
	//         on_chain_result (staked, not_found, error, no_query_client)
	supplierDrainDecisionTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_drain_decision_total",
			Help:      "Total supplier drain decisions with on-chain verification result",
		},
		[]string{"drain_reason", "on_chain_result"},
	)

	// ====== WORKER POOL METRICS ======
	// These metrics track the pond worker pool state and performance.
	// Useful for diagnosing concurrency bottlenecks with many suppliers.

	// workerPoolConfiguredSize tracks the configured/calculated pool size.
	workerPoolConfiguredSize = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_configured_size",
			Help:      "Configured maximum size of the worker pool",
		},
		[]string{"pool_name"},
	)

	// workerPoolRunningWorkers tracks the current number of active workers.
	workerPoolRunningWorkers = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_running_workers",
			Help:      "Current number of running workers in the pool",
		},
		[]string{"pool_name"},
	)

	// workerPoolWaitingTasks tracks tasks waiting in the queue.
	workerPoolWaitingTasks = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_waiting_tasks",
			Help:      "Current number of tasks waiting in the queue",
		},
		[]string{"pool_name"},
	)

	// workerPoolSubmittedTasksTotal tracks total tasks submitted.
	workerPoolSubmittedTasksTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_submitted_tasks_total",
			Help:      "Total number of tasks submitted to the pool",
		},
		[]string{"pool_name"},
	)

	// workerPoolCompletedTasksTotal tracks total tasks completed (success + failed).
	workerPoolCompletedTasksTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_completed_tasks_total",
			Help:      "Total number of tasks completed (success + failed)",
		},
		[]string{"pool_name"},
	)

	// workerPoolSuccessfulTasksTotal tracks tasks completed successfully.
	workerPoolSuccessfulTasksTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_successful_tasks_total",
			Help:      "Total number of tasks completed successfully",
		},
		[]string{"pool_name"},
	)

	// workerPoolFailedTasksTotal tracks tasks that panicked.
	workerPoolFailedTasksTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_failed_tasks_total",
			Help:      "Total number of tasks that completed with panic",
		},
		[]string{"pool_name"},
	)

	// workerPoolDroppedTasksTotal tracks tasks dropped due to full queue.
	workerPoolDroppedTasksTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "worker_pool_dropped_tasks_total",
			Help:      "Total number of tasks dropped because the queue was full",
		},
		[]string{"pool_name"},
	)
)

// =============================================
// METRICS HELPER FUNCTIONS FOR OPERATORS
// =============================================

// RecordRelayConsumedFromStream records a relay consumed from Redis Stream.
func RecordRelayConsumedFromStream(supplier, serviceID string) {
	relaysConsumedFromStream.WithLabelValues(supplier, serviceID).Inc()
}

// RecordOrphanedStreams publishes the number of relay streams with no known
// supplier. Called only by the leader, so the gauge has a single writer.
func RecordOrphanedStreams(n int) {
	orphanedStreams.Set(float64(n))
}

// RecordShutdownDrainedRelay records one relay drained and processed during a
// graceful shutdown.
func RecordShutdownDrainedRelay(supplier string) {
	shutdownDrainedRelays.WithLabelValues(supplier).Inc()
}

// RecordShutdownAbandonedRelays records relays still buffered when the drain
// window closed. They remain in the pending list for the reclaim to recover.
func RecordShutdownAbandonedRelays(supplier string, n int) {
	if n <= 0 {
		return
	}
	shutdownAbandonedRelays.WithLabelValues(supplier).Add(float64(n))
}

// RecordRelayAddedToSMST records a relay successfully added to SMST tree.
func RecordRelayAddedToSMST(supplier, serviceID string) {
	relaysAddedToSMST.WithLabelValues(supplier, serviceID).Inc()
}

// RecordClaimLeafStats pins the two gauges that let operators compare the
// number of distinct SMST leaves sealed into a claim against the number of
// relays the session coordinator counted. When leaves < attempts, some
// relays shared a RelayHash and were deduped at the SMST-key level — log +
// collapse counter bumped so it surfaces in dashboards.
func RecordClaimLeafStats(supplier, serviceID, sessionID string, leaves, attempts int64) {
	claimNumLeaves.WithLabelValues(supplier, serviceID, sessionID).Set(float64(leaves))
	claimRelayAttempts.WithLabelValues(supplier, serviceID, sessionID).Set(float64(attempts))
	if leaves < attempts {
		claimLeafCollapseTotal.WithLabelValues(supplier, serviceID).Inc()
	}
}

// ClearClaimLeafStats removes the per-session claim leaf gauges (called on
// terminal transitions to keep cardinality bounded).
func ClearClaimLeafStats(supplier, serviceID, sessionID string) {
	claimNumLeaves.DeleteLabelValues(supplier, serviceID, sessionID)
	claimRelayAttempts.DeleteLabelValues(supplier, serviceID, sessionID)
}

// RecordRelayFailedSMST records a relay that failed to add to SMST tree.
func RecordRelayFailedSMST(supplier, serviceID, reason string) {
	relaysFailedSMST.WithLabelValues(supplier, serviceID, reason).Inc()
}

// RecordRelayRejected records a relay that was rejected.
func RecordRelayRejected(supplier, reason, serviceID string) {
	relaysRejected.WithLabelValues(supplier, reason, serviceID).Inc()
}

// RecordRelayProcessingLatency records how long it took to mine a relay on miner side.
func RecordRelayProcessingLatency(supplier, serviceID, statusCode string, seconds float64) {
	relayProcessingLatency.WithLabelValues(supplier, serviceID, statusCode).Observe(seconds)
}

// RecordSessionCreated increments the session created counter.
func RecordSessionCreated(supplier, serviceID string) {
	sessionsCreatedTotal.WithLabelValues(supplier, serviceID).Inc()
}

// ClearSessionMetrics removes session-specific metrics when session completes.
func ClearSessionMetrics(supplier, sessionID, serviceID string) {
	claimScheduledHeight.DeleteLabelValues(supplier, serviceID, sessionID)
	proofScheduledHeight.DeleteLabelValues(supplier, serviceID, sessionID)
	ClearClaimLeafStats(supplier, serviceID, sessionID)
}

// SetClaimScheduledHeight sets when a claim is scheduled to be submitted.
func SetClaimScheduledHeight(supplier, serviceID, sessionID string, height float64) {
	claimScheduledHeight.WithLabelValues(supplier, serviceID, sessionID).Set(height)
}

// RecordClaimSubmissionLatency records how many blocks after window opened the claim was submitted.
func RecordClaimSubmissionLatency(supplier string, blocksAfterWindowOpened float64) {
	claimSubmissionLatencyBlocks.WithLabelValues(supplier).Observe(blocksAfterWindowOpened)
}

// SetProofScheduledHeight sets when a proof is scheduled to be submitted.
func SetProofScheduledHeight(supplier, serviceID, sessionID string, height float64) {
	proofScheduledHeight.WithLabelValues(supplier, serviceID, sessionID).Set(height)
}

// RecordProofSubmissionLatency records how many blocks after window opened the proof was submitted.
func RecordProofSubmissionLatency(supplier string, blocksAfterWindowOpened float64) {
	proofSubmissionLatencyBlocks.WithLabelValues(supplier).Observe(blocksAfterWindowOpened)
}

// RecordSessionProved increments the proved sessions counter.
func RecordSessionProved(supplier, serviceID string) {
	sessionsProvedTotal.WithLabelValues(supplier, serviceID).Inc()
}

// RecordSessionProbabilisticProved records a session that was probabilistically proved (no proof required).
func RecordSessionProbabilisticProved(supplier, serviceID string) {
	sessionsProbabilisticProvedTotal.WithLabelValues(supplier, serviceID).Inc()
}

// recordSessionFailure is the internal function that records all failure metrics.
// This tracks sessions, relays, compute units, and uPOKT lost due to claim/proof failures.
func recordSessionFailure(supplier, serviceID, reason string, relays, computeUnits int64) {
	cu := float64(computeUnits)
	relayCount := float64(relays)

	// Session failure count
	sessionsFailedTotal.WithLabelValues(supplier, serviceID, reason).Inc()

	// Relays lost
	relaysLostTotal.WithLabelValues(supplier, serviceID, reason).Add(relayCount)

	// Compute units lost (in pPOKT from service config)
	computeUnitsLostTotal.WithLabelValues(supplier, serviceID, reason).Add(cu)

	// uPOKT lost (convert pPOKT to uPOKT by dividing by 1e6)
	upoktLostTotal.WithLabelValues(supplier, serviceID, reason).Add(cu / 1e6)
}

// RecordClaimWindowClosed records a claim window timeout failure.
func RecordClaimWindowClosed(supplier, serviceID string, relays, computeUnits int64) {
	recordSessionFailure(supplier, serviceID, "claim_window_closed", relays, computeUnits)
}

// RecordClaimTxError records a claim transaction error failure.
func RecordClaimTxError(supplier, serviceID string, relays, computeUnits int64) {
	recordSessionFailure(supplier, serviceID, "claim_tx_error", relays, computeUnits)
}

// RecordProofWindowClosed records a proof window timeout failure.
func RecordProofWindowClosed(supplier, serviceID string, relays, computeUnits int64) {
	recordSessionFailure(supplier, serviceID, "proof_window_closed", relays, computeUnits)
}

// RecordProofTxError records a proof transaction error failure.
func RecordProofTxError(supplier, serviceID string, relays, computeUnits int64) {
	recordSessionFailure(supplier, serviceID, "proof_tx_error", relays, computeUnits)
}

// recordRevenueClaimed is the internal function that records all claim success metrics.
// This tracks compute units, uPOKT revenue, and relay count when a claim is accepted.
func recordRevenueClaimed(supplier, serviceID string, computeUnits uint64, relayCount int64) {
	cu := float64(computeUnits)
	relays := float64(relayCount)

	// Compute Units view (in pPOKT from service config)
	computeUnitsClaimedTotal.WithLabelValues(supplier, serviceID).Add(cu)

	// uPOKT view (convert pPOKT to uPOKT by dividing by 1e6)
	upoktClaimedTotal.WithLabelValues(supplier, serviceID).Add(cu / 1e6)

	// Relays view
	relaysClaimedTotal.WithLabelValues(supplier, serviceID).Add(relays)
}

// recordRevenueProved is the internal function that records all proof success metrics.
// This tracks compute units, uPOKT revenue, and relay count when a proof is accepted.
func recordRevenueProved(supplier, serviceID string, computeUnits uint64, relayCount int64) {
	cu := float64(computeUnits)
	relays := float64(relayCount)

	// Compute Units view (in pPOKT from service config)
	computeUnitsProvedTotal.WithLabelValues(supplier, serviceID).Add(cu)

	// uPOKT view (convert pPOKT to uPOKT by dividing by 1e6)
	upoktProvedTotal.WithLabelValues(supplier, serviceID).Add(cu / 1e6)

	// Relays view
	relaysProvedTotal.WithLabelValues(supplier, serviceID).Add(relays)
}

// RecordRevenueClaimed records successful claim submission across all revenue views.
func RecordRevenueClaimed(supplier, serviceID string, computeUnits uint64, relayCount int64) {
	recordRevenueClaimed(supplier, serviceID, computeUnits, relayCount)
}

// RecordRevenueProved records successful proof submission across all revenue views.
func RecordRevenueProved(supplier, serviceID string, computeUnits uint64, relayCount int64) {
	recordRevenueProved(supplier, serviceID, computeUnits, relayCount)
}

// RecordRevenueProbabilisticProved records revenue from a probabilistically proved session.
// Uses same metrics as explicit proof since both are successful outcomes.
func RecordRevenueProbabilisticProved(supplier, serviceID string, computeUnits uint64, relayCount int64) {
	recordRevenueProved(supplier, serviceID, computeUnits, relayCount)
}

// RecordClaimCreated records a claim built into a submission batch (pre-submit attempt).
func RecordClaimCreated(supplier, serviceID string) {
	claimsCreated.WithLabelValues(supplier, serviceID).Inc()
}

// RecordProofCreated records a proof built into a submission batch (pre-submit attempt).
func RecordProofCreated(supplier, serviceID string) {
	proofsCreated.WithLabelValues(supplier, serviceID).Inc()
}

// RecordClaimSubmitted increments the claims submitted counter.
func RecordClaimSubmitted(supplier, serviceID string) {
	claimsSubmitted.WithLabelValues(supplier, serviceID).Inc()
}

// RecordProofSubmitted increments the proofs submitted counter.
func RecordProofSubmitted(supplier, serviceID string) {
	proofsSubmitted.WithLabelValues(supplier, serviceID).Inc()
}

// =============================================
// PROOF REQUIREMENT METRICS HELPERS
// =============================================

// RecordProofRequirementCheck records that a proof requirement check was performed.
func RecordProofRequirementCheck(supplier string) {
	proofRequirementChecks.WithLabelValues(supplier).Inc()
}

// RecordProofRequirementRequired records that a proof was determined to be required.
// reason should be either "threshold" or "probabilistic".
func RecordProofRequirementRequired(supplier, reason string) {
	proofRequirementRequired.WithLabelValues(supplier, reason).Inc()
}

// RecordProofRequirementSkipped records that a proof was determined to NOT be required.
func RecordProofRequirementSkipped(supplier string) {
	proofRequirementSkipped.WithLabelValues(supplier).Inc()
}

// RecordProofRequirementCheckError records an error during proof requirement checking.
func RecordProofRequirementCheckError(supplier, operation string) {
	proofRequirementErrors.WithLabelValues(supplier, operation).Inc()
}

// Proof-skip reasons — kept as constants so logs and metrics agree.
// Bounded set prevents label explosion on the proofSkippedTotal counter.
const (
	// ProofSkippedReasonClaimMissingOnChain — pre-proof GetClaim guard found
	// no on-chain claim for the session (tx accepted to mempool but never
	// included, or DeliverTx rejected). Session transitions to
	// SessionStateClaimMissing.
	ProofSkippedReasonClaimMissingOnChain = "claim_missing_on_chain"

	// ProofSkippedReasonBuildFailed — ProveClosest or buildSessionHeader
	// returned an error for a single session inside the batched build
	// fan-out. The rest of the batch continues so one bad session does
	// not poison the submission.
	ProofSkippedReasonBuildFailed = "build_failed"
)

// RecordProofSkipped increments the proof-skipped counter with the given
// reason. Reason must come from the ProofSkippedReason* constants to keep
// the label cardinality bounded.
func RecordProofSkipped(supplier, serviceID, reason string) {
	proofSkippedTotal.WithLabelValues(supplier, serviceID, reason).Inc()
}

// RecordClaimSkipped records an intentional claim skip with a reason label.
// Use for decisions we make BEFORE sending the claim tx to the chain
// (unprofitable, dedup hit, zero-work, etc).
func RecordClaimSkipped(supplier, serviceID, reason string) {
	claimsSkippedTotal.WithLabelValues(supplier, serviceID, reason).Inc()
}

// ====== WORKER POOL METRICS HELPERS ======

// WorkerPoolMetrics represents the metrics from a pond worker pool.
type WorkerPoolMetrics struct {
	PoolName        string
	ConfiguredSize  int
	RunningWorkers  int64
	WaitingTasks    uint64
	SubmittedTasks  uint64
	CompletedTasks  uint64
	SuccessfulTasks uint64
	FailedTasks     uint64
	DroppedTasks    uint64
}

// workerPoolPreviousMetrics stores the previous counter values for delta calculation.
// This is needed because pond returns total counts, but we want to export them as counters.
var workerPoolPreviousMetrics = make(map[string]*WorkerPoolMetrics)

// RecordWorkerPoolConfiguredSize records the configured/calculated pool size.
// Called once at startup when the pool is created.
func RecordWorkerPoolConfiguredSize(poolName string, size int) {
	workerPoolConfiguredSize.WithLabelValues(poolName).Set(float64(size))
}

// RecordWorkerPoolMetrics records all worker pool metrics.
// This should be called periodically by a ticker.
func RecordWorkerPoolMetrics(metrics WorkerPoolMetrics) {
	poolName := metrics.PoolName

	// Record gauge metrics (current state)
	workerPoolRunningWorkers.WithLabelValues(poolName).Set(float64(metrics.RunningWorkers))
	workerPoolWaitingTasks.WithLabelValues(poolName).Set(float64(metrics.WaitingTasks))

	// Get previous values for delta calculation
	prev, exists := workerPoolPreviousMetrics[poolName]
	if !exists {
		prev = &WorkerPoolMetrics{}
		workerPoolPreviousMetrics[poolName] = prev
	}

	// Record counter metrics (incremental deltas)
	// Only add the delta since last collection to avoid double-counting
	if metrics.SubmittedTasks > prev.SubmittedTasks {
		delta := metrics.SubmittedTasks - prev.SubmittedTasks
		workerPoolSubmittedTasksTotal.WithLabelValues(poolName).Add(float64(delta))
	}
	if metrics.CompletedTasks > prev.CompletedTasks {
		delta := metrics.CompletedTasks - prev.CompletedTasks
		workerPoolCompletedTasksTotal.WithLabelValues(poolName).Add(float64(delta))
	}
	if metrics.SuccessfulTasks > prev.SuccessfulTasks {
		delta := metrics.SuccessfulTasks - prev.SuccessfulTasks
		workerPoolSuccessfulTasksTotal.WithLabelValues(poolName).Add(float64(delta))
	}
	if metrics.FailedTasks > prev.FailedTasks {
		delta := metrics.FailedTasks - prev.FailedTasks
		workerPoolFailedTasksTotal.WithLabelValues(poolName).Add(float64(delta))
	}
	if metrics.DroppedTasks > prev.DroppedTasks {
		delta := metrics.DroppedTasks - prev.DroppedTasks
		workerPoolDroppedTasksTotal.WithLabelValues(poolName).Add(float64(delta))
	}

	// Update previous values
	prev.SubmittedTasks = metrics.SubmittedTasks
	prev.CompletedTasks = metrics.CompletedTasks
	prev.SuccessfulTasks = metrics.SuccessfulTasks
	prev.FailedTasks = metrics.FailedTasks
	prev.DroppedTasks = metrics.DroppedTasks
}

// StartWorkerPoolMetricsTicker starts a goroutine that periodically collects
// and records metrics from a pond worker pool.
// The ticker runs every 5 seconds and stops when the context is cancelled.
func StartWorkerPoolMetricsTicker(
	ctx context.Context,
	logger logging.Logger,
	pool pond.Pool,
	poolName string,
	configuredSize int,
) {
	// Record the configured size immediately
	RecordWorkerPoolConfiguredSize(poolName, configuredSize)

	// Log initial pool creation metrics
	logger.Info().
		Str("pool_name", poolName).
		Int("configured_size", configuredSize).
		Msg("starting worker pool metrics ticker")

	// Start the ticker goroutine
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Debug().
					Str("pool_name", poolName).
					Msg("worker pool metrics ticker stopped")
				return
			case <-ticker.C:
				// Collect metrics from the pool
				metrics := WorkerPoolMetrics{
					PoolName:        poolName,
					ConfiguredSize:  configuredSize,
					RunningWorkers:  pool.RunningWorkers(),
					WaitingTasks:    pool.WaitingTasks(),
					SubmittedTasks:  pool.SubmittedTasks(),
					CompletedTasks:  pool.CompletedTasks(),
					SuccessfulTasks: pool.SuccessfulTasks(),
					FailedTasks:     pool.FailedTasks(),
					DroppedTasks:    pool.DroppedTasks(),
				}

				// Record the metrics
				RecordWorkerPoolMetrics(metrics)

				// Debug log for high utilization (running workers > 80% of configured)
				utilizationPct := float64(metrics.RunningWorkers) / float64(configuredSize) * 100
				if utilizationPct > 80 {
					logger.Debug().
						Str("pool_name", poolName).
						Int64("running_workers", metrics.RunningWorkers).
						Uint64("waiting_tasks", metrics.WaitingTasks).
						Float64("utilization_pct", utilizationPct).
						Msg("worker pool high utilization")
				}
			}
		}
	}()
}
