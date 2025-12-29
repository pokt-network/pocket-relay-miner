package miner

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

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

	relaysAddedToSMST = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_added_to_smst_total",
			Help:      "Total number of relays successfully added to SMST tree",
		},
		[]string{"supplier", "service_id", "session_id"},
	)

	relaysFailedSMST = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_failed_smst_total",
			Help:      "Total number of relays that failed to add to SMST tree",
		},
		[]string{"supplier", "service_id", "session_id", "reason"},
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

	// ====== SERVICE FACTOR METRICS ======
	// These metrics track claim ceiling events when claims exceed configured limits

	claimCeilingExceededTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_ceiling_exceeded_total",
			Help:      "Total number of claims that exceeded the configured ceiling (potential unpaid work)",
		},
		[]string{"supplier", "service_id"},
	)

	claimCeilingExceededUpokt = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_ceiling_exceeded_upokt",
			Help:      "Total uPOKT claimed above the configured ceiling (potential unpaid amount)",
		},
		[]string{"supplier", "service_id"},
	)

	// Legacy metric for backward compatibility (deprecated - use compute_units_proved_total)
	computeUnitsSettledTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_settled_total",
			Help:      "DEPRECATED: Use compute_units_proved_total instead. Total compute units settled (proven) across all sessions",
		},
		[]string{"supplier", "service_id"},
	)

	// ====== ON-CHAIN SETTLEMENT TRACKING METRICS ======
	// These metrics track actual on-chain settlement results from EventClaimSettled events

	claimsSettledByStatus = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_settled_by_status_total",
			Help:      "Total claims settled on-chain by their validation status (proven=1, invalid=2, expired=3)",
		},
		[]string{"supplier", "service_id", "status"},
	)

	upoktEarnedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_earned_total",
			Help:      "Total uPOKT actually earned from on-chain claim settlements (supplier's share from reward_distribution)",
		},
		[]string{"supplier", "service_id"},
	)

	computeUnitsSettledByStatus = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_settled_by_status_total",
			Help:      "Total compute units settled on-chain by validation status",
		},
		[]string{"supplier", "service_id", "status"},
	)

	relaysSettledByStatus = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_settled_by_status_total",
			Help:      "Total relays settled on-chain by validation status (from num_relays field)",
		},
		[]string{"supplier", "service_id", "status"},
	)

	settlementLatencyBlocks = observability.MinerFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "settlement_latency_blocks",
			Help:      "Blocks between session end and claim settlement",
			Buckets:   []float64{5, 10, 15, 20, 25, 30, 40, 50, 75, 100},
		},
		[]string{"supplier", "status"},
	)

	// EventClaimExpired metrics (proof missing/invalid)
	claimsExpiredByReason = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_expired_by_reason_total",
			Help:      "Total claims expired on-chain by expiration reason (proof_missing, proof_invalid)",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	relaysExpiredByReason = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_expired_by_reason_total",
			Help:      "Total relays lost due to claim expiration by reason",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	computeUnitsExpiredByReason = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "compute_units_expired_by_reason_total",
			Help:      "Total compute units lost due to claim expiration by reason",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	upoktExpiredByReason = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "upokt_expired_by_reason_total",
			Help:      "Total uPOKT lost due to claim expiration by reason (from claimed_upokt field)",
		},
		[]string{"supplier", "service_id", "reason"},
	)

	// EventSupplierSlashed metrics
	supplierSlashedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "supplier_slashed_total",
			Help:      "Total number of times supplier was slashed for missing proofs",
		},
		[]string{"supplier", "service_id"},
	)

	// EventClaimDiscarded metrics
	claimsDiscardedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_discarded_total",
			Help:      "Total claims discarded due to unexpected errors (prevents chain halts)",
		},
		[]string{"supplier", "service_id"},
	)

	// Settlement monitor operational metrics
	blockResultsRetriesTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "block_results_retries_total",
			Help:      "Total number of block_results query retries due to ABCI indexing delays",
		},
		[]string{"height"},
	)

	// Deduplication metrics
	dedupLocalCacheHits = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_local_cache_hits_total",
			Help:      "Total number of deduplication local cache hits",
		},
		[]string{"session_id"},
	)

	dedupRedisCacheHits = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_redis_cache_hits_total",
			Help:      "Total number of deduplication Redis cache hits",
		},
		[]string{"session_id"},
	)

	dedupMisses = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_misses_total",
			Help:      "Total number of deduplication cache misses (new relays)",
		},
		[]string{"session_id"},
	)

	dedupMarked = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_marked_total",
			Help:      "Total number of relays marked as processed",
		},
		[]string{"session_id"},
	)

	dedupErrors = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "dedup_errors_total",
			Help:      "Total number of deduplication errors",
		},
		[]string{"session_id", "operation"},
	)

	// Session tree metrics (reserved for future instrumentation)
	_ = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_trees_active",
			Help:      "Number of active session trees",
		},
		[]string{"supplier"},
	)

	_ = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_tree_updates_total",
			Help:      "Total number of session tree updates",
		},
		[]string{"supplier", "session_id"},
	)

	_ = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_tree_flushes_total",
			Help:      "Total number of session tree flushes",
		},
		[]string{"supplier"},
	)

	_ = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_tree_errors_total",
			Help:      "Total number of session tree errors",
		},
		[]string{"supplier", "operation"},
	)

	// Claim and proof metrics (claimsCreated reserved for future instrumentation)
	_ = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claims_created_total",
			Help:      "Total number of claims created",
		},
		[]string{"supplier"},
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

	// proofsCreated reserved for future instrumentation
	_ = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proofs_created_total",
			Help:      "Total number of proofs created",
		},
		[]string{"supplier"},
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

	// Redis consumer metrics (reserved for future instrumentation)
	_ = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "consumer_lag",
			Help:      "Number of messages pending in the consumer group",
		},
		[]string{"supplier"},
	)

	_ = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "messages_acknowledged_total",
			Help:      "Total number of Redis messages acknowledged",
		},
		[]string{"supplier"},
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

	// Leader election metrics (legacy - from old per-supplier leader elector)
	// These are kept for backwards compatibility with miner/leader.go but not actively used
	leaderStatus = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "leader_status_legacy",
			Help:      "LEGACY: Whether this instance is the leader (1=leader, 0=standby) - per supplier",
		},
		[]string{"supplier", "instance"},
	)

	leaderAcquisitions = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "leader_acquisitions_total",
			Help:      "LEGACY: Total number of times this instance acquired leadership",
		},
		[]string{"supplier"},
	)

	leaderLosses = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "leader_losses_total_legacy",
			Help:      "LEGACY: Total number of times this instance lost leadership",
		},
		[]string{"supplier"},
	)

	leaderHeartbeats = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "leader_heartbeats_total",
			Help:      "Total number of successful leader heartbeats",
		},
		[]string{"supplier"},
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

	// Params refresher metrics
	paramsRefreshed = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "params_refreshed_total",
			Help:      "Total number of on-chain params cache refreshes",
		},
		[]string{"param_type"}, // param_type: shared, session, app_stake, service
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

	// Meter cleanup metrics (unused - reserved for future meter cleanup tracking)
	// meterCleanupPublished = observability.MinerFactory.NewCounterVec(
	// 	prometheus.CounterOpts{
	// 		Namespace: metricsNamespace,
	// 		Subsystem: metricsSubsystem,
	// 		Name:      "meter_cleanup_published_total",
	// 		Help:      "Total number of meter cleanup signals published",
	// 	},
	// 	[]string{"supplier"},
	// )

	// Late relay detection metrics - tracks relays that arrived but weren't consumed before claim

	sessionLateRelays = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_late_relays",
			Help:      "Number of late-arriving relays per session (pending in stream but not consumed before claim)",
		},
		[]string{"supplier", "session_id"},
	)

	sessionLateRelaysTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_late_relays_total",
			Help:      "Total number of late-arriving relays across all sessions",
		},
		[]string{"supplier"},
	)

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
)

