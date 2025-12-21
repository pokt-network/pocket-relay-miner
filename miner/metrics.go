package miner

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "miner"
)

var (
	// Relay consumption metrics
	relaysConsumed = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_consumed_total",
			Help:      "Total number of relays consumed from Redis streams",
		},
		[]string{"supplier", "service_id"},
	)

	relaysProcessed = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_processed_total",
			Help:      "Total number of relays successfully processed",
		},
		[]string{"supplier", "service_id"},
	)

	relaysDeduplicated = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "relays_deduplicated_total",
			Help:      "Total number of duplicate relays filtered",
		},
		[]string{"supplier", "service_id"},
	)

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

	// Sessions by state - helps operators see session lifecycle status at a glance
	sessionsByState = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_by_state",
			Help:      "Number of sessions in each state (active, claiming, claimed, proving, proved, settled)",
		},
		[]string{"supplier", "state"},
	)

	// Session info - detailed metrics per session for debugging
	sessionRelayCount = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_relay_count",
			Help:      "Number of relays in each session",
		},
		[]string{"supplier", "session_id", "service_id"},
	)

	sessionComputeUnits = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_compute_units",
			Help:      "Total compute units in each session",
		},
		[]string{"supplier", "session_id", "service_id"},
	)

	sessionEndHeight = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_end_height",
			Help:      "End height of each active session",
		},
		[]string{"supplier", "session_id"},
	)

	// Claim timing metrics - helps operators verify timing spread
	claimScheduledHeight = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_scheduled_height",
			Help:      "Block height when claim is scheduled to be submitted",
		},
		[]string{"supplier", "session_id"},
	)

	claimBlocksUntilSubmit = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "claim_blocks_until_submit",
			Help:      "Number of blocks until claim will be submitted (0 if already submitted)",
		},
		[]string{"supplier", "session_id"},
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
		[]string{"supplier", "session_id"},
	)

	proofBlocksUntilSubmit = observability.MinerFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "proof_blocks_until_submit",
			Help:      "Number of blocks until proof will be submitted (0 if already submitted)",
		},
		[]string{"supplier", "session_id"},
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

	sessionsSettledTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_settled_total",
			Help:      "Total number of sessions successfully settled",
		},
		[]string{"supplier", "service_id"},
	)

	sessionsFailedTotal = observability.MinerFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_failed_total",
			Help:      "Total number of sessions that failed (missed claim/proof window)",
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

	// uPOKT - Revenue view (compute units * service rate)
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
			Help:      "Total uPOKT lost due to claim/proof failures",
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
		[]string{"supplier"},
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
		[]string{"supplier"},
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
		[]string{"supplier", "from_state", "to_state"},
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

// RecordRelayConsumed records a relay consumed from the stream.
func RecordRelayConsumed(supplier, serviceID string) {
	relaysConsumed.WithLabelValues(supplier, serviceID).Inc()
}

// RecordRelayProcessed records a relay successfully processed.
func RecordRelayProcessed(supplier, serviceID string) {
	relaysProcessed.WithLabelValues(supplier, serviceID).Inc()
}

// RecordRelayDeduplicated records a relay that was deduplicated.
func RecordRelayDeduplicated(supplier, serviceID string) {
	relaysDeduplicated.WithLabelValues(supplier, serviceID).Inc()
}

// RecordRelayRejected records a relay that was rejected.
func RecordRelayRejected(supplier, reason, serviceID string) {
	relaysRejected.WithLabelValues(supplier, reason, serviceID).Inc()
}

// RecordRelayProcessingLatency records how long it took to process a relay.
func RecordRelayProcessingLatency(supplier string, seconds float64, statusCode string) {
	relayProcessingLatency.WithLabelValues(supplier, statusCode).Observe(seconds)
}

// SetSessionsByState sets the count of sessions in a given state.
func SetSessionsByState(supplier, state string, count float64) {
	sessionsByState.WithLabelValues(supplier, state).Set(count)
}

