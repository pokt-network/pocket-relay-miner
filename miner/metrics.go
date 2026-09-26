package miner

import (
	"context"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "miner"
)

// Reasons a GC is forced, used as a metric label.
const (
	gcReasonRebuildAdmission = "rebuild_admission"
	gcReasonMemoryBrake      = "memory_brake"
)

// Why the ingestion memory brake closed, logged.
const (
	memoryBrakeReasonOverage = "overage"
	memoryBrakeReasonLive    = "live"
)

// Ingestion memory brake states, used as a metric label.
const (
	memoryBrakeClosed = "closed"
	memoryBrakeOpen   = "open"
)

var (
	// forcedGCs counts the GCs the miner forces to read a recent live heap,
	// by reason.
	forcedGCs = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "forced_gc_total",
			Help:      "GCs the miner forced to read a recent live heap, by reason",
		},
		[]string{"reason"},
	)

	// smstTreesUnloaded counts the trees of ended sessions dropped from memory
	// and kept in Redis, by supplier.
	smstTreesUnloaded = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "smst_trees_unloaded_total",
			Help:      "Trees of sessions past their grace period dropped from memory and kept in Redis",
		},
		[]string{"supplier"},
	)

	// ingestionMemoryBrakeClosed is 1 while the heap near the memory limit
	// holds the stream consumers.
	ingestionMemoryBrakeClosed = observability.MinerFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "ingestion_memory_brake_closed",
			Help:      "1 while the live heap near the process memory limit holds the stream consumers",
		},
	)

	// ingestionMemoryBrakeTransitions counts the brake closing and reopening.
	ingestionMemoryBrakeTransitions = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "ingestion_memory_brake_transitions_total",
			Help:      "Times the ingestion memory brake closed or reopened, by the state it entered",
		},
		[]string{"state"},
	)

	// trackingWritesFailed counts submission tracking records Redis REFUSED,
	// by kind (claim, proof, claim_outcome, proof_outcome) and by reason: oom
	// when Redis answered that it is out of memory, other for anything else.
	//
	// It replaces tracking_writes_skipped_total, which counted records this
	// miner chose not to write while its own store gate was closed. That choice
	// is gone: these records are facts about claims and proofs already sent, and
	// the memory reserve exists so their writes fit. What is worth counting is a
	// write the store turned down, which is also the signal the brake trusts
	// most -- an OOM reply closes every gate on its own.
	trackingWritesFailed = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "tracking_writes_failed_total",
			Help:      "Submission tracking records Redis refused, by kind and reason (oom, other)",
		},
		[]string{"kind", "reason"},
	)

	// trackingOutcomesWithoutRecord counts on-chain outcomes the reconciler
	// resolved and could not write down, because the submission record they
	// annotate was not there. The reconciler polls a tx until it resolves and
	// then stops, so that outcome is gone for good: nothing will ask again.
	trackingOutcomesWithoutRecord = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "tracking_outcomes_without_record_total",
			Help:      "On-chain outcomes that found no submission record to annotate, by kind",
		},
		[]string{"kind"},
	)

	// storeMemoryAtClose is what Redis held, by key family (stream, smst, other),
	// the last time the store closed: whether a full store is the relay backlog or
	// the trees decides what can free it.
	storeMemoryAtClose = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "store_memory_at_close_bytes",
			Help:      "Redis memory by key family (stream, smst, other) measured when the store last closed",
		},
		[]string{"family"},
	)

	// Relay flow tracking metrics (for debugging SMST sealing issues)
	relaysConsumedFromStream = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_consumed_from_stream_total",
			Help:      "Total number of relays consumed from the relay stream (relayer → miner)",
		},
		[]string{"supplier", "service_id"},
	)

	// relaysAddedToSMST counts UpdateTree CALLS that returned nil — i.e.,
	// the relay's bytes were written into the SMST backing store. It does
	// NOT count unique SMST leaves: when two relays share the same
	// RelayHash (dedup by protocol-key), UpdateTree succeeds for both but
	// only one leaf survives in the claimed root. For the number of
	// billable leaves at claim time, see ha_miner_claim_leaves_total.
	relaysAddedToSMST = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_added_to_smst_total",
			Help:      "SMST UpdateTree successes (NOT unique leaves — see claim_leaves_total for billable count)",
		},
		// session_id deliberately excluded: it is an unbounded value on a
		// Counter (never DeleteLabelValues'd) → TSDB OOM. Per-session detail
		// lives in logs.
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
	// relaysDroppedNoKey counts relays destroyed because the signing key was
	// withdrawn. Money that was served and will never be claimed, on purpose.
	relaysDroppedNoKey = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_dropped_no_key_total",
			Help:      "Relays acknowledged and destroyed because the supplier's signing key was withdrawn (no SMST, claim or proof is possible)",
		},
		[]string{"supplier", "service_id"},
	)

	shutdownDrainedRelays = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "shutdown_drained_relays_total",
			Help:      "Relays a supplier's exit handed back to the group unprocessed -- from its delivery buffer or from under its consumer's name -- for another consumer to finish",
		},
		[]string{"supplier"},
	)

	shutdownAbandonedRelays = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "shutdown_abandoned_relays_total",
			Help:      "Relays a supplier's exit could not hand back -- still buffered when the drain window closed, or refused when released from under its consumer's name; they stay pending, not lost",
		},
		[]string{"supplier"},
	)

	// claimLeavesTotal adds up the distinct SMST leaves of every claim built
	// (each equals EventClaimCreated.num_relays on-chain). Paired with
	// claimRelayAttemptsTotal below, the difference in their rates exposes
	// collapses caused by identical-bytes relays sharing a key (e.g. a
	// subscription fan-out where every event body is byte-identical). Counters
	// rather than per-session gauges: sessions of one supplier and service
	// are built concurrently, and a session_id label is unbounded.
	claimLeavesTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_leaves_total",
			Help:      "Distinct SMST leaves in the claims built (each claim's matches its on-chain num_relays)",
		},
		[]string{"supplier", "service_id"},
	)

	// claimRelayAttemptsTotal adds up the session coordinator's RelayCount of
	// every claim built -- how many relays the relayer mined into each session
	// before sealing. Compare its rate with claim_leaves_total to detect
	// dedup-by-key collapses without tailing logs.
	claimRelayAttemptsTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_relay_attempts_total",
			Help:      "Session coordinator RelayCount of the claims built (compare with claim_leaves_total to detect collapse)",
		},
		[]string{"supplier", "service_id"},
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

	// claimWindowOpenUnchecked counts relays admitted without knowing whether
	// their session's claim still waits for them, because the shared params at their
	// session end height could not be read. Admitting is deliberate: dropping on
	// an unknown is how served work stops being paid. A rate here means the entry
	// cut is not cutting, which the rejection counter alone cannot tell apart
	// from nothing arriving late.
	claimWindowOpenUnchecked = observability.MinerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_window_open_unchecked_total",
			Help:      "Relays admitted without checking whether their session's claim still waits for them, because the shared params at the session end height could not be read",
		},
	)

	// claimFlushCapped counts, ONE PER SESSION, sessions whose flush delay
	// ended by hitting the cap (2 block times past the claim window opening)
	// instead of confirming the stream had drained. It is not the same
	// series as relays_rejected{reason="session_sealed"}: a session counted
	// here is a candidate to produce session_sealed downstream (a relay that
	// arrived after the cap-forced flush), not a duplicate of it. No
	// "supplier" label -- bounded per-supplier labeling is pending, tracked
	// separately.
	claimFlushCapped = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_flush_capped_total",
			Help: "Sessions whose claim sealed at the height cap (claim window " +
				"open + 2 blocks) without confirming the stream had drained; " +
				"any live relay still undelivered or unprocessed at that " +
				"point is dropped downstream as session_sealed.",
		},
		[]string{"service_id"},
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
			Help:      "Failed session attempts by reason (claim_window_closed, claim_tx_error, claim_ejected_unrecoverable, proof_window_closed, proof_tx_error, panic_recovered on relays only); claim_tx_error and proof_tx_error are retried by the inclusion reconciler ONLY when a transaction was actually broadcast, since the reconciler walks rebroadcast entries -- a session marked proof_tx_error before any proof was built has none, and nothing retries it. What settles the broadcast case is claim_inclusion_outcome_total / proof_inclusion_outcome_total with outcome=on_chain_found",
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
			Help:      "Compute units that entered the claim book and will not be paid, by reason (see sessions_failed_total for the session-level reasons). A RETRIED claim_tx_error or proof_tx_error no longer reaches this series: the loss is counted once, either when the window closes with nothing submitted, or when the inclusion reconciler gets the chain's answer -- which arrives here as on_chain_missing or on_chain_rejected. Work whose claim never reached the chain is in compute_units_forgone_total instead, so this series is subtractable from compute_units_claimed_total",
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
			Help:      "uPOKT that entered the claim book and will not be paid, compute units 1:1, by reason (see sessions_failed_total for the session-level reasons). A RETRIED claim_tx_error or proof_tx_error no longer reaches this series: the loss is counted once, either when the window closes with nothing submitted, or when the inclusion reconciler gets the chain's answer -- which arrives here as on_chain_missing or on_chain_rejected. Work whose claim never reached the chain is in upokt_forgone_total instead, so upokt_claimed_total = upokt_proved_total + this + (upokt_unresolved_opened_total - upokt_unresolved_resolved_total)",
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
			Help:      "Relays that entered the claim book and will not be paid, by reason (see sessions_failed_total for the session-level reasons, plus panic_recovered for a relay dropped by a recovered panic -- which never reached a tree, so it is the one reason here whose work was never in the book). A RETRIED claim_tx_error or proof_tx_error no longer reaches this series: the loss is counted once, either when the window closes with nothing submitted, or when the inclusion reconciler gets the chain's answer -- which arrives here as on_chain_missing or on_chain_rejected. Work whose claim never reached the chain is in relays_forgone_total instead",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// ====== THE THIRD DOOR: UNRESOLVED ======
	//
	// Money that entered the book leaves it through exactly one door, so the
	// doors must sum:
	//
	//     claimed = proved + lost + (unresolved_opened - unresolved_resolved)
	//
	// `unresolved` is a session whose submission was never confirmed and whose
	// fate the chain has not yet told us. It is a SERIES and not an absence on
	// purpose: if nothing ever resolves it, it stays up and is visible, which is
	// the honest failure mode. A signal whose job is to reveal a gap cannot have
	// a gap of its own.
	//
	// A COUNTER PAIR AND NOT A GAUGE, AND THE REASON IS THE RESTART. A gauge
	// lives in one process. A session this replica leaves unresolved and a
	// DIFFERENT replica resolves would leave a stale 1 on the first series and a
	// 0 on the second, and no query over the fleet recovers the truth -- it
	// reads 1 forever, or 0 once the dead series ages out, and both are wrong.
	// Two counters survive it: this replica reports the open, that one reports
	// the close, and the fleet-wide difference is right. Failover is the normal
	// case here, so this is the case that decides the shape.
	//
	// `phase` is a CLOSED set of two, and only "proof" is reachable today.
	// Opening one requires the money to already be in the book, and the claim
	// side only enters the book when the chain accepts the claim -- at which
	// point there is nothing left to be unresolved ABOUT the claim. The label
	// stays because the claim side entering the book earlier (the success-side
	// rework) would make "claim" reachable without a schema change.
	upoktUnresolvedOpenedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_unresolved_opened_total",
			Help:      "uPOKT that left through a retryable submission failure and is awaiting the chain's answer (opened). Subtract upokt_unresolved_resolved_total for the pending balance.",
		},
		[]string{"supplier", "service_id", "phase"},
	)

	upoktUnresolvedResolvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_unresolved_resolved_total",
			Help:      "uPOKT previously counted unresolved that the inclusion reconciler has since settled into proved or lost.",
		},
		[]string{"supplier", "service_id", "phase"},
	)

	computeUnitsUnresolvedOpenedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_unresolved_opened_total",
			Help:      "Compute units awaiting the chain's answer after a retryable submission failure (opened).",
		},
		[]string{"supplier", "service_id", "phase"},
	)

	computeUnitsUnresolvedResolvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_unresolved_resolved_total",
			Help:      "Compute units previously counted unresolved that have since been settled into proved or lost.",
		},
		[]string{"supplier", "service_id", "phase"},
	)

	relaysUnresolvedOpenedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_unresolved_opened_total",
			Help:      "Relays awaiting the chain's answer after a retryable submission failure (opened).",
		},
		[]string{"supplier", "service_id", "phase"},
	)

	relaysUnresolvedResolvedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_unresolved_resolved_total",
			Help:      "Relays previously counted unresolved that have since been settled into proved or lost.",
		},
		[]string{"supplier", "service_id", "phase"},
	)

	// ====== FORGONE: WORK THAT NEVER ENTERED THE BOOK ======
	//
	// Relays served whose claim never reached the chain. It is revenue the
	// operator did not earn, and it is deliberately OUTSIDE the identity above.
	//
	// The book measures what entered the book, and `claimed` rises in exactly
	// one place: after a claim transaction the chain accepted. A session that
	// ran out of claim window, or whose claim was ejected with no way back, was
	// never in `claimed` -- so adding it to `lost` would make the ledger fail to
	// close by exactly this amount, which is the defect this whole family exists
	// to remove. It is counted apart so that it cannot disappear either: work
	// served and never claimed is the operator's most expensive number.
	upoktForgoneTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_forgone_total",
			Help:      "uPOKT for work that never entered the claim book (no claim ever reached the chain), by reason (claim_window_closed, claim_tx_error, claim_ejected_unrecoverable, on_chain_missing, on_chain_rejected). OUTSIDE the claimed/proved/lost/unresolved identity by construction: claimed only rises on a claim the chain accepted, so this can never be subtracted from it.",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	computeUnitsForgoneTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_forgone_total",
			Help:      "Compute units for work that never entered the claim book, by reason (see upokt_forgone_total). OUTSIDE the ledger identity by construction.",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	relaysForgoneTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_forgone_total",
			Help:      "Relays served whose work never entered the claim book, by reason (see upokt_forgone_total). OUTSIDE the ledger identity by construction.",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// relayBatchPanicsTotal counts relay batches whose flush panicked and were
	// finished one relay at a time instead. It counts BATCHES, not relays: the
	// relays of such a batch are still counted and acknowledged by that path,
	// and the one whose own processing panics there lands in relays_lost_total.
	relayBatchPanicsTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_batch_panics_total",
			Help:      "Relay batches whose flush panicked and fell back to the per-relay path",
		},
		[]string{"supplier"},
	)

	// relayBatchFlushesTotal counts relay batch flushes by what set them off:
	// time (the interval ticked), count (a session reached relayBatchCap) or
	// bytes (relayBatchFlushBytes went into the supplier's leaves). With small
	// relays bytes stays at zero; with big ones it is what bounds the leaves.
	relayBatchFlushesTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_batch_flushes_total",
			Help:      "Relay batch flushes by trigger (time, count, bytes)",
		},
		[]string{"supplier", "trigger"},
	)

	// relayBatchReleasedTotal counts relays a batch handed back unacknowledged
	// at a flush instead of settling them, by why: they stay pending for a
	// redelivery that puts them in the session's current tree.
	relayBatchReleasedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relay_batch_released_total",
			Help:      "Relays a relay batch handed back unacknowledged at a flush, by reason (tree_not_resident, tree_replaced)",
		},
		[]string{"supplier", "reason"},
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
	dedupCacheHits = observability.MinerFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_cache_hits_total",
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
		// session_id excluded; operation is bounded (redis_check/redis_mark).
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
	claimInclusionOutcomeTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_inclusion_outcome_total",
			Help:      "Post-broadcast on-chain claim inclusion outcome (labeled by supplier, service_id, outcome)",
		},
		[]string{"supplier", "service_id", "outcome"},
	)

	// inclusionMissingCauseTotal splits on_chain_missing by what the chain says
	// about the transaction itself.
	//
	// The module-state query answers one question -- is this session's claim in
	// the state -- and its negative answer covers a transaction that never
	// landed, one that landed and whose messages failed, and one that is sitting
	// in the mempool right now. Those need different responses from an operator
	// and produced the same line.
	//
	// The cause set is closed and comes from TxInclusion.String(). The chain's
	// codespace and code are NOT labels: they come from the chain, so their
	// value set is unbounded. They go in the log line beside this.
	inclusionMissingCauseTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_missing_cause_total",
			Help:      "on_chain_missing split by what the chain says about the tx (labeled by phase, cause)",
		},
		[]string{"phase", "cause"},
	)

	// proofRejectionDiagnosisTotal splits on_chain_rejected by the ONE thing the
	// miner can decide locally: whether the root the chain holds for that claim
	// is the root this miner stored for that session.
	//
	// It exists because the rejection reason is NOT readable from here. The
	// chain records it in the FailureReason of an EndBlocker event, and the
	// claim itself carries only four fields, none of them the reason -- so no
	// query returns it, and reading events would need the tx indexer this
	// reconciler exists to avoid. What the roots CAN separate is the one cause
	// with no remedy (what we hold is not what we claimed) from the six that
	// are construction or signature faults.
	//
	// No supplier label: three series is the whole point, and the operator who
	// needs the supplier has the Warn log and the submission-tracker record.
	proofRejectionDiagnosisTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_rejection_diagnosis_total",
			Help:      "on_chain_rejected split by whether the on-chain claim root matches the root this miner stored",
		},
		[]string{"cause"},
	)

	// inclusionReadState is 1 on the state this node's post-inclusion read is
	// in, 0 on the others. All three series exist from startup.
	//
	// Registering them eagerly is the whole point rather than a detail: if the
	// state were only published once a read failed, then before the first read
	// there would be no series at all, and "I have not looked yet" would be
	// indistinguishable from "it works". That is precisely how poll_dropped
	// misled, one level further in -- a signal that exists to reveal an
	// absence, having an absence of its own.
	inclusionReadState = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_read_state",
			Help:      "1 on the current state of the post-inclusion read (available|unavailable|unknown)",
		},
		[]string{"state"},
	)

	// inclusionEntryDroppedTotal counts pending entries the reconciler removed
	// WITHOUT the normal inclusion outcome; inclusionClearFailedTotal counts the
	// opposite event, an entry that could NOT be removed; and
	// inclusionGroupAbandonedTotal counts work skipped wholesale.
	//
	// They exist because "it never happened" and "it happens every block" were
	// producing the same signal: those paths logged and returned, and a Warn on
	// a per-block path is not something an operator can alert on or count. A
	// dropped entry is a claim or a proof whose fate nobody recorded.
	//
	// Labels are {phase, cause} and deliberately NOT {supplier, service_id}: a
	// corrupt entry cannot be decoded, so the service_id is precisely the field
	// that is unavailable. Naming WHICH session is the log's job -- a session id
	// as a label is unbounded cardinality.
	// inclusionResendCapUnusedTotal counts entries that reached window close
	// still missing WITH resend budget left. It exists because "the cap was not
	// needed" and "the cap could not be spent" are indistinguishable from the
	// resend counter alone, and with a cap above 1 the second becomes routine:
	// a window shorter than the spacing simply cannot hold the second attempt.
	inclusionResendCapUnusedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_resend_cap_unused_total",
			Help:      "Entries missing at window close that still had resend budget (labeled by phase)",
		},
		[]string{"phase"},
	)

	inclusionEntryDroppedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_entry_dropped_total",
			Help:      "Pending inclusion entries removed without a normal outcome (labeled by phase, cause)",
		},
		[]string{"phase", "cause"},
	)

	// inclusionClearFailedTotal is its own vector rather than a cause of the one
	// above, because a failed clear is the OPPOSITE event: the entry survives.
	// Filing it under "dropped" needed a comment explaining that nothing was
	// dropped, and a name that has to be explained is a name that will be
	// misread by whoever reads only the series.
	//
	// What it means: the entry is still there, so the next block reconciles it
	// again and emits its outcome a SECOND time. The counter does not prevent
	// that -- it makes it attributable.
	inclusionClearFailedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_clear_failed_total",
			Help:      "Pending inclusion entries whose delete failed, so their outcome is emitted again (labeled by phase)",
		},
		[]string{"phase"},
	)

	inclusionGroupAbandonedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "inclusion_group_abandoned_total",
			Help:      "Reconcile passes that abandoned pending work (labeled by phase, cause)",
		},
		[]string{"phase", "cause"},
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
			Help:      "Height of the last block this miner process received, leader or standby",
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
			Help:      "Actual time between the last two blocks in seconds, measured by the leader; a standby keeps the last value it measured as leader, or 0",
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
			Help:      "Total number of session snapshots saved to the store",
		},
		[]string{"supplier"},
	)

	sessionSnapshotsLoaded = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_snapshots_loaded_total",
			Help:      "Total number of session snapshots loaded from the store",
		},
		[]string{"supplier"},
	)

	// sessionTransitionsUndispatched MUST stay at zero: a lifecycle verdict the
	// transition dispatcher has no batch for, so the session never moves. It is
	// how 58 proved sessions stayed in proving on 2026-09-22 with nothing to say so.
	sessionTransitionsUndispatched = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_transitions_undispatched_total",
			Help:      "Session transitions the lifecycle decided but had no dispatcher for, so the session stayed in its state. Any non-zero value is a bug",
		},
		[]string{"supplier", "target_state"},
	)

	sessionSnapshotsResumedAtStartup = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_snapshots_resumed_at_startup_total",
			Help:      "Session snapshots loaded in claiming or proving with no transaction sent, moved back so it is sent, by the state they were loaded in",
		},
		[]string{"supplier", "from_state"},
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

	// supplierLeaseLostTotal counts leases this instance LOST -- expired,
	// stolen, or a renewal that stalled long enough that the key cannot still
	// be ours. It is deliberately NOT supplier_released_total: that one counts
	// a deliberate hand-over, this one counts the window in which two replicas
	// can believe they own the same supplier. Folding them into one series
	// would make an existing panel count more and be unable to say why.
	//
	// No supplier label on purpose: the fleet runs 500+ of them, and the
	// question an operator asks here is "are we losing leases, and why",
	// which trigger answers with a bounded set.
	supplierLeaseLostTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_lease_lost_total",
			Help:      "Total supplier leases lost by an instance, by trigger",
		},
		[]string{"trigger", "instance"},
	)

	// supplierDrainLeaseOverrunTotal counts drains that outlived the lease
	// budget their supplier was released with. The lease is kept until the
	// drain ends so the old consume loop is its only writer; past the budget the
	// key may have expired and a peer taken the supplier while this instance
	// still wrote -- the window the budget exists to close. Same labels as
	// supplier_lease_lost_total, for the same reason.
	supplierDrainLeaseOverrunTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_drain_lease_overrun_total",
			Help:      "Total supplier drains that outlived their lease budget, by release trigger",
		},
		[]string{"trigger", "instance"},
	)

	// smstExitCheckpointFailedTotal counts the trees whose live_root could not
	// be written as their supplier was torn down. Relays finished one at a time
	// since the tree's last checkpoint are already acknowledged, so each such
	// tree may resume at the next owner without some of them. No session_id:
	// a Counter's series are never deleted.
	smstExitCheckpointFailedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "smst_exit_checkpoint_failed_total",
			Help:      "Total session trees whose live_root could not be checkpointed when their supplier was torn down",
		},
		[]string{"supplier"},
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
	// Labels: drain_reason (lease_expired, lease_stolen, renew_stalled,
	//         rebalance_release, key_removal, claim_callback_failed,
	//         consume_loop_panicked),
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
// RecordRelayDroppedNoKey counts a relay destroyed on purpose because the
// operator withdrew the supplier's signing key: without it no SMST, claim or
// proof can be built by anyone in this fleet, so the entry is acknowledged
// rather than left for another consumer to rediscover the same dead end.
//
// It is a LOSS counter and deliberately separate from shutdown_drained_relays:
// counting a destroyed relay as a successful drain is how the cost of pulling a
// key stayed invisible.
func RecordRelayDroppedNoKey(supplier, serviceID string) {
	relaysDroppedNoKey.WithLabelValues(supplier, serviceID).Inc()
}

