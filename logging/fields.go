// Package logging provides centralized logging utilities for the HA RelayMiner.
// It defines standardized field names and helper functions to ensure consistent
// structured logging across all HA components.
package logging

// Standard field name constants for structured logging.
// Using constants ensures consistency and prevents typos across the codebase.
const (
	// Component identification
	FieldComponent = "component"
	FieldService   = "service"

	// Miner identification (top-level context)
	FieldMinerID = "miner_id"
	FieldReplica = "replica" // "leader" or "standby"

	// Supplier/operator identification
	FieldSupplier         = "supplier"
	FieldSupplierOperator = "supplier_operator"
	FieldAppAddress       = "app_address"
	FieldInstance         = "instance"

	// Session fields
	FieldSessionID        = "session_id"
	FieldSessionEndHeight = "session_end_height"
	FieldSessionState     = "session_state"

	// Service fields
	FieldServiceID = "service_id"

	FieldAction = "action"
	FieldSource = "source"

	FieldListenAddr = "listen_addr"

	// Redis/stream fields
	FieldStreamID  = "stream_id"
	FieldMessageID = "message_id"

	// Count/size fields
	FieldCount = "count"

	// State fields
	FieldOldState = "old_state"
	FieldNewState = "new_state"

	// Cache fields
	FieldCacheType = "cache_type"

	FieldAttempt  = "attempt"
	FieldMaxRetry = "max_retries"
)

// Component name constants for the "component" field.
// These identify the source of log messages.
const (
	ComponentProxyServer         = "proxy_server"
	ComponentWebsocketBridge     = "websocket_bridge"
	ComponentGRPCBridge          = "grpc_bridge"
	ComponentHTTPStream          = "http_stream"
	ComponentRelayProcessor      = "relay_processor"
	ComponentRelayValidator      = "relay_validator"
	ComponentRelayMeter          = "relay_meter"
	ComponentServiceFactorClient = "service_factor_client"
	ComponentHealthChecker       = "health_checker"
	ComponentDifficultyProvider  = "difficulty_provider"

	ComponentSessionLifecycle      = "session_lifecycle"
	ComponentSessionStore          = "session_store"
	ComponentProofChecker          = "proof_requirement_checker"
	ComponentLeaderElector         = "leader_elector"
	ComponentLeaderController      = "leader_controller"
	ComponentSupplierManager       = "supplier_manager"
	ComponentSupplierRegistry      = "supplier_registry"
	ComponentServiceFactorRegistry = "service_factor_registry"
	ComponentSMSTSnapshot          = "smst_snapshot_manager"
	ComponentCacheOrchestrator     = "cache_orchestrator"
	ComponentDeduplicator          = "deduplicator"
	ComponentSupplierClaimer       = "supplier_claimer"

	ComponentTxClient = "tx_client"

	ComponentBlockSubscriber    = "block_subscriber"
	ComponentSessionCache       = "session_cache"
	ComponentLifecycleCallback  = "lifecycle_callback"
	ComponentSharedParamCache   = "shared_param_cache"
	ComponentSupplierParamCache = "supplier_param_cache"
	ComponentBalanceMonitor     = "balance_monitor"
	ComponentBlockHealth        = "block_health_monitor"

	ComponentRedisPublisher = "redis_streams_publisher"
	ComponentRedisConsumer  = "redis_streams_consumer"

	ComponentKeyManager       = "key_manager"
	ComponentKeyRingProvider  = "keyring_provider"
	ComponentSupplierKeysFile = "supplier_keys_file"

	ComponentQueryClients  = "query_clients"
	ComponentQueryApp      = "query_application"
	ComponentQuerySupplier = "query_supplier"
	ComponentQueryService  = "query_service"
	ComponentQueryAccount  = "query_account"

	ComponentObservability  = "observability_server"
	ComponentRuntimeMetrics = "runtime_metrics_collector"

	ComponentRedisHealthMonitor = "redis_health_monitor"

	// Internal adapters and helpers
	ComponentRelayPipeline           = "relay_pipeline"
	ComponentRedisBlockClientAdapter = "redis_block_client_adapter"
	ComponentBlockSubscriberAdapter  = "block_subscriber_adapter"
)

// Cache type constants for the "cache_type" field.
const (
	CacheTypeSession      = "session"
	CacheTypeSharedParams = "shared_params"
)

// Operation result constants for the "result" field.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultSkipped = "skipped"
	ResultTimeout = "timeout"
)

// Invalidation source constants for the "source" field.
const (
	SourceManual = "manual"
	SourcePubSub = "pubsub"
	SourceBlock  = "block"
)

const (
	// FieldApplication is the application address field
	FieldApplication = "application"
)

// Replica role constants for the "replica" field.
const (
	ReplicaLeader  = "leader"
	ReplicaStandby = "standby"
)
