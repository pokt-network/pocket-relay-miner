package relayer

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/pool"
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

// TimeoutProfile defines a complete set of timeout settings for a service.
// Multiple profiles can be defined to support different service types (fast RPCs vs streaming).
type TimeoutProfile struct {
	// Name is the profile name (e.g., "fast", "streaming")
	Name string `yaml:"name,omitempty"`

	// RequestTimeoutSeconds is the overall timeout for backend requests.
	// This is the total time allowed for the request/response cycle.
	// Default: 30 seconds
	RequestTimeoutSeconds int64 `yaml:"request_timeout_seconds"`

	// ResponseHeaderTimeoutSeconds is the timeout for receiving response headers.
	// Set to 0 for no timeout (useful for streaming responses).
	// Default: inherits from HTTPTransportConfig if 0
	ResponseHeaderTimeoutSeconds int64 `yaml:"response_header_timeout_seconds"`

	// DialTimeoutSeconds is the timeout for establishing a new connection.
	// Default: inherits from HTTPTransportConfig if 0
	DialTimeoutSeconds int64 `yaml:"dial_timeout_seconds"`

	// TLSHandshakeTimeoutSeconds is the timeout for completing the TLS handshake.
	// Default: inherits from HTTPTransportConfig if 0
	TLSHandshakeTimeoutSeconds int64 `yaml:"tls_handshake_timeout_seconds"`
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

	// Pprof configuration for profiling
	Pprof config.PprofConfig `yaml:"pprof,omitempty"`

	// HealthCheck configuration for the relayer itself
	HealthCheck HealthCheckConfig `yaml:"health_check"`

	// CacheWarmup configuration for pre-warming caches at startup.
	CacheWarmup CacheWarmupConfig `yaml:"cache_warmup,omitempty"`

	// Logging configuration
	Logging logging.Config `yaml:"logging,omitempty"`

	// RelayMeter configuration for rate limiting based on app stakes
	RelayMeter RelayMeterYAMLConfig `yaml:"relay_meter,omitempty"`

	// HTTPTransport configuration for backend HTTP client connection pooling.
	HTTPTransport HTTPTransportConfig `yaml:"http_transport,omitempty"`

	// TimeoutProfiles defines HTTP client timeout profiles.
	// Auto-populated with "fast" and "streaming" defaults if not specified.
	TimeoutProfiles map[string]TimeoutProfile `yaml:"timeout_profiles,omitempty"`

	// pools is the registry of backend pools, keyed by "serviceID:rpcType".
	// Built by BuildPools() after validation, not serialized to YAML.
	pools map[string]*pool.Pool `yaml:"-"`
}

// HTTPTransportConfig contains HTTP transport settings for backend connections.
// These settings optimize connection reuse, reduce latency, and prevent resource exhaustion.
// Defaults are tuned for 1000+ RPS with connection pooling.
type HTTPTransportConfig struct {
	// MaxIdleConns controls the maximum number of idle (keep-alive) connections across all hosts.
	// Default: 500 (5x increase: supports multiple backends and services)
	MaxIdleConns int `yaml:"max_idle_conns"`

	// MaxIdleConnsPerHost controls the maximum idle (keep-alive) connections to keep per-host.
	// Default: 100 (5x increase: keeps connections warm after traffic bursts)
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"`

	// MaxConnsPerHost limits the total number of connections per host (including active and idle).
	// Default: 500 (5x increase: handles p99 latency spikes and slow backends)
	// Set to 0 for unlimited.
	MaxConnsPerHost int `yaml:"max_conns_per_host"`

	// IdleConnTimeoutSeconds is how long idle connections are kept alive.
	// Default: 90 (seconds)
	IdleConnTimeoutSeconds int64 `yaml:"idle_conn_timeout_seconds"`

	// DialTimeoutSeconds is the timeout for establishing a new connection.
	// Default: 5 (seconds)
	DialTimeoutSeconds int64 `yaml:"dial_timeout_seconds"`

	// TLSHandshakeTimeoutSeconds is the timeout for completing the TLS handshake.
	// Default: 10 (seconds)
	TLSHandshakeTimeoutSeconds int64 `yaml:"tls_handshake_timeout_seconds"`

	// ResponseHeaderTimeoutSeconds is the timeout for receiving response headers after sending the request.
	// This prevents hanging on slow backends that never send headers.
	// Default: 30 (seconds)
	ResponseHeaderTimeoutSeconds int64 `yaml:"response_header_timeout_seconds"`

	// ExpectContinueTimeoutSeconds is the timeout for receiving server's first response headers
	// after fully writing the request headers if the request has "Expect: 100-continue".
	// Default: 1 (second) - zero means no timeout
	ExpectContinueTimeoutSeconds int64 `yaml:"expect_continue_timeout_seconds"`

	// TCPKeepAliveSeconds is the keep-alive period for active network connections.
	// Default: 30 (seconds) - 0 disables keep-alive
	TCPKeepAliveSeconds int64 `yaml:"tcp_keep_alive_seconds"`

	// DisableCompression disables automatic gzip compression for requests and responses.
	// Default: true (don't modify content encoding for relay protocol)
	DisableCompression bool `yaml:"disable_compression"`
}