// =============================================
// METRICS HELPER FUNCTIONS FOR OPERATORS
// =============================================

// RecordRelayConsumedFromStream records a relay consumed from Redis Stream.
func RecordRelayConsumedFromStream(supplier, serviceID string) {
	relaysConsumedFromStream.WithLabelValues(supplier, serviceID).Inc()
}

// RecordRelayAddedToSMST records a relay successfully added to SMST tree.
func RecordRelayAddedToSMST(supplier, serviceID, sessionID string) {
	relaysAddedToSMST.WithLabelValues(supplier, serviceID, sessionID).Inc()
}

// RecordRelayFailedSMST records a relay that failed to add to SMST tree.
func RecordRelayFailedSMST(supplier, serviceID, sessionID, reason string) {
	relaysFailedSMST.WithLabelValues(supplier, serviceID, sessionID, reason).Inc()
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
	computeUnitsSettledTotal.WithLabelValues(supplier, serviceID).Add(cu) // Legacy metric

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

// RecordClaimCeilingExceeded records when a claim exceeds the configured ceiling.
// excessUpokt is the amount of uPOKT claimed above the ceiling.
func RecordClaimCeilingExceeded(supplier, serviceID string, excessUpokt int64) {
	claimCeilingExceededTotal.WithLabelValues(supplier, serviceID).Inc()
	claimCeilingExceededUpokt.WithLabelValues(supplier, serviceID).Add(float64(excessUpokt))
}

// ====== ON-CHAIN SETTLEMENT TRACKING ======

// recordOnChainSettlement is the internal function that records claim settlement metrics.
// This tracks on-chain settlement events (EventClaimSettled) by status.
func recordOnChainSettlement(supplier, serviceID, status string, numRelays, computeUnits, upoktEarned int64, sessionEndHeight, settlementHeight int64) {
	relays := float64(numRelays)
	cu := float64(computeUnits)

	// Track settlement by status
	claimsSettledByStatus.WithLabelValues(supplier, serviceID, status).Inc()

	// Track relays settled by status
	relaysSettledByStatus.WithLabelValues(supplier, serviceID, status).Add(relays)

	// Track compute units settled by status
	computeUnitsSettledByStatus.WithLabelValues(supplier, serviceID, status).Add(cu)

	// Track ACTUAL revenue earned from blockchain (only for proven claims)
	if status == "proven" && upoktEarned > 0 {
		upoktEarnedTotal.WithLabelValues(supplier, serviceID).Add(float64(upoktEarned))
	}

	// Track settlement latency
	latency := settlementHeight - sessionEndHeight
	settlementLatencyBlocks.WithLabelValues(supplier, status).Observe(float64(latency))
}

// recordOnChainExpiration is the internal function that records claim expiration metrics.
// This tracks on-chain expiration events (EventClaimExpired) by reason.
func recordOnChainExpiration(supplier, serviceID, reason string, numRelays, computeUnits, claimedUpokt int64, sessionEndHeight, settlementHeight int64) {
	relays := float64(numRelays)
	cu := float64(computeUnits)
	upokt := float64(claimedUpokt)

	// Track expiration by reason
	claimsExpiredByReason.WithLabelValues(supplier, serviceID, reason).Inc()

	// Track relays lost due to expiration
	relaysExpiredByReason.WithLabelValues(supplier, serviceID, reason).Add(relays)

	// Track compute units lost due to expiration
	computeUnitsExpiredByReason.WithLabelValues(supplier, serviceID, reason).Add(cu)

	// Track uPOKT lost due to expiration (claimed amount that was forfeited)
	upoktExpiredByReason.WithLabelValues(supplier, serviceID, reason).Add(upokt)

	// Track settlement latency for expired claims
	latency := settlementHeight - sessionEndHeight
	settlementLatencyBlocks.WithLabelValues(supplier, "expired").Observe(float64(latency))
}

// RecordClaimSettled records an on-chain claim settlement event.
// status should be one of: "proven" (1), "invalid" (2), "expired" (3).
func RecordClaimSettled(supplier, serviceID, status string, numRelays, computeUnits, upoktEarned int64, sessionEndHeight, settlementHeight int64) {
	recordOnChainSettlement(supplier, serviceID, status, numRelays, computeUnits, upoktEarned, sessionEndHeight, settlementHeight)
}

// RecordClaimExpired records an on-chain claim expiration event.
// reason should be one of: "proof_missing" (1), "proof_invalid" (2), "unspecified" (0).
func RecordClaimExpired(supplier, serviceID, reason string, numRelays, computeUnits, claimedUpokt int64, sessionEndHeight, settlementHeight int64) {
	recordOnChainExpiration(supplier, serviceID, reason, numRelays, computeUnits, claimedUpokt, sessionEndHeight, settlementHeight)
}

// RecordSupplierSlashed records an on-chain supplier slashing event.
func RecordSupplierSlashed(supplier, serviceID, _ string, _, _ int64) {
	// Track slashing events
	supplierSlashedTotal.WithLabelValues(supplier, serviceID).Inc()
}

// RecordClaimDiscarded records an on-chain claim discard event.
func RecordClaimDiscarded(supplier, serviceID, _ string, _, _ int64) {
	// Track claim discard events
	claimsDiscardedTotal.WithLabelValues(supplier, serviceID).Inc()

	// Note: errorMsg provides context on why claim was discarded (not used in metric)
	// These are rare and indicate unexpected issues that could halt the chain
}

// RecordBlockResultsRetry records a retry attempt for block_results query.
func RecordBlockResultsRetry(height int64, _ int) {
	// Track retry attempts (helps identify ABCI indexing lag)
	blockResultsRetriesTotal.WithLabelValues(fmt.Sprintf("%d", height)).Inc()
}
