package relayer

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// ValidationMode determines when relay requests are validated.
type ValidationMode string

const (
	// ValidationModeEager validates ALL requests before forwarding to backend.
	// Use for expensive backends (LLMs, paid APIs) where invalid requests cost money.
	ValidationModeEager ValidationMode = "eager"

	// ValidationModeOptimistic serves first, validates in background.
	// Use for cheap/fast backends where throughput is prioritized.
	ValidationModeOptimistic ValidationMode = "optimistic"
)

// Backend type constants matching on-chain RPCType enum.
// Reference: poktroll/x/shared/types/service.pb.go
const (
	BackendTypeJSONRPC   = "jsonrpc"   // JSON-RPC (RPCType_JSON_RPC = 3)
	BackendTypeREST      = "rest"      // REST (RPCType_REST = 4)
	BackendTypeWebSocket = "websocket" // WebSocket (RPCType_WEBSOCKET = 2)
	BackendTypeGRPC      = "grpc"      // gRPC (RPCType_GRPC = 1)
	BackendTypeCometBFT  = "cometbft"  // CometBFT (RPCType_COMET_BFT = 5)
)

// DefaultBackendType is the default backend type when not configured.
const DefaultBackendType = BackendTypeJSONRPC

// RPCTypeToBackendType converts numeric RPCType codes (from Rpc-Type header) to backend type strings.
// This maps the on-chain RPCType enum values to configuration keys.
//
// Mapping:
//   - "1" → "grpc" (RPCType_GRPC = 1)
//   - "2" → "websocket" (RPCType_WEBSOCKET = 2)
//   - "3" → "jsonrpc" (RPCType_JSON_RPC = 3)
//   - "4" → "rest" (RPCType_REST = 4)
//   - "5" → "cometbft" (RPCType_COMET_BFT = 5)
//
// If the input is not a numeric code, it's returned unchanged (already a backend type name).
func RPCTypeToBackendType(rpcType string) string {
	// Convert string to int and map using protobuf enum values
	switch rpcType {
	case fmt.Sprint(int(sharedtypes.RPCType_GRPC)):
		return BackendTypeGRPC
	case fmt.Sprint(int(sharedtypes.RPCType_WEBSOCKET)):
		return BackendTypeWebSocket
	case fmt.Sprint(int(sharedtypes.RPCType_JSON_RPC)):
		return BackendTypeJSONRPC
	case fmt.Sprint(int(sharedtypes.RPCType_REST)):
		return BackendTypeREST
	case fmt.Sprint(int(sharedtypes.RPCType_COMET_BFT)):
		return BackendTypeCometBFT
	default:
		// Already a backend type name (e.g., "grpc", "jsonrpc")
		// or unknown - return as-is and let backend lookup handle it
		return rpcType
	}
}

// Config is the configuration for the HA Relayer service.
type Config struct {
	// ListenAddr is the address to listen on for incoming relay requests.
	// Format: "host:port" (e.g., "0.0.0.0:8080")
	ListenAddr string `yaml:"listen_addr"`

	// Redis configuration
	Redis RedisConfig `yaml:"redis"`

	// PocketNode is the configuration for connecting to the Pocket blockchain.
	PocketNode PocketNodeConfig `yaml:"pocket_node"`

	// Keys configuration for supplier signing keys.
	// Required for signing relay responses.
	Keys KeysConfig `yaml:"keys"`

	// Services is a map of service configurations keyed by service ID.
	Services map[string]ServiceConfig `yaml:"services"`

	// DefaultValidationMode is the default validation mode for services.
	// Can be overridden per-service.
	DefaultValidationMode ValidationMode `yaml:"default_validation_mode"`

	// DefaultRequestTimeoutSeconds is the default timeout for backend requests.
	DefaultRequestTimeoutSeconds int64 `yaml:"default_request_timeout_seconds"`

	// DefaultMaxBodySizeBytes is the default max body size for requests/responses.
	DefaultMaxBodySizeBytes int64 `yaml:"default_max_body_size_bytes"`

	// Metrics configuration
	Metrics MetricsConfig `yaml:"metrics"`

	// HealthCheck configuration for the relayer itself
	HealthCheck HealthCheckConfig `yaml:"health_check"`

	// GracePeriodExtraBlocks is additional grace period blocks beyond on-chain config.
	// Helps handle clock drift and network delays between gateway and relayer.
	GracePeriodExtraBlocks int64 `yaml:"grace_period_extra_blocks"`

	// CacheWarmup configuration for pre-warming caches at startup.
	CacheWarmup CacheWarmupConfig `yaml:"cache_warmup,omitempty"`

	// Logging configuration
	Logging logging.Config `yaml:"logging,omitempty"`

	// RelayMeter configuration for rate limiting based on app stakes
	RelayMeter RelayMeterYAMLConfig `yaml:"relay_meter,omitempty"`
}

