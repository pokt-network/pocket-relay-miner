package config

// RedisConfig contains Redis connection configuration shared between miner and relayer.
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
	// If not specified, defaults are used (ha:cache, ha:events, ha:relays, etc.)
	Namespace RedisNamespaceConfig `yaml:"namespace,omitempty"`
}

// RedisNamespaceConfig contains Redis key namespace/prefix configuration.
// This centralizes all Redis key prefixes used across the system.
// Components use transport/redis.KeyBuilder to construct keys from this config.
type RedisNamespaceConfig struct {
	// BasePrefix is the root prefix for all Redis keys (default: "ha")
	BasePrefix string `yaml:"base_prefix,omitempty"`

	// CachePrefix is the prefix for cache data (default: "cache")
	// Full key: {BasePrefix}:{CachePrefix}:{entityType}:{key}
	CachePrefix string `yaml:"cache_prefix,omitempty"`

	// EventsPrefix is the prefix for pub/sub events (default: "events")
	// Full key: {BasePrefix}:{EventsPrefix}:{channel}
	EventsPrefix string `yaml:"events_prefix,omitempty"`

	// StreamsPrefix is the prefix for Redis Streams/relay WAL (default: "relays")
	// Full key: {BasePrefix}:{StreamsPrefix}:{supplier}
	StreamsPrefix string `yaml:"streams_prefix,omitempty"`

	// MinerPrefix is the prefix for miner state (default: "miner")
	// Full key: {BasePrefix}:{MinerPrefix}:{resource}
	MinerPrefix string `yaml:"miner_prefix,omitempty"`

	// SupplierPrefix is the prefix for supplier data (default: "supplier")
	// Full key: {BasePrefix}:{SupplierPrefix}:{address}
	SupplierPrefix string `yaml:"supplier_prefix,omitempty"`

	// MeterPrefix is the prefix for metering data (default: "meter")
	// Full key: {BasePrefix}:{MeterPrefix}:{session}
	MeterPrefix string `yaml:"meter_prefix,omitempty"`

	// ParamsPrefix is the prefix for cached params (default: "params")
	// Full key: {BasePrefix}:{ParamsPrefix}:{type}
	ParamsPrefix string `yaml:"params_prefix,omitempty"`

	// ConsumerGroupPrefix is the consumer group name for Redis Streams (default: "miners")
	// Full group name: {BasePrefix}-{ConsumerGroupPrefix}
	// Example: "ha-miners"
	ConsumerGroupPrefix string `yaml:"consumer_group_prefix,omitempty"`
}

// DefaultRedisNamespaceConfig returns the default namespace configuration.
// This matches the hardcoded "ha:" prefix structure used historically.
func DefaultRedisNamespaceConfig() RedisNamespaceConfig {
	return RedisNamespaceConfig{
		BasePrefix:          "ha",
		CachePrefix:         "cache",
		EventsPrefix:        "events",
		StreamsPrefix:       "relays",
		MinerPrefix:         "miner",
		SupplierPrefix:      "supplier",
		MeterPrefix:         "meter",
		ParamsPrefix:        "params",
		ConsumerGroupPrefix: "miners",
	}
}
