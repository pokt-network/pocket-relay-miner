package relayer

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
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

	// Compression configuration for relay bytes compression.
	// Reduces Redis memory usage and network bandwidth.
	Compression CompressionConfig `yaml:"compression,omitempty"`
}

// CompressionConfig contains configuration for relay compression.
// Compression significantly reduces Redis memory usage (2-50x for relay data).
type CompressionConfig struct {
	// Enabled enables compression of relay bytes.
	// Default: true
	Enabled *bool `yaml:"enabled,omitempty"`

	// Level is the compression level to use.
	// Possible values:
	//   - "none"    : No compression (passthrough)
	//   - "fastest" : zstd level 1, ~315 MB/s, lowest CPU, larger output
	//   - "default" : zstd level 3, balanced speed/ratio
	//   - "better"  : zstd level 7, slower, better ratio
	//   - "best"    : zstd level 11, slowest, best ratio (DEFAULT - memory savings prioritized)
	// Default: "best"
	Level string `yaml:"level,omitempty"`

	// MinSize is the minimum size in bytes before compression is applied.
	// Data smaller than this is sent uncompressed (overhead not worth it).
	// Default: 64 bytes
	MinSize int `yaml:"min_size,omitempty"`
}

// ToTransportConfig converts the YAML config to transport.CompressionConfig.
func (c CompressionConfig) ToTransportConfig() transport.CompressionConfig {
	cfg := transport.DefaultCompressionConfig()

	if c.Enabled != nil {
		cfg.Enabled = *c.Enabled
	}
	if c.Level != "" {
		cfg.Level = transport.CompressionLevel(c.Level)
	}
	if c.MinSize > 0 {
		cfg.MinSize = c.MinSize
	}

	return cfg
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
		Compression: CompressionConfig{
			Enabled: boolPtr(true), // Enable compression by default
			Level:   "best",        // zstd level 11 - best ratio (memory savings prioritized)
			MinSize: 64,            // Don't compress tiny data
		},
	}
}

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool {
	return &b
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