func RecordShutdownDrainedRelay(supplier string) {
	shutdownDrainedRelays.WithLabelValues(supplier).Inc()
}

// RecordShutdownAbandonedRelays records relays a supplier's exit could not hand
// back. They remain in the pending list.
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

// RecordClaimLeafStats adds a claim's distinct SMST leaves and the relays the
// session coordinator counted to the two counters that let operators compare
// them. When leaves < attempts, some relays shared a RelayHash and were deduped
// at the SMST-key level — log + collapse counter bumped so it surfaces in
// dashboards.
func RecordClaimLeafStats(supplier, serviceID string, leaves, attempts int64) {
	claimLeavesTotal.WithLabelValues(supplier, serviceID).Add(float64(leaves))
	claimRelayAttemptsTotal.WithLabelValues(supplier, serviceID).Add(float64(attempts))
	if leaves < attempts {
		claimLeafCollapseTotal.WithLabelValues(supplier, serviceID).Inc()
	}
}

// RecordRelayFailedSMST records a relay that failed to add to SMST tree.
func RecordRelayFailedSMST(supplier, serviceID, reason string) {
	relaysFailedSMST.WithLabelValues(supplier, serviceID, reason).Inc()
}

// RecordRelayRejected records a relay that was rejected.
func RecordRelayRejected(supplier, reason, serviceID string) {
	relaysRejected.WithLabelValues(supplier, reason, serviceID).Inc()
}