// RedisConfig contains Redis connection configuration.
type RedisConfig struct {
	// URL is the Redis connection URL.
	// Supports: redis://, rediss://, redis-sentinel://, redis-cluster://
	URL string `yaml:"url"`

	// StreamPrefix is the prefix for Redis stream names.
	StreamPrefix string `yaml:"stream_prefix"`

	// MaxStreamLen is the maximum length of each supplier stream.
	MaxStreamLen int64 `yaml:"max_stream_len"`
}

// PocketNodeConfig contains Pocket blockchain connection configuration.
type PocketNodeConfig struct {
	// QueryNodeRPCUrl is the URL for RPC queries.
	QueryNodeRPCUrl string `yaml:"query_node_rpc_url"`

	// QueryNodeGRPCUrl is the URL for gRPC queries.
	QueryNodeGRPCUrl string `yaml:"query_node_grpc_url"`
}

// ServiceConfig contains configuration for a single service.
// The service ID is the map key in Config.Services.
// All backends must be specified per RPC type in backends map.
// ComputeUnitsPerRelay is fetched from the on-chain service entity.
type ServiceConfig struct {
	// ValidationMode overrides the default validation mode for this service.
	ValidationMode ValidationMode `yaml:"validation_mode,omitempty"`

	// RequestTimeoutSeconds overrides the default timeout for this service.
	RequestTimeoutSeconds int64 `yaml:"request_timeout_seconds,omitempty"`

	// MaxBodySizeBytes overrides the default max body size for this service.
	MaxBodySizeBytes int64 `yaml:"max_body_size_bytes,omitempty"`

	// DefaultBackend specifies which backend to use when no Rpc-Type header is provided.
	// Must match one of the keys in the Backends map.
	// Valid values: "jsonrpc", "rest", "websocket", "grpc", "cometbft"
	// If not set, defaults to "jsonrpc"
	DefaultBackend string `yaml:"default_backend,omitempty"`

	// Backends contains backend configuration per RPC type.
	// Key is RPC type: "jsonrpc", "rest", "websocket", "grpc", "cometbft"
	// At least one backend type is required.
	Backends map[string]BackendConfig `yaml:"backends"`
}

// BackendConfig contains configuration for a specific RPC type backend.
type BackendConfig struct {
	// URL is the backend URL for this RPC type.
	// Supports http://, https://, ws://, wss://, grpc://, grpcs://
	URL string `yaml:"url"`

	// Headers are additional headers for this backend.
	Headers map[string]string `yaml:"headers,omitempty"`

	// Authentication for this backend.
	Authentication *AuthenticationConfig `yaml:"authentication,omitempty"`

	// HealthCheck configuration for this backend.
	HealthCheck *BackendHealthCheckConfig `yaml:"health_check,omitempty"`
}

// AuthenticationConfig contains authentication configuration for a backend.
type AuthenticationConfig struct {
	// Username for basic auth.
	Username string `yaml:"username,omitempty"`

	// Password for basic auth.
	Password string `yaml:"password,omitempty"`

	// BearerToken for bearer auth (alternative to basic auth).
	BearerToken string `yaml:"bearer_token,omitempty"`
}

// BackendHealthCheckConfig contains health check configuration for a backend.
type BackendHealthCheckConfig struct {
	// Enabled enables health checking for this backend.
	Enabled bool `yaml:"enabled"`

	// Endpoint is the health check endpoint path (e.g., "/health").
	Endpoint string `yaml:"endpoint"`

	// IntervalSeconds is how often to check health.
	IntervalSeconds int64 `yaml:"interval_seconds"`

	// TimeoutSeconds is the timeout for health check requests.
	TimeoutSeconds int64 `yaml:"timeout_seconds"`

	// UnhealthyThreshold is how many failures before marking unhealthy.
	UnhealthyThreshold int `yaml:"unhealthy_threshold"`

	// HealthyThreshold is how many successes before marking healthy.
	HealthyThreshold int `yaml:"healthy_threshold"`
}

// MetricsConfig contains metrics server configuration.
type MetricsConfig struct {
	// Enabled enables the metrics server.
	Enabled bool `yaml:"enabled"`

	// Addr is the address for the metrics server.
	Addr string `yaml:"addr"`
}

// HealthCheckConfig contains health check server configuration for the relayer.
type HealthCheckConfig struct {
	// Enabled enables the health check endpoint.
	Enabled bool `yaml:"enabled"`

	// Addr is the address for the health check server.
	Addr string `yaml:"addr"`
}

// KeysConfig contains key provider configuration for supplier signing keys.
type KeysConfig struct {
	// KeysFile is the path to a supplier-keys.yaml file with hex-encoded keys.
	KeysFile string `yaml:"keys_file,omitempty"`

	// KeysDir is a directory containing individual key files.
	KeysDir string `yaml:"keys_dir,omitempty"`

	// Keyring configuration for Cosmos SDK keyring.
	Keyring *KeyringConfig `yaml:"keyring,omitempty"`
}