// RedisConfig contains Redis connection configuration.
type RedisConfig struct {
	// URL is the Redis connection URL.
	// Supports: redis://, rediss://, redis-sentinel://, redis-cluster://
	URL string `yaml:"url"`

	// PoolSize is the maximum number of socket connections.
	// Default: 20 × runtime.GOMAXPROCS (2x go-redis default for production)
	// Set to 0 to use go-redis default (10 × GOMAXPROCS)
	PoolSize int `yaml:"pool_size,omitempty"`

	// MinIdleConns is the minimum number of idle connections to maintain.
	// Keeping idle connections warm eliminates connection dial latency (~1-5ms).
	// Default: PoolSize / 4
	// Set to 0 to disable (connections created on demand)
	MinIdleConns int `yaml:"min_idle_conns,omitempty"`

	// PoolTimeout is the amount of time to wait for a connection from the pool.
	// Default: 4 seconds
	// Set to 0 to wait indefinitely
	PoolTimeoutSeconds int `yaml:"pool_timeout_seconds,omitempty"`

	// ConnMaxIdleTime is the maximum amount of time a connection can be idle.
	// Idle connections older than this are closed.
	// Default: 5 minutes
	// Set to 0 to disable (connections never closed due to idle time)
	ConnMaxIdleTimeSeconds int `yaml:"conn_max_idle_time_seconds,omitempty"`

	// Namespace configures Redis key prefixes for all data types.
	// All components (miner, relayer, cache) read from this config to build keys.
	// Must match miner configuration for proper operation.
	// If not specified, defaults are used (ha:cache, ha:events, ha:relays, etc.)
	Namespace config.RedisNamespaceConfig `yaml:"namespace,omitempty"`
}

// PocketNodeConfig contains Pocket blockchain connection configuration.
type PocketNodeConfig struct {
	// QueryNodeRPCUrl is the URL for RPC queries (HTTP endpoint).
	// Used for health checks and fallback queries.
	QueryNodeRPCUrl string `yaml:"query_node_rpc_url"`

	// QueryNodeGRPCUrl is the URL for gRPC queries.
	// Primary interface for chain queries (application, session, service, etc.)
	QueryNodeGRPCUrl string `yaml:"query_node_grpc_url"`

	// GRPCInsecure disables TLS for gRPC connections.
	// Default: false (TLS enabled for production)
	// Set to true for localnet/development without TLS.
	GRPCInsecure bool `yaml:"grpc_insecure,omitempty"`
}