// RecordClaimWindowOpenUnchecked records a relay admitted without the claim
// window check. See claimWindowOpenUnchecked.
func RecordClaimWindowOpenUnchecked() {
	claimWindowOpenUnchecked.Inc()
}

// RecordClaimFlushCapped records that a session's flush-delay wait exited
// via the height cap, not by draining. See claimFlushCapped's Help.
func RecordClaimFlushCapped(serviceID string) {
	claimFlushCapped.WithLabelValues(serviceID).Inc()
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

// THE FOUR VERDICTS.
//
// Every session that fails increments sessions_failed_total exactly once, with
// its reason -- that series is the operator's unchanged view and it counts
// SESSIONS, never money. What differs between the four is where the MONEY goes,
// and there are only three destinations plus "nowhere":
//
//	attempt    -> nowhere. The outcome is genuinely unknown and something exists
//	              that will answer it. Putting uPOKT on an attempt is what made
//	              upokt_lost_total read 45% of revenue on a run that lost nothing.
//	lost       -> the money WAS in the book (the chain accepted its claim) and
//	              will not be paid.
//	unresolved -> the money is in the book and the chain has not answered yet.
//	forgone    -> the money was never in the book, because no claim ever reached
//	              the chain. Outside the ledger identity by construction.
//
// Choosing between them needs one fact -- did this session's claim reach the
// chain -- and that fact is not inferred anywhere: it is read off the snapshot's
// tx hash, or off the caller's own knowledge of whether a resolver exists.

// recordSessionAttemptFailure counts a failed ATTEMPT: the session, its reason,
// and no money at all.
func recordSessionAttemptFailure(supplier, serviceID, reason string) {
	sessionsFailedTotal.WithLabelValues(supplier, serviceID, reason).Inc()
}

// RecordRevenueLost counts MONEY ONLY: relays, compute units and uPOKT that
// entered the book and will not be paid.
//
// It is split from the session counter because the two have different
// arithmetic. A session fails ONCE and is counted once in sessions_failed_total,
// under the reason that ended it. Its money can be named twice -- held in
// `unresolved` when the submission failed, then settled into `lost` when the
// chain finally answers -- and the second of those must not claim a second
// failed session.
func RecordRevenueLost(supplier, serviceID, reason string, relays, computeUnits int64) {
	cu := float64(computeUnits)

	relaysLostTotal.WithLabelValues(supplier, serviceID, reason).Add(float64(relays))
	computeUnitsLostTotal.WithLabelValues(supplier, serviceID, reason).Add(cu)
	// uPOKT lost (convert pPOKT to uPOKT by dividing by 1e6)
	upoktLostTotal.WithLabelValues(supplier, serviceID, reason).Add(cu / 1e6)
}

// RecordRevenueForgone counts MONEY ONLY for work that never entered the book.
// Split from the session counter for the same reason as RecordRevenueLost.
func RecordRevenueForgone(supplier, serviceID, reason string, relays, computeUnits int64) {
	cu := float64(computeUnits)

	relaysForgoneTotal.WithLabelValues(supplier, serviceID, reason).Add(float64(relays))
	computeUnitsForgoneTotal.WithLabelValues(supplier, serviceID, reason).Add(cu)
	upoktForgoneTotal.WithLabelValues(supplier, serviceID, reason).Add(cu / 1e6)
}

// recordSessionLoss is the session-ending verdict: one failed session plus its
// money in `lost`.
func recordSessionLoss(supplier, serviceID, reason string, relays, computeUnits int64) {
	recordSessionAttemptFailure(supplier, serviceID, reason)
	RecordRevenueLost(supplier, serviceID, reason, relays, computeUnits)
}

// recordSessionForgone is the session-ending verdict for work that never
// entered the book: one failed session plus its money in `forgone`.
func recordSessionForgone(supplier, serviceID, reason string, relays, computeUnits int64) {
	recordSessionAttemptFailure(supplier, serviceID, reason)
	RecordRevenueForgone(supplier, serviceID, reason, relays, computeUnits)
}

// RecordSessionUnresolvedOpened counts money that left through a retryable
// submission failure and is waiting for the chain's answer. The session counter
// carries the caller's reason; the money carries the PHASE, because that is
// what the resolving side knows about it.
func RecordSessionUnresolvedOpened(supplier, serviceID, reason, phase string, relays, computeUnits int64) {
	cu := float64(computeUnits)

	sessionsFailedTotal.WithLabelValues(supplier, serviceID, reason).Inc()
	relaysUnresolvedOpenedTotal.WithLabelValues(supplier, serviceID, phase).Add(float64(relays))
	computeUnitsUnresolvedOpenedTotal.WithLabelValues(supplier, serviceID, phase).Add(cu)
	upoktUnresolvedOpenedTotal.WithLabelValues(supplier, serviceID, phase).Add(cu / 1e6)
}

// RecordSessionUnresolvedResolved closes an unresolved balance. It does NOT say
// which way it went: the caller records the destination (proved or lost) on top,
// because only the caller knows what the chain said.
//
// It touches no session counter. sessions_failed_total already counted this
// session when it was opened, and a session does not fail twice.
func RecordSessionUnresolvedResolved(supplier, serviceID, phase string, relays, computeUnits int64) {
	cu := float64(computeUnits)

	relaysUnresolvedResolvedTotal.WithLabelValues(supplier, serviceID, phase).Add(float64(relays))
	computeUnitsUnresolvedResolvedTotal.WithLabelValues(supplier, serviceID, phase).Add(cu)
	upoktUnresolvedResolvedTotal.WithLabelValues(supplier, serviceID, phase).Add(cu / 1e6)
}

// RecordClaimWindowClosed records a claim window that closed on a session.
//
// claimTxHash is the discriminator and it is a FACT on the snapshot, not a
// guess. Empty means no claim transaction ever went out, so this session was
// never in `claimed` and its money is forgone, not lost -- counting it lost is
// what makes the ledger fail to close. Non-empty means the claim was accepted
// and the money IS in the book, and a window closing on it now means it will
// never be proved.
func RecordClaimWindowClosed(supplier, serviceID, claimTxHash string, relays, computeUnits int64) {
	if claimTxHash == "" {
		recordSessionForgone(supplier, serviceID, "claim_window_closed", relays, computeUnits)
		return
	}
	recordSessionLoss(supplier, serviceID, "claim_window_closed", relays, computeUnits)
}

// RecordClaimEjectedUnrecoverable records a claim the chain named inside a batch
// whose rebroadcast entry did NOT land, so nothing will ever re-send it.
//
// It is deliberately a separate reason from claim_tx_error rather than a second
// counter. Both end the session, but claim_tx_error carries an implicit promise
// that the reconciler will retry, and here that promise is false. An operator
// reading sessions_failed_total has to be able to tell a loss that is still
// pending from one that is already final.
//
// The money is FORGONE and not lost: this claim message never travelled, so the
// session was never in `claimed`.
func RecordClaimEjectedUnrecoverable(supplier, serviceID string, relays, computeUnits int64) {
	recordSessionForgone(supplier, serviceID, "claim_ejected_unrecoverable", relays, computeUnits)
}

// RecordClaimTxError records a claim whose transaction did not go out.
//
// resolvable says whether a rebroadcast entry was persisted for this session, so
// that the inclusion reconciler will eventually answer for it. It is the
// caller's own fact -- it knows whether it has a store -- and never an inference.
//
// Resolvable: this is an ATTEMPT and takes no money. The claim is not in the
// book (nothing reached the chain), and the reconciler will either put it there
// on on_chain_found or count it forgone on on_chain_missing. Giving it money
// here is what let one session be counted lost and proved at the same time.
//
// Not resolvable: nothing will ever answer, so the work is forgone now rather
// than silently.
func RecordClaimTxError(supplier, serviceID string, resolvable bool, relays, computeUnits int64) {
	if resolvable {
		recordSessionAttemptFailure(supplier, serviceID, "claim_tx_error")
		return
	}
	recordSessionForgone(supplier, serviceID, "claim_tx_error", relays, computeUnits)
}

// RecordProofWindowClosed records a proof window that closed on a session.
//
// proofTxHash is the discriminator, read off the snapshot. Empty means no proof
// transaction ever went out: the session IS in the book (its claim was accepted)
// and will never be paid, which is a real loss. Non-empty means the proof was
// submitted, and a submitted proof was ALREADY counted into upokt_proved_total
// at submission -- so counting anything here would be the session leaving the
// book through a second door. Whether that proof actually landed is a question
// about the SUCCESS side, and proof_inclusion_outcome_total is the series that
// answers it.
func RecordProofWindowClosed(supplier, serviceID, proofTxHash string, relays, computeUnits int64) {
	if proofTxHash == "" {
		recordSessionLoss(supplier, serviceID, "proof_window_closed", relays, computeUnits)
		return
	}
	recordSessionAttemptFailure(supplier, serviceID, "proof_window_closed")
}

// RecordProofTxError records a proof whose transaction did not go out.
//
// The session IS in the book here -- a proof is only attempted for a claim the
// chain accepted -- so unlike the claim side this money has to leave through a
// named door. Resolvable means the reconciler holds an entry and will answer:
// the money waits in `unresolved` until it does. Not resolvable means nothing
// will ever answer, and it is lost now.
func RecordProofTxError(supplier, serviceID string, resolvable bool, relays, computeUnits int64) {
	if resolvable {
		RecordSessionUnresolvedOpened(
			supplier, serviceID, "proof_tx_error", string(RebroadcastPhaseProof), relays, computeUnits)
		return
	}
	recordSessionLoss(supplier, serviceID, "proof_tx_error", relays, computeUnits)
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

	// ProofSkippedReasonClaimedRootUnavailable — the session has no
	// authoritative root to anchor a proof on and never will: no provider
	// is wired, or the SMST answered that it holds no root. Terminal; the
	// session goes to SessionStateProofTxError.
	ProofSkippedReasonClaimedRootUnavailable = "claimed_root_unavailable"

	// ProofSkippedReasonClaimedRootUnreadable — the root could not be read
	// this block (Redis unreachable, pool timeout, shutdown). NOT terminal:
	// the session is written back to claimed and the per-block engine tries
	// again until the proof window closes. This counter rising while
	// sessions_failed_total{reason="proof_window_closed"} stays flat is the
	// signal the deferral is working.
	//
	// It counts ATTEMPTS, not sessions, and it is NOT a denominator: one
	// session deferred for four blocks increments it four times. The count
	// of sessions that actually lost their proof is
	// sessions_failed_total{reason="proof_window_closed"}.
	ProofSkippedReasonClaimedRootUnreadable = "claimed_root_unreadable"
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

// RecordRelayLostToPanic counts ONE relay that was served and will never be
// billed because processing it panicked.
//
// It increments relays_lost_total alone, deliberately: the session-level
// recorder next to it also moves sessions_failed_total and
// compute_units_lost_total, and a single panicked relay is not a failed session.
//
// This exists because the panic path counted NOTHING. logging.PanicRecoveriesTotal
// says a panic happened; nothing said a relay had already been served to the
// client, at the supplier's expense, and then dropped.
//
// It stays on relays_lost_total rather than moving to relays_forgone_total, and
// that is a deliberate INCONSISTENCY with the rest of this family: a panicked
// relay never reached a tree, so strictly it never entered the claim book. What
// keeps it here is that relays_lost_total is not a term in the money identity --
// only the uPOKT series are -- so the reason costs nothing there, while moving it
// would rewrite a Help that says where panic_recovered lives and is not this
// change's to rewrite. Worth revisiting with the relays view as a whole.
func RecordRelayLostToPanic(supplier, serviceID string) {
	relaysLostTotal.WithLabelValues(supplier, serviceID, "panic_recovered").Add(1)
}

// RecordRelayBatchReleased counts n relays a batch handed back at a flush.
func RecordRelayBatchReleased(supplier, reason string, n int) {
	if n <= 0 {
		return
	}
	relayBatchReleasedTotal.WithLabelValues(supplier, reason).Add(float64(n))
}

// RecordRelayBatchFlush counts one relay batch flush by its trigger.
func RecordRelayBatchFlush(supplier, trigger string) {
	relayBatchFlushesTotal.WithLabelValues(supplier, trigger).Inc()
}

// RecordRelayBatchPanic counts one relay batch whose flush panicked.
func RecordRelayBatchPanic(supplier string) {
	relayBatchPanicsTotal.WithLabelValues(supplier).Inc()
}

// RecordRelaysRejected is RecordRelayRejected for n relays at once, on the same
// series, for a batch that learns about its rejections together.
func RecordRelaysRejected(supplier, reason, serviceID string, n int) {
	relaysRejected.WithLabelValues(supplier, reason, serviceID).Add(float64(n))
}

// Register every {phase, cause} series at zero.
//
// A counter child does not exist until it is first incremented, so before the
// first occurrence a query for these returns NO DATA -- which an operator reads
// as "this never happens" and which is indistinguishable from the metric not
// existing at all. That is not hypothetical here: `poll_dropped` was documented
// as an outcome in two files and never had an emitter, so anyone who looked
// for it concluded saturation does not occur.
//
// The sets are the closed ones declared in inclusion_reconciler.go. Registering
// the cross product by hand rather than with EagerCounterChildren because that
// helper takes a single label value and these vectors carry two.
func init() {
	for _, state := range []string{
		string(tx.InclusionReadAvailable),
		string(tx.InclusionReadUnavailable),
		string(tx.InclusionReadUnknown),
	} {
		inclusionReadState.WithLabelValues(state)
	}
	for _, cause := range []string{
		tx.TxInclusionUnknown.String(),
		tx.TxInclusionNotInBlock.String(),
		tx.TxInclusionIncludedOK.String(),
		tx.TxInclusionIncludedFailed.String(),
	} {
		for _, phase := range []string{string(RebroadcastPhaseClaim), string(RebroadcastPhaseProof)} {
			inclusionMissingCauseTotal.WithLabelValues(phase, cause)
		}
	}
	for _, cause := range []string{rejectionRootMismatch, rejectionRootMatch, rejectionRootUnknown} {
		proofRejectionDiagnosisTotal.WithLabelValues(cause)
	}
	for _, phase := range []string{string(RebroadcastPhaseClaim), string(RebroadcastPhaseProof)} {
		inclusionEntryDroppedTotal.WithLabelValues(phase, dropCauseCorrupt)
		inclusionClearFailedTotal.WithLabelValues(phase)
		for _, cause := range []string{
			abandonCauseListFailed, abandonCauseParamsFailed,
			abandonCauseIndexMalformed, abandonCauseIndexUnreadable,
			abandonCauseBudgetExhausted,
		} {
			inclusionGroupAbandonedTotal.WithLabelValues(phase, cause)
		}
	}
}

// SetInclusionReadState publishes which state the post-inclusion read is in,
// leaving the other two series at zero rather than deleting them.
//
// Keeping them is what makes the signal readable: a state whose series vanishes
// cannot be told from one that was never registered, and the whole reason this
// gauge exists is that "I have not looked" and "it works" must not look alike.
func SetInclusionReadState(state tx.InclusionReadState) {
	for _, s := range []tx.InclusionReadState{
		tx.InclusionReadAvailable,
		tx.InclusionReadUnavailable,
		tx.InclusionReadUnknown,
	} {
		v := 0.0
		if s == state {
			v = 1.0
		}
		inclusionReadState.WithLabelValues(string(s)).Set(v)
	}
}
