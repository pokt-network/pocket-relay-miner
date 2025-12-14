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

	// Block/height fields
	FieldBlockHeight = "block_height"
	FieldHeight      = "height"

	// Operation fields
	FieldOperation = "operation"
	FieldAction    = "action"
	FieldMethod    = "method"
	FieldResult    = "result"
	FieldReason    = "reason"
	FieldSource    = "source"

	// Network/connection fields
	FieldAddr       = "addr"
	FieldListenAddr = "listen_addr"
	FieldRemoteAddr = "remote_addr"

	// Redis/stream fields
	FieldStreamID     = "stream_id"
	FieldMessageID    = "message_id"
	FieldConsumerName = "consumer_name"

	// Transaction fields
	FieldTxHash = "tx_hash"

	// Timing fields
	FieldDuration  = "duration"
	FieldLatency   = "latency"
	FieldTimestamp = "timestamp"

	// Count/size fields
	FieldCount     = "count"
	FieldSize      = "size"
	FieldBatchSize = "batch_size"

	// State fields
	FieldOldState = "old_state"
	FieldNewState = "new_state"
	FieldStatus   = "status"

	// Cache fields
	FieldCacheType  = "cache_type"
	FieldCacheLevel = "cache_level"

	// Error fields
	FieldErrorType = "error_type"
	FieldAttempt   = "attempt"
	FieldMaxRetry  = "max_retries"

	// Relay fields
	FieldRelayHash = "relay_hash"

	// Query fields
	FieldQueryType   = "query_type"
	FieldQueryClient = "query_client"
)

// Component name constants for the "component" field.
// These identify the source of log messages.
const (
	ComponentProxyServer        = "proxy_server"
	ComponentWebsocketBridge    = "websocket_bridge"
	ComponentGRPCBridge         = "grpc_bridge"
	ComponentHTTPStream         = "http_stream"
	ComponentRelayProcessor     = "relay_processor"
	ComponentRelayValidator     = "relay_validator"
	ComponentRelayMeter         = "relay_meter"
	ComponentHealthChecker      = "health_checker"
	ComponentService            = "ha_relayer"
	ComponentDifficultyProvider = "difficulty_provider"

	ComponentSessionLifecycle  = "session_lifecycle"
	ComponentSessionStore      = "session_store"
	ComponentClaimPipeline     = "claim_pipeline"
	ComponentClaimBatcher      = "claim_batcher"
	ComponentProofPipeline     = "proof_pipeline"
	ComponentProofBatcher      = "proof_batcher"
	ComponentProofChecker      = "proof_requirement_checker"
	ComponentLeaderElector     = "leader_elector"
	ComponentSupplierManager   = "supplier_manager"
	ComponentSupplierRegistry  = "supplier_registry"
	ComponentSubmissionTiming  = "submission_timing"
	ComponentSubmissionSched   = "submission_scheduler"
	ComponentSMSTRecovery      = "smst_recovery"
	ComponentSMSTSnapshot      = "smst_snapshot_manager"
	ComponentCacheOrchestrator = "cache_orchestrator"
	ComponentWAL               = "wal"
	ComponentDeduplicator      = "deduplicator"
	ComponentSupplierDrain     = "supplier_drain"

	ComponentTxClient = "tx_client"

	ComponentBlockSubscriber   = "block_subscriber"
	ComponentSessionCache      = "session_cache"
	ComponentSupplierTxClient  = "supplier_tx_client"
	ComponentLifecycleCallback = "lifecycle_callback"
	ComponentSharedParamCache  = "shared_param_cache"
	ComponentParamsRefresher   = "params_refresher"
	ComponentBalanceMonitor    = "balance_monitor"
	ComponentBlockHealth       = "block_health_monitor"

	ComponentRedisPublisher = "redis_streams_publisher"
	ComponentRedisConsumer  = "redis_streams_consumer"
	ComponentRedisClient    = "redis_client"

	ComponentKeyManager       = "key_manager"
	ComponentKeyFileProvider  = "key_file_provider"
	ComponentKeyRingProvider  = "keyring_provider"
	ComponentSupplierKeysFile = "supplier_keys_file"

	ComponentQueryClients  = "query_clients"
	ComponentQueryShared   = "query_shared"
	ComponentQuerySession  = "query_session"
	ComponentQueryApp      = "query_application"
	ComponentQuerySupplier = "query_supplier"
	ComponentQueryProof    = "query_proof"
	ComponentQueryService  = "query_service"
	ComponentQueryAccount  = "query_account"

	ComponentObservability  = "observability_server"
	ComponentRuntimeMetrics = "runtime_metrics_collector"
)

// Cache level constants for the "cache_level" field.
const (
	CacheLevelL1      = "l1"
	CacheLevelL2      = "l2"
	CacheLevelL2Retry = "l2_retry"
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

	// FieldSessionStartHeight is the session start block height
	FieldSessionStartHeight = "session_start_height"
)