// KeyringConfig contains Cosmos SDK keyring configuration.
type KeyringConfig struct {
	// Backend is the keyring backend type: "file", "os", "test", "memory"
	Backend string `yaml:"backend"`

	// Dir is the directory containing the keyring (for "file" backend).
	Dir string `yaml:"dir,omitempty"`

	// AppName is the application name for the keyring.
	// Default: "pocket"
	AppName string `yaml:"app_name,omitempty"`

	// KeyNames is a list of key names to load from the keyring.
	// If empty, all keys are loaded.
	KeyNames []string `yaml:"key_names,omitempty"`
}

// SupplierCacheConfig contains configuration for the shared supplier state cache.
type SupplierCacheConfig struct {
	// KeyPrefix is the Redis key prefix for supplier state.
	// Default: "ha:supplier"
	KeyPrefix string `yaml:"key_prefix"`

	// FailOpen determines behavior when Redis is unavailable.
	// If true, accept relays when cache unavailable (safer for traffic).
	// If false, reject relays when cache unavailable (safer for validation).
	// Default: true (fail open - prioritize serving traffic)
	FailOpen bool `yaml:"fail_open"`
}

// RelayMeterYAMLConfig contains YAML configuration for the relay meter.
// This is converted to relayer.RelayMeterConfig when instantiating the RelayMeter.
type RelayMeterYAMLConfig struct {
	// Enabled enables relay metering and rate limiting.
	// Default: true
	Enabled bool `yaml:"enabled"`

	// OverServicingEnabled allows relays to exceed app stake limits temporarily.
	// Default: false (strict enforcement)
	OverServicingEnabled bool `yaml:"over_servicing_enabled"`

	// RedisKeyPrefix is the prefix for Redis keys used by the relay meter.
	// Default: "ha"
	RedisKeyPrefix string `yaml:"redis_key_prefix"`

	// FailBehavior determines behavior when Redis is unavailable.
	// "open" - Allow relays when Redis down (prioritize availability)
	// "closed" - Reject relays when Redis down (prioritize safety)
	// Default: "open"
	FailBehavior string `yaml:"fail_behavior"`

	// SessionCleanupInterval is how often to clean up expired sessions.
	// Default: 5 minutes
	SessionCleanupInterval time.Duration `yaml:"session_cleanup_interval"`

	// ParamsCacheTTL is the TTL for cached shared/session params.
	// Should be session-wide duration to avoid stale data.
	// Default: 10 minutes
	ParamsCacheTTL time.Duration `yaml:"params_cache_ttl"`

	// AppStakeCacheTTL is the TTL for cached app stakes.
	// Default: 10 minutes
	AppStakeCacheTTL time.Duration `yaml:"app_stake_cache_ttl"`
}