// RecordSessionCreated increments the session created counter.
func RecordSessionCreated(supplier, serviceID string) {
	sessionsCreatedTotal.WithLabelValues(supplier, serviceID).Inc()
}

// SetSessionRelayCount sets the current relay count for a session.
func SetSessionRelayCount(supplier, sessionID, serviceID string, count float64) {
	sessionRelayCount.WithLabelValues(supplier, sessionID, serviceID).Set(count)
}

// SetSessionComputeUnits sets the current compute units for a session.
func SetSessionComputeUnits(supplier, sessionID, serviceID string, units float64) {
	sessionComputeUnits.WithLabelValues(supplier, sessionID, serviceID).Set(units)
}

// SetSessionEndHeight sets the end height for a session.
func SetSessionEndHeight(supplier, sessionID string, height float64) {
	sessionEndHeight.WithLabelValues(supplier, sessionID).Set(height)
}

// ClearSessionMetrics removes session-specific metrics when session completes.
func ClearSessionMetrics(supplier, sessionID, serviceID string) {
	sessionRelayCount.DeleteLabelValues(supplier, sessionID, serviceID)
	sessionComputeUnits.DeleteLabelValues(supplier, sessionID, serviceID)
	sessionEndHeight.DeleteLabelValues(supplier, sessionID)
	claimScheduledHeight.DeleteLabelValues(supplier, sessionID)
	claimBlocksUntilSubmit.DeleteLabelValues(supplier, sessionID)
	proofScheduledHeight.DeleteLabelValues(supplier, sessionID)
	proofBlocksUntilSubmit.DeleteLabelValues(supplier, sessionID)
}

// SetClaimScheduledHeight sets when a claim is scheduled to be submitted.
func SetClaimScheduledHeight(supplier, sessionID string, height float64) {
	claimScheduledHeight.WithLabelValues(supplier, sessionID).Set(height)
}

// SetClaimBlocksUntilSubmit sets blocks remaining until claim submission.
func SetClaimBlocksUntilSubmit(supplier, sessionID string, blocks float64) {
	claimBlocksUntilSubmit.WithLabelValues(supplier, sessionID).Set(blocks)
}

// RecordClaimSubmissionLatency records how many blocks after window opened the claim was submitted.
func RecordClaimSubmissionLatency(supplier string, blocksAfterWindowOpened float64) {
	claimSubmissionLatencyBlocks.WithLabelValues(supplier).Observe(blocksAfterWindowOpened)
}

// SetProofScheduledHeight sets when a proof is scheduled to be submitted.
func SetProofScheduledHeight(supplier, sessionID string, height float64) {
	proofScheduledHeight.WithLabelValues(supplier, sessionID).Set(height)
}

// SetProofBlocksUntilSubmit sets blocks remaining until proof submission.
func SetProofBlocksUntilSubmit(supplier, sessionID string, blocks float64) {
	proofBlocksUntilSubmit.WithLabelValues(supplier, sessionID).Set(blocks)
}

// RecordProofSubmissionLatency records how many blocks after window opened the proof was submitted.
func RecordProofSubmissionLatency(supplier string, blocksAfterWindowOpened float64) {
	proofSubmissionLatencyBlocks.WithLabelValues(supplier).Observe(blocksAfterWindowOpened)
}

// RecordSessionSettled increments the settled sessions counter.
func RecordSessionSettled(supplier, serviceID string) {
	sessionsSettledTotal.WithLabelValues(supplier, serviceID).Inc()
}

// RecordSessionFailed increments the failed sessions counter.
func RecordSessionFailed(supplier, serviceID, reason string) {
	sessionsFailedTotal.WithLabelValues(supplier, serviceID, reason).Inc()
}

// RecordComputeUnitsClaimed adds to the claimed compute units total.
// DEPRECATED: Use RecordRevenueClaimed instead to track all revenue metrics.
func RecordComputeUnitsClaimed(supplier, serviceID string, units float64) {
	computeUnitsClaimedTotal.WithLabelValues(supplier, serviceID).Add(units)
}