// ServiceConfig contains configuration for a single service.
// The service ID is the map key in Config.Services.
// All backends must be specified per RPC type in backends map.
// ComputeUnitsPerRelay is fetched from the on-chain service entity.
type ServiceConfig struct {
	// ValidationMode overrides the default validation mode for this service.
	ValidationMode ValidationMode `yaml:"validation_mode,omitempty"`

	// TimeoutProfile is the name of the timeout profile to use for this service.
	// The profile defines request_timeout_seconds and HTTP client timeouts.
	// Must match a profile name in Config.TimeoutProfiles.
	// If not specified, uses the "fast" profile.
	TimeoutProfile string `yaml:"timeout_profile,omitempty"`

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
// Supports both single-URL (url field) and multi-URL (urls field) modes.
// The url and urls fields are mutually exclusive.
type BackendConfig struct {
	// URL is the single backend URL for this RPC type (backward compatible).
	// Supports http://, https://, ws://, wss://, grpc://, grpcs://
	// Mutually exclusive with URLs.
	URL string `yaml:"url,omitempty"`

	// URLs is a list of backend endpoints for this RPC type.
	// Supports mixed entries: plain strings and objects with optional name.
	// Mutually exclusive with URL.
	URLs []BackendEndpointConfig `yaml:"urls,omitempty"`

	// BasePath is an optional path prefix that the backend expects on every
	// request (e.g. "/ext/bc/C/rpc" for AvalancheGo). When set, the proxy
	// prepends it to client requests that do not already start with it, so
	// that callers which already include the prefix (or which send "/") do
	// not produce a duplicated or broken path. Takes precedence over any
	// path component present in URL/URLs.
	BasePath string `yaml:"base_path,omitempty"`

	// LoadBalancing strategy for this backend type.
	// Defined in Phase 1, used starting Phase 2.
	// Valid values: "round_robin" (default in Phase 2), others added in Phase 10.
	LoadBalancing string `yaml:"load_balancing,omitempty"`

	// Headers are additional headers shared across all backends in this pool.
	Headers map[string]string `yaml:"headers,omitempty"`

	// Authentication shared across all backends in this pool.
	Authentication *AuthenticationConfig `yaml:"authentication,omitempty"`

	// HealthCheck configuration shared across all backends in this pool.
	HealthCheck *BackendHealthCheckConfig `yaml:"health_check,omitempty"`

	// MaxRetries is the maximum number of retry attempts when a backend request fails.
	// Uses a pointer to distinguish "not set" (default 1) from "explicitly 0" (disabled).
	// Valid range: 0-3.
	MaxRetries *int `yaml:"max_retries,omitempty"`

	// RecoveryTimeoutSeconds is the duration (in seconds) after which an unhealthy
	// endpoint auto-recovers. Prevents the circuit breaker death spiral when no
	// active health checks are configured. Default: 30 seconds. Set to 0 to disable.
	RecoveryTimeoutSeconds *int `yaml:"recovery_timeout_seconds,omitempty"`
}

// BackendEndpointConfig represents a single endpoint in a backend pool.
// Supports both plain string URLs and objects with name + url.
type BackendEndpointConfig struct {
	// Name is an optional display name for this endpoint (used in logs and metrics).
	Name string `yaml:"name,omitempty"`

	// URL is the backend endpoint URL.
	URL string `yaml:"url"`
}

// UnmarshalYAML implements custom unmarshaling to handle mixed YAML entries:
//   - Plain string: "http://node1:8545"
//   - Object: {name: "primary", url: "http://node1:8545"}
func (c *BackendEndpointConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		url := strings.TrimSpace(value.Value)
		if url == "" {
			return fmt.Errorf("backend endpoint URL must not be empty")
		}
		c.URL = value.Value
		return nil
	}
	// Object form: decode as struct
	type plain BackendEndpointConfig
	if err := value.Decode((*plain)(c)); err != nil {
		return err
	}
	if strings.TrimSpace(c.URL) == "" {
		return fmt.Errorf("backend endpoint URL must not be empty")
	}
	return nil
}

