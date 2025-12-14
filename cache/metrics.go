package cache

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

const (
	metricsNamespace = "ha"
	metricsSubsystem = "cache"

	// Cache levels for metrics labels
	CacheLevelL1      = "l1"       // In-memory cache
	CacheLevelL2      = "l2"       // Redis cache
	CacheLevelL2Retry = "l2_retry" // Redis cache after waiting for lock
	CacheLevelL3      = "l3"       // Chain query

	// Cache invalidation sources
	SourceManual = "manual" // Manual invalidation via API/code
	SourcePubSub = "pubsub" // Invalidation via pub/sub event

	// Result labels for metrics
	ResultSuccess = "success"
	ResultFailure = "failure"
)

var (
	// Cache hit/miss metrics
	cacheHits = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "hits_total",
			Help:      "Total number of cache hits",
		},
		[]string{"cache_type", "level"}, // level: l1, l2, l2_retry
	)

	cacheMisses = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "misses_total",
			Help:      "Total number of cache misses",
		},
		[]string{"cache_type", "level"},
	)

	cacheInvalidations = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "invalidations_total",
			Help:      "Total number of cache invalidations",
		},
		[]string{"cache_type", "source"}, // source: manual, pubsub
	)

	// Chain query metrics
	chainQueries = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "chain_queries_total",
			Help:      "Total number of chain queries (cache misses that hit chain)",
		},
		[]string{"query_type"},
	)

	chainQueryErrors = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "chain_query_errors_total",
			Help:      "Total number of chain query errors",
		},
		[]string{"query_type"},
	)

	// chainQueryLatency tracks latency of L3 chain queries
	chainQueryLatency = observability.SharedFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "chain_query_latency_seconds",
			Help:      "Latency of chain queries (L3 cache misses)",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"query_type"},
	)

	// cacheGetLatency tracks total cache Get operation latency (all levels)
	cacheGetLatency = observability.SharedFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "get_latency_seconds",
			Help:      "Latency of cache Get operations (includes all cache levels)",
			Buckets:   []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1},
		},
		[]string{"cache_type", "level"}, // level indicates which cache level resolved the query
	)

	// Session cache specific metrics
	sessionRewardableChecks = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "session_rewardable_checks_total",
			Help:      "Total number of session rewardability checks",
		},
		[]string{"result"}, // result: rewardable, non_rewardable
	)

	sessionMarkedNonRewardable = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "sessions_marked_non_rewardable_total",
			Help:      "Total number of sessions marked as non-rewardable",
		},
		[]string{"reason"},
	)

	// Block event metrics
	blockEventsPublished = observability.SharedFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "block_events_published_total",
			Help:      "Total number of block events published",
		},
	)

	blockEventsReceived = observability.SharedFactory.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "block_events_received_total",
			Help:      "Total number of block events received",
		},
	)

	currentBlockHeight = observability.SharedFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "current_block_height",
			Help:      "Current block height as seen by the cache",
		},
	)

	// lockAcquisitions tracks distributed lock acquisitions during L3 queries
	lockAcquisitions = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "lock_acquisitions_total",
			Help:      "Total number of distributed lock acquisitions for cache queries",
		},
		[]string{"cache_type", "result"}, // result: acquired, contended
	)

	// ========================================================================
	// Cache Orchestrator Metrics (for unified cache architecture)
	// ========================================================================

	// cacheOrchestratorRefreshes tracks the number of complete refresh cycles.
	// Each refresh cycle updates all caches in parallel.
	cacheOrchestratorRefreshes = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "orchestrator_refreshes_total",
			Help:      "Total number of cache orchestrator refresh cycles",
		},
		[]string{"result"}, // result: success, failure
	)

	// cacheOrchestratorRefreshDuration tracks how long each refresh cycle takes.
	// This includes the time to refresh all caches in parallel.
	cacheOrchestratorRefreshDuration = observability.SharedFactory.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "orchestrator_refresh_duration_seconds",
			Help:      "Duration of cache orchestrator refresh cycles",
			Buckets:   []float64{0.1, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0}, // 100ms to 30s
		},
	)

	// cacheOrchestratorLeaderStatus indicates whether this instance is the global leader.
	// 1 = leader (performs cache refresh), 0 = follower (only consumes cache).
	cacheOrchestratorLeaderStatus = observability.SharedFactory.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "orchestrator_leader_status",
			Help:      "Whether this instance is the global cache refresh leader (1=leader, 0=follower)",
		},
	)

	// NOTE: Refresh metrics removed - Refresh() now calls Get(force=true) which uses:
	// - chainQueries: Tracks L3 queries
	// - chainQueryLatency: Tracks L3 query duration
	// - chainQueryErrors: Tracks L3 query errors
	// - cacheGetLatency: Tracks overall Get() latency including L3
)