// RecordComputeUnitsSettled adds to the settled compute units total.
// DEPRECATED: Use RecordRevenueProved instead to track all revenue metrics.
func RecordComputeUnitsSettled(supplier, serviceID string, units float64) {
	computeUnitsSettledTotal.WithLabelValues(supplier, serviceID).Add(units)
	computeUnitsProvedTotal.WithLabelValues(supplier, serviceID).Add(units)
}

// RecordRevenueClaimed records successful claim submission across all revenue views.
// This tracks compute units, uPOKT revenue, and relay count when a claim is accepted.
func RecordRevenueClaimed(supplier, serviceID string, computeUnits uint64, relayCount int64) {
	cu := float64(computeUnits)
	relays := float64(relayCount)

	// Compute Units view
	computeUnitsClaimedTotal.WithLabelValues(supplier, serviceID).Add(cu)

	// uPOKT view (compute units are already in uPOKT units - 1:1 mapping)
	upoktClaimedTotal.WithLabelValues(supplier, serviceID).Add(cu)

	// Relays view
	relaysClaimedTotal.WithLabelValues(supplier, serviceID).Add(relays)
}

// RecordRevenueProved records successful proof submission across all revenue views.
// This tracks compute units, uPOKT revenue, and relay count when a proof is accepted.
func RecordRevenueProved(supplier, serviceID string, computeUnits uint64, relayCount int64) {
	cu := float64(computeUnits)
	relays := float64(relayCount)

	// Compute Units view
	computeUnitsProvedTotal.WithLabelValues(supplier, serviceID).Add(cu)
	computeUnitsSettledTotal.WithLabelValues(supplier, serviceID).Add(cu) // Legacy metric

	// uPOKT view (compute units are already in uPOKT units - 1:1 mapping)
	upoktProvedTotal.WithLabelValues(supplier, serviceID).Add(cu)

	// Relays view
	relaysProvedTotal.WithLabelValues(supplier, serviceID).Add(relays)
}

// RecordRevenueLost records failed claim/proof submission across all revenue views.
// This tracks compute units, uPOKT revenue, and relay count when revenue is lost.
// Common reasons: "claim_tx_failed", "proof_tx_failed", "exhausted_retries"
func RecordRevenueLost(supplier, serviceID, reason string, computeUnits uint64, relayCount int64) {
	cu := float64(computeUnits)
	relays := float64(relayCount)

	// Compute Units view
	computeUnitsLostTotal.WithLabelValues(supplier, serviceID, reason).Add(cu)

	// uPOKT view
	upoktLostTotal.WithLabelValues(supplier, serviceID, reason).Add(cu)

	// Relays view
	relaysLostTotal.WithLabelValues(supplier, serviceID, reason).Add(relays)
}

// RecordClaimSubmitted increments the claims submitted counter.
func RecordClaimSubmitted(supplier string) {
	claimsSubmitted.WithLabelValues(supplier).Inc()
}

// RecordClaimError increments the claim errors counter.
func RecordClaimError(supplier, reason string) {
	claimErrors.WithLabelValues(supplier, reason).Inc()
}

// RecordProofSubmitted increments the proofs submitted counter.
func RecordProofSubmitted(supplier string) {
	proofsSubmitted.WithLabelValues(supplier).Inc()
}

// RecordProofError increments the proof errors counter.
func RecordProofError(supplier, reason string) {
	proofErrors.WithLabelValues(supplier, reason).Inc()
}

// SetCurrentBlockHeight sets the current block height.
func SetCurrentBlockHeight(height float64) {
	currentBlockHeight.Set(height)
}

// RecordSessionStateTransition records a state change.
func RecordSessionStateTransition(supplier, fromState, toState string) {
	sessionStateTransitions.WithLabelValues(supplier, fromState, toState).Inc()
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