// AuthenticationConfig contains authentication configuration for a backend.
type AuthenticationConfig struct {
	// Username for basic auth.
	Username string `yaml:"username,omitempty"`

	// Password for basic auth.
	Password string `yaml:"password,omitempty"`

	// BearerToken for bearer token auth (sent as "Authorization: Bearer <token>").
	BearerToken string `yaml:"bearer_token,omitempty"`

	// PlainToken for plain token auth (sent as "Authorization: <token>" without "Bearer " prefix).
	PlainToken string `yaml:"plain_token,omitempty"`
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

	// Method is the HTTP method for health check probes (default: GET).
	// Use POST for JSON-RPC backends that require a request body.
	Method string `yaml:"method,omitempty"`

	// RequestBody is the request body for health check probes (e.g., JSON-RPC request).
	// When set, the probe sends this as the HTTP body.
	RequestBody string `yaml:"request_body,omitempty"`

	// ContentType is an explicit Content-Type override for probe requests.
	// If not set and RequestBody starts with '{' or '[', defaults to "application/json".
	ContentType string `yaml:"content_type,omitempty"`

	// ExpectedBody is a substring that must be present in the response body.
	// If not set, response body is not validated (only status code).
	ExpectedBody string `yaml:"expected_body,omitempty"`

	// ExpectedStatus is a list of acceptable HTTP status codes.
	// If not set, any 2xx status code (200-299) is considered healthy.
	ExpectedStatus []int `yaml:"expected_status,omitempty"`
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

	// RedisKeyPrefix is the prefix for Redis keys used by the relay meter.
	// Default: "ha"
	RedisKeyPrefix string `yaml:"redis_key_prefix"`

	// FailBehavior determines behavior when Redis is unavailable.
	// "open" - Allow relays when Redis down (prioritize availability)
	// "closed" - Reject relays when Redis down (prioritize safety)
	// Default: "open"
	FailBehavior string `yaml:"fail_behavior"`

	// CacheTTL is the TTL for all cached Redis data (streams, params, app stakes, meters).
	// Redis TTL handles automatic expiration - no cleanup goroutines needed.
	// Default: 2h (covers ~15 session lifecycles at 30s blocks)
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// CacheWarmupConfig contains configuration for cache pre-warming at startup.
// This helps reduce latency for the first requests by pre-loading application data.
type CacheWarmupConfig struct {
	// Enabled enables cache warmup at startup.
	// Default: true (speeds up first requests by pre-loading application data)
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
			URL: "redis://localhost:6379",
		},
		DefaultValidationMode:        ValidationModeOptimistic,
		DefaultRequestTimeoutSeconds: 30,
		DefaultMaxBodySizeBytes:      10 * 1024 * 1024, // 10MB
		Metrics: MetricsConfig{
			Enabled: true,
			Addr:    "0.0.0.0:9090",
		},
		Pprof: config.PprofConfig{
			Enabled: true, // Enable by default for debugging
			Addr:    "0.0.0.0:6060",
		},
		HealthCheck: HealthCheckConfig{
			Enabled: true,
			Addr:    "0.0.0.0:8081",
		},
		RelayMeter: RelayMeterYAMLConfig{
			Enabled:        true,
			RedisKeyPrefix: "ha",
			FailBehavior:   "open",
			CacheTTL:       2 * time.Hour, // Covers ~15 session lifecycles at 30s blocks
		},
		HTTPTransport: HTTPTransportConfig{
			MaxIdleConns:                 500,  // Total idle connections across all hosts (5x for 1000+ RPS)
			MaxIdleConnsPerHost:          100,  // Idle connections per backend host (5x - keeps warm after bursts)
			MaxConnsPerHost:              500,  // Total connections per host (5x - handles p99 spikes)
			IdleConnTimeoutSeconds:       90,   // Keep idle connections for 90s
			DialTimeoutSeconds:           5,    // 5s to establish connection
			TLSHandshakeTimeoutSeconds:   10,   // 10s for TLS handshake
			ResponseHeaderTimeoutSeconds: 30,   // 30s to receive headers
			ExpectContinueTimeoutSeconds: 1,    // 1s for 100-continue
			TCPKeepAliveSeconds:          30,   // TCP keepalive every 30s
			DisableCompression:           true, // Don't modify content encoding
		},
		TimeoutProfiles: map[string]TimeoutProfile{
			"fast": {
				Name:                         "fast",
				RequestTimeoutSeconds:        30, // Standard RPC services
				ResponseHeaderTimeoutSeconds: 30,
				DialTimeoutSeconds:           5,
				TLSHandshakeTimeoutSeconds:   10,
			},
			"streaming": {
				Name:                         "streaming",
				RequestTimeoutSeconds:        600, // 10 minutes for LLM streaming
				ResponseHeaderTimeoutSeconds: 0,   // No header timeout for streaming
				DialTimeoutSeconds:           10,  // Allow more time for connection
				TLSHandshakeTimeoutSeconds:   15,  // Allow more time for TLS
			},
		},
		CacheWarmup: CacheWarmupConfig{
			Enabled:               true, // Enable by default for faster first requests
			PersistDiscoveredApps: true,
			WarmupConcurrency:     10,
			WarmupTimeoutSeconds:  5,
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

	// Validate Redis pool settings (all are optional, 0 = use defaults)
	if c.Redis.PoolSize < 0 {
		return fmt.Errorf("redis.pool_size must be >= 0 (0 = use default)")
	}
	if c.Redis.MinIdleConns < 0 {
		return fmt.Errorf("redis.min_idle_conns must be >= 0 (0 = use default)")
	}
	if c.Redis.PoolTimeoutSeconds < 0 {
		return fmt.Errorf("redis.pool_timeout_seconds must be >= 0 (0 = use default)")
	}
	if c.Redis.ConnMaxIdleTimeSeconds < 0 {
		return fmt.Errorf("redis.conn_max_idle_time_seconds must be >= 0 (0 = use default)")
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

	// Validate and auto-populate timeout profiles
	if err := c.ValidateTimeoutProfiles(); err != nil {
		return err
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
		hasURL := backend.URL != ""
		hasURLs := len(backend.URLs) > 0

		// Mutual exclusivity: url and urls cannot both be set
		if hasURL && hasURLs {
			return fmt.Errorf("service[%s].backends[%s]: url and urls are mutually exclusive; use one or the other", id, rpcType)
		}

		// At least one must be set
		if !hasURL && !hasURLs {
			return fmt.Errorf("service[%s].backends[%s]: at least one of url or urls is required", id, rpcType)
		}

		// Validate single URL mode
		if hasURL {
			if _, err := url.Parse(backend.URL); err != nil {
				return fmt.Errorf("service[%s].backends[%s].url is invalid: %w", id, rpcType, err)
			}
		}

		// Validate multi-URL mode
		if hasURLs {
			if err := validateBackendEndpoints(id, rpcType, backend.URLs); err != nil {
				return err
			}
		}

		// Validate max_retries if present (must be 0-3)
		if backend.MaxRetries != nil {
			if *backend.MaxRetries < 0 || *backend.MaxRetries > 3 {
				return fmt.Errorf("service[%s].backends[%s].max_retries must be 0-3, got %d", id, rpcType, *backend.MaxRetries)
			}
		}

		// Validate health check config if present.
		// endpoint is optional when base_path is set — the probe path defaults
		// to base_path in that case, which is exactly where the backend lives.
		if backend.HealthCheck != nil && backend.HealthCheck.Enabled {
			if backend.HealthCheck.Endpoint == "" && backend.BasePath == "" {
				return fmt.Errorf("service[%s].backends[%s].health_check.endpoint is required when enabled (or set base_path on the backend)", id, rpcType)
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
// Uses the timeout profile's request_timeout_seconds, falling back to "fast" profile.
func (c *Config) GetServiceTimeout(serviceID string) time.Duration {
	// Get timeout profile for service
	profile := c.GetServiceTimeoutProfile(serviceID)
	if profile != nil && profile.RequestTimeoutSeconds > 0 {
		return time.Duration(profile.RequestTimeoutSeconds) * time.Second
	}
	// Fallback to default request timeout
	if c.DefaultRequestTimeoutSeconds > 0 {
		return time.Duration(c.DefaultRequestTimeoutSeconds) * time.Second
	}
	// Default to 30 seconds if not configured
	return 30 * time.Second
}

// GetServiceTimeoutProfile returns the timeout profile for a service.
// Falls back to "fast" profile if service doesn't specify one.
func (c *Config) GetServiceTimeoutProfile(serviceID string) *TimeoutProfile {
	profileName := "fast" // default
	if svc, ok := c.Services[serviceID]; ok && svc.TimeoutProfile != "" {
		profileName = svc.TimeoutProfile
	}
	if profile, ok := c.TimeoutProfiles[profileName]; ok {
		return &profile
	}
	// Fallback to fast if specified profile doesn't exist
	if profile, ok := c.TimeoutProfiles["fast"]; ok {
		return &profile
	}
	return nil
}

// GetServiceMaxBodySize returns the max body size for a service.
func (c *Config) GetServiceMaxBodySize(serviceID string) int64 {
	if svc, ok := c.Services[serviceID]; ok && svc.MaxBodySizeBytes > 0 {
		return svc.MaxBodySizeBytes
	}
	return c.DefaultMaxBodySizeBytes
}

// getMaxServiceTimeout returns the maximum timeout across all services.
// Used to set HTTP server timeouts that accommodate the longest-running service.
func (c *Config) getMaxServiceTimeout() time.Duration {
	max := time.Duration(c.DefaultRequestTimeoutSeconds) * time.Second

	// Check all services for their timeout profile
	for svcID := range c.Services {
		svcTimeout := c.GetServiceTimeout(svcID)
		if svcTimeout > max {
			max = svcTimeout
		}
	}

	return max
}

// normalizeTimeoutProfile fills in missing timeout values from HTTPTransportConfig.
// Note: ResponseHeaderTimeoutSeconds can legitimately be 0 (no timeout for streaming),
// so it is not auto-populated.
func normalizeTimeoutProfile(profile *TimeoutProfile, transportConfig *HTTPTransportConfig) {
	if profile.DialTimeoutSeconds == 0 {
		profile.DialTimeoutSeconds = transportConfig.DialTimeoutSeconds
	}
	if profile.TLSHandshakeTimeoutSeconds == 0 {
		profile.TLSHandshakeTimeoutSeconds = transportConfig.TLSHandshakeTimeoutSeconds
	}
}

// ValidateTimeoutProfiles validates and auto-populates timeout profiles.
func (c *Config) ValidateTimeoutProfiles() error {
	// Auto-populate missing timeout_profiles with defaults
	if len(c.TimeoutProfiles) == 0 {
		c.TimeoutProfiles = map[string]TimeoutProfile{
			"fast": {
				Name:                         "fast",
				RequestTimeoutSeconds:        30, // Standard RPC services
				ResponseHeaderTimeoutSeconds: 30,
				DialTimeoutSeconds:           5,
				TLSHandshakeTimeoutSeconds:   10,
			},
			"streaming": {
				Name:                         "streaming",
				RequestTimeoutSeconds:        600, // 10 minutes for LLM streaming
				ResponseHeaderTimeoutSeconds: 0,   // No header timeout for streaming
				DialTimeoutSeconds:           10,
				TLSHandshakeTimeoutSeconds:   15,
			},
		}
	}

	// Ensure required profiles exist (either from config or auto-populated)
	if _, ok := c.TimeoutProfiles["fast"]; !ok {
		return fmt.Errorf("required timeout profile 'fast' not defined")
	}
	if _, ok := c.TimeoutProfiles["streaming"]; !ok {
		return fmt.Errorf("required timeout profile 'streaming' not defined")
	}

	// Normalize all profiles (fill in missing timeout values from HTTPTransportConfig)
	for name, profile := range c.TimeoutProfiles {
		normalizeTimeoutProfile(&profile, &c.HTTPTransport)
		c.TimeoutProfiles[name] = profile
	}

	// Validate all service timeout_profile references are valid
	for svcID, svc := range c.Services {
		if svc.TimeoutProfile != "" {
			if _, ok := c.TimeoutProfiles[svc.TimeoutProfile]; !ok {
				return fmt.Errorf("service %s references undefined timeout profile %s",
					svcID, svc.TimeoutProfile)
			}
		}
	}

	return nil
}

// GetBackendConfig returns the BackendConfig for a service and RPC type,
// using the same fallback chain as GetPool (exact -> default_backend -> jsonrpc -> rest -> any).
// Returns nil if no backend config is found after all fallbacks.
// Use this to access pool-level shared config (headers, auth) alongside GetPool().
func (c *Config) GetBackendConfig(serviceID, rpcType string) *BackendConfig {
	svc, svcExists := c.Services[serviceID]
	if !svcExists {
		return nil
	}

	// Direct lookup
	if backend, ok := svc.Backends[rpcType]; ok {
		return &backend
	}

	// Fallback: default_backend
	if svc.DefaultBackend != "" {
		if backend, ok := svc.Backends[svc.DefaultBackend]; ok {
			return &backend
		}
	}

	// Fallback: jsonrpc
	if backend, ok := svc.Backends[BackendTypeJSONRPC]; ok {
		return &backend
	}

	// Fallback: rest
	if backend, ok := svc.Backends[BackendTypeREST]; ok {
		return &backend
	}

	// Fallback: any available
	for _, backend := range svc.Backends {
		return &backend
	}

	return nil
}

// BuildPools creates backend pools from the service configuration.
// Must be called after Validate(). Each service+backend pair gets a Pool
// keyed as "serviceID:rpcType".
func (c *Config) BuildPools() error {
	c.pools = make(map[string]*pool.Pool)

	for serviceID, svc := range c.Services {
		for rpcType, backend := range svc.Backends {
			endpoints, err := buildEndpoints(backend)
			if err != nil {
				return fmt.Errorf("service[%s].backends[%s]: %w", serviceID, rpcType, err)
			}

			poolName := serviceID + ":" + rpcType

			// Select load balancing strategy
			var selector pool.Selector
			var strategyLabel string
			switch backend.LoadBalancing {
			case "round_robin":
				selector = &pool.RoundRobinSelector{}
				strategyLabel = "round_robin(explicit)"
			case "first_healthy":
				selector = &pool.FirstHealthySelector{}
				strategyLabel = "first_healthy(explicit)"
			case "":
				// Auto-detect: round_robin for 2+ endpoints, first_healthy for 1
				if len(endpoints) > 1 {
					selector = &pool.RoundRobinSelector{}
					strategyLabel = "round_robin(auto)"
				} else {
					selector = &pool.FirstHealthySelector{}
					strategyLabel = "first_healthy(auto)"
				}
			default:
				return fmt.Errorf("service[%s].backends[%s]: unknown load_balancing strategy: %q (valid: round_robin, first_healthy)", serviceID, rpcType, backend.LoadBalancing)
			}

			// Set recovery timeout on endpoints to prevent circuit breaker death spiral.
			// If operator sets recovery_timeout_seconds, use it. Otherwise use default (30s).
			// Set to 0 explicitly to disable.
			recoveryTimeout := pool.DefaultRecoveryTimeout
			if backend.RecoveryTimeoutSeconds != nil {
				recoveryTimeout = time.Duration(*backend.RecoveryTimeoutSeconds) * time.Second
			}
			if recoveryTimeout > 0 {
				for _, ep := range endpoints {
					ep.SetRecoveryTimeout(recoveryTimeout)
				}
			}

			c.pools[poolName] = pool.NewPool(poolName, endpoints, selector, strategyLabel)
		}
	}

	return nil
}

// GetPool returns the pool for a service and RPC type, with fallback chain.
// Fallback order: exact match -> default_backend -> jsonrpc -> rest -> any available.
// Returns nil if no pool is found after all fallbacks.
func (c *Config) GetPool(serviceID, rpcType string) *pool.Pool {
	if c.pools == nil {
		return nil
	}

	// Direct lookup
	key := serviceID + ":" + rpcType
	if p, ok := c.pools[key]; ok {
		return p
	}

	// Fallback: default_backend
	svc, svcExists := c.Services[serviceID]
	if !svcExists {
		return nil
	}

	if svc.DefaultBackend != "" {
		if p, ok := c.pools[serviceID+":"+svc.DefaultBackend]; ok {
			return p
		}
	}

	// Fallback: jsonrpc
	if p, ok := c.pools[serviceID+":"+BackendTypeJSONRPC]; ok {
		return p
	}

	// Fallback: rest
	if p, ok := c.pools[serviceID+":"+BackendTypeREST]; ok {
		return p
	}

	// Fallback: any available
	for backendType := range svc.Backends {
		if p, ok := c.pools[serviceID+":"+backendType]; ok {
			return p
		}
	}

	return nil
}

// buildEndpoints converts a BackendConfig into a slice of pool.BackendEndpoint.
// Handles both single-URL (url field) and multi-URL (urls field) modes.
func buildEndpoints(backend BackendConfig) ([]*pool.BackendEndpoint, error) {
	if backend.URL != "" {
		// Single-URL mode: create 1-endpoint pool
		ep, err := pool.NewBackendEndpoint("", backend.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid url: %w", err)
		}
		return []*pool.BackendEndpoint{ep}, nil
	}

	// Multi-URL mode
	endpoints := make([]*pool.BackendEndpoint, 0, len(backend.URLs))
	for _, epCfg := range backend.URLs {
		ep, err := pool.NewBackendEndpoint(epCfg.Name, epCfg.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid endpoint URL %q: %w", epCfg.URL, err)
		}
		endpoints = append(endpoints, ep)
	}
	return endpoints, nil
}

// validateBackendEndpoints validates the URLs list for uniqueness and correctness.
func validateBackendEndpoints(serviceID, rpcType string, endpoints []BackendEndpointConfig) error {
	seenURLs := make(map[string]bool, len(endpoints))
	seenNames := make(map[string]bool, len(endpoints))

	for i, ep := range endpoints {
		if strings.TrimSpace(ep.URL) == "" {
			return fmt.Errorf("service[%s].backends[%s].urls[%d]: URL must not be empty", serviceID, rpcType, i)
		}

		parsed, err := url.Parse(ep.URL)
		if err != nil {
			return fmt.Errorf("service[%s].backends[%s].urls[%d]: invalid URL %q: %w", serviceID, rpcType, i, ep.URL, err)
		}

		// Normalize URL for duplicate detection: host + path (trim trailing slash)
		normalized := parsed.Host + strings.TrimRight(parsed.Path, "/")
		if seenURLs[normalized] {
			return fmt.Errorf("service[%s].backends[%s]: duplicate URL detected: %s", serviceID, rpcType, ep.URL)
		}
		seenURLs[normalized] = true

		// Check name uniqueness (only when name is explicitly set)
		if ep.Name != "" {
			if seenNames[ep.Name] {
				return fmt.Errorf("service[%s].backends[%s]: duplicate name detected: %s", serviceID, rpcType, ep.Name)
			}
			seenNames[ep.Name] = true
		}
	}

	return nil
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

	if err := config.BuildPools(); err != nil {
		return nil, fmt.Errorf("failed to build backend pools: %w", err)
	}

	return &config, nil
}