// CacheWarmupConfig contains configuration for cache pre-warming at startup.
// This helps reduce latency for the first requests by pre-loading application data.
type CacheWarmupConfig struct {
	// Enabled enables cache warmup at startup.
	// Default: false
	Enabled bool `yaml:"enabled"`

	// KnownApplications is a list of application addresses to pre-warm on startup.
	// These are applications the operator knows will send traffic.
	KnownApplications []string `yaml:"known_applications,omitempty"`

	// PersistDiscoveredApps enables saving discovered application addresses to Redis.
	// When enabled, apps discovered during runtime are saved to Redis and loaded
	// on subsequent restarts for faster warmup.
	// Default: true (when enabled)
	PersistDiscoveredApps bool `yaml:"persist_discovered_apps"`

	// WarmupConcurrency is the number of parallel warmup operations.
	// Higher values = faster warmup but more load on the chain.
	// Default: 10
	WarmupConcurrency int `yaml:"warmup_concurrency,omitempty"`

	// WarmupTimeoutSeconds is the timeout for warming each application.
	// Default: 5
	WarmupTimeoutSeconds int64 `yaml:"warmup_timeout_seconds,omitempty"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ListenAddr: "0.0.0.0:8080",
		Redis: RedisConfig{
			URL:          "redis://localhost:6379",
			StreamPrefix: "ha:relays",
			MaxStreamLen: 100000,
		},
		DefaultValidationMode:        ValidationModeOptimistic,
		DefaultRequestTimeoutSeconds: 30,
		DefaultMaxBodySizeBytes:      10 * 1024 * 1024, // 10MB
		GracePeriodExtraBlocks:       2,
		Metrics: MetricsConfig{
			Enabled: true,
			Addr:    "0.0.0.0:9090",
		},
		HealthCheck: HealthCheckConfig{
			Enabled: true,
			Addr:    "0.0.0.0:8081",
		},
		RelayMeter: RelayMeterYAMLConfig{
			Enabled:                true,
			OverServicingEnabled:   false,
			RedisKeyPrefix:         "ha",
			FailBehavior:           "open",
			SessionCleanupInterval: 5 * time.Minute,
			ParamsCacheTTL:         10 * time.Minute,
			AppStakeCacheTTL:       10 * time.Minute,
		},
	}
}

// Validate validates the configuration and returns an error if invalid.
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}

	if c.Redis.URL == "" {
		return fmt.Errorf("redis.url is required")
	}

	if _, err := url.Parse(c.Redis.URL); err != nil {
		return fmt.Errorf("invalid redis.url: %w", err)
	}

	if c.PocketNode.QueryNodeRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_rpc_url is required")
	}

	if c.PocketNode.QueryNodeGRPCUrl == "" {
		return fmt.Errorf("pocket_node.query_node_grpc_url is required")
	}

	if len(c.Services) == 0 {
		return fmt.Errorf("at least one service must be configured")
	}

	for id, svc := range c.Services {
		if err := c.validateServiceConfig(id, svc); err != nil {
			return err
		}
	}

	if c.DefaultValidationMode != ValidationModeEager && c.DefaultValidationMode != ValidationModeOptimistic {
		return fmt.Errorf("invalid default_validation_mode: %s", c.DefaultValidationMode)
	}

	return nil
}

// validateServiceConfig validates a single service configuration.
// The id parameter is the map key from Config.Services.
func (c *Config) validateServiceConfig(id string, svc ServiceConfig) error {
	// At least one backend is required
	if len(svc.Backends) == 0 {
		return fmt.Errorf("service[%s].backends is required: at least one backend type must be configured", id)
	}

	if svc.ValidationMode != "" &&
		svc.ValidationMode != ValidationModeEager &&
		svc.ValidationMode != ValidationModeOptimistic {
		return fmt.Errorf("service[%s].validation_mode is invalid: %s", id, svc.ValidationMode)
	}

	// Validate each backend
	for rpcType, backend := range svc.Backends {
		if backend.URL == "" {
			return fmt.Errorf("service[%s].backends[%s].url is required", id, rpcType)
		}
		if _, err := url.Parse(backend.URL); err != nil {
			return fmt.Errorf("service[%s].backends[%s].url is invalid: %w", id, rpcType, err)
		}

		// Validate health check config if present
		if backend.HealthCheck != nil && backend.HealthCheck.Enabled {
			if backend.HealthCheck.Endpoint == "" {
				return fmt.Errorf("service[%s].backends[%s].health_check.endpoint is required when enabled", id, rpcType)
			}
			if backend.HealthCheck.IntervalSeconds <= 0 {
				return fmt.Errorf("service[%s].backends[%s].health_check.interval_seconds must be positive", id, rpcType)
			}
		}
	}

	return nil
}

// GetServiceValidationMode returns the validation mode for a service.
func (c *Config) GetServiceValidationMode(serviceID string) ValidationMode {
	if svc, ok := c.Services[serviceID]; ok && svc.ValidationMode != "" {
		return svc.ValidationMode
	}
	return c.DefaultValidationMode
}

// GetServiceTimeout returns the request timeout for a service.
func (c *Config) GetServiceTimeout(serviceID string) time.Duration {
	if svc, ok := c.Services[serviceID]; ok && svc.RequestTimeoutSeconds > 0 {
		return time.Duration(svc.RequestTimeoutSeconds) * time.Second
	}
	if c.DefaultRequestTimeoutSeconds > 0 {
		return time.Duration(c.DefaultRequestTimeoutSeconds) * time.Second
	}
	// Default to 30 seconds if not configured
	return 30 * time.Second
}

// GetServiceMaxBodySize returns the max body size for a service.
func (c *Config) GetServiceMaxBodySize(serviceID string) int64 {
	if svc, ok := c.Services[serviceID]; ok && svc.MaxBodySizeBytes > 0 {
		return svc.MaxBodySizeBytes
	}
	return c.DefaultMaxBodySizeBytes
}

// GetBackend returns the backend configuration for a service and RPC type.
// Returns nil if the service or RPC type is not found.
func (c *Config) GetBackend(serviceID, rpcType string) *BackendConfig {
	if svc, ok := c.Services[serviceID]; ok {
		if backend, ok := svc.Backends[rpcType]; ok {
			return &backend
		}
	}
	return nil
}

// GetBackendURL returns the backend URL for a service and RPC type.
// Returns an empty string if not found.
func (c *Config) GetBackendURL(serviceID, rpcType string) string {
	if backend := c.GetBackend(serviceID, rpcType); backend != nil {
		return backend.URL
	}
	return ""
}

// LoadConfig loads a relayer configuration from a YAML file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Start with defaults
	config := DefaultConfig()

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}
