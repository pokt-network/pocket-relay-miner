package redis

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// Client wraps a Redis client with a KeyBuilder for namespace-aware key construction.
// This eliminates hardcoded "ha:" prefixes throughout the codebase.
type Client struct {
	redis.UniversalClient
	keyBuilder *KeyBuilder
	poolSize   int // Configured pool size for validation
}

// KB returns the KeyBuilder for constructing Redis keys with configured namespaces.
// Use this instead of hardcoding key patterns.
//
// Example:
//
//	key := client.KB().CacheKey("application", appAddress)
//	// Returns: "ha:cache:application:pokt1abc..." (based on config)
func (c *Client) KB() *KeyBuilder {
	return c.keyBuilder
}

// PoolSize returns the configured pool size for validation purposes.
// Use this to check if pool size is sufficient for the number of suppliers.
// Formula: poolSize = numSuppliers + 20 overhead
func (c *Client) PoolSize() int {
	return c.poolSize
}

// ClientConfig contains configuration for creating a Redis client.
type ClientConfig struct {
	// URL is the Redis connection URL.
	// Supports: redis://, rediss:// (TLS), redis-sentinel://, redis-cluster://
	URL string

	// MaxRetries is the maximum number of retries before giving up.
	// Default: 3
	MaxRetries int

	// PoolSize is the maximum number of socket connections.
	// Default: 10 connections per CPU
	PoolSize int

	// MinIdleConns is the minimum number of idle connections kept warm.
	// Default: PoolSize / 4 (see the assignment in NewClient).
	MinIdleConns int

	// PoolTimeout is the amount of time to wait for a connection from the pool (seconds).
	// Default: 4 seconds
	// Set to 0 for go-redis default (1 second + ReadTimeout)
	PoolTimeoutSeconds int

	// ConnMaxIdleTime is the maximum amount of time a connection can be idle (seconds).
	// Idle connections older than this are closed.
	// Default: 5 minutes
	// Set to 0 to disable (connections never closed due to idle time)
	ConnMaxIdleTimeSeconds int

	// Namespace configures Redis key prefixes.
	// If not provided, defaults are used (ha:cache, ha:events, etc.)
	Namespace config.RedisNamespaceConfig
}

// NewClient creates a new Redis client with KeyBuilder from the configuration.
// Supports standalone, sentinel, and cluster modes based on URL scheme.
// Returns a wrapped Client that provides both Redis operations and namespace-aware key building.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("redis URL is required")
	}

	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}

	// Set defaults
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	// Pool size must be large enough for blocked + non-blocked operations:
	//
	// BLOCKED CONNECTIONS (held indefinitely):
	// - 1 per supplier: XREADGROUP blocking read (stream consumption)
	// - 1: Block event pub/sub subscriber
	// - 2-3: Cache invalidation pub/sub channels
	// - 1: Supplier registry pub/sub
	// Subtotal: numSuppliers + ~5 for pub/sub
	//
	// NON-BLOCKED OPERATIONS (share pool, fast release):
	// - SMST HSET/HGET/HDEL (tree updates)
	// - ACK batches (message acknowledgment)
	// - Session store reads/writes
	// - Leader heartbeat/lock
	// - Cache reads/writes
	// Subtotal: ~10-15 concurrent operations
	//
	// Formula: poolSize = numSuppliers + 20 overhead
	// - 1 supplier:   1 + 20 = 21 connections
	// - 10 suppliers: 10 + 20 = 30 connections
	// - 100 suppliers: 100 + 20 = 120 connections
	//
	// Default to 50 which handles up to ~30 suppliers.
	// For larger deployments, set pool_size = numSuppliers + 20.
	poolSize := cfg.PoolSize
	if poolSize <= 0 {
		poolSize = 50
	}

	// Keep a quarter of the pool warm. This is the default config/redis.go has
	// always promised and both binaries' validation messages have always
	// described; until 2026-09-12 the value travelled here raw, arrived as 0,
	// and go-redis kept no idle connections at all -- so every burst after a
	// quiet moment paid a TCP dial (~1-5ms) per connection it needed.
	minIdleConns := cfg.MinIdleConns
	if minIdleConns <= 0 {
		minIdleConns = poolSize / 4
	}

	// A pool timeout this repository chose, instead of the one go-redis applies
	// when the field is left at zero. See config.DefaultPoolTimeoutSeconds: the
	// implicit value was 6s, described nowhere and contradicted by two comments.
	poolTimeoutSeconds := cfg.PoolTimeoutSeconds
	if poolTimeoutSeconds <= 0 {
		poolTimeoutSeconds = config.DefaultPoolTimeoutSeconds
	}

	var client redis.UniversalClient

	switch u.Scheme {
	case "redis", "rediss":
		// Standalone Redis
		opts, parseErr := redis.ParseURL(cfg.URL)
		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse redis URL: %w", parseErr)
		}
		opts.MaxRetries = maxRetries
		opts.PoolSize = poolSize
		opts.MinIdleConns = minIdleConns

		// Always set, never left at zero: a zero here is not "no timeout", it is
		// go-redis substituting its own (options.go, ReadTimeout+1s).
		opts.PoolTimeout = time.Duration(poolTimeoutSeconds) * time.Second
		if cfg.ConnMaxIdleTimeSeconds > 0 {
			opts.ConnMaxIdleTime = time.Duration(cfg.ConnMaxIdleTimeSeconds) * time.Second
		}

		client = redis.NewClient(opts)

	case "redis-sentinel":
		// Redis Sentinel
		client, err = newSentinelClient(u, maxRetries, poolSize, minIdleConns, poolTimeoutSeconds, cfg.ConnMaxIdleTimeSeconds)
		if err != nil {
			return nil, err
		}

	case "redis-cluster":
		// Redis Cluster
		client, err = newClusterClient(u, maxRetries, poolSize, minIdleConns, poolTimeoutSeconds, cfg.ConnMaxIdleTimeSeconds)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported redis URL scheme: %s", u.Scheme)
	}

	// Test connection
	if err = client.Ping(ctx).Err(); err != nil {
		// Close the client to prevent resource leak
		if closeErr := client.Close(); closeErr != nil {
			return nil, fmt.Errorf("failed to connect to redis: %w (close error: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	// Create wrapped client with KeyBuilder and pool size for validation.
	// NewKeyBuilder defaults the namespace field by field: a config that
	// sets only base_prefix still gets every sub-prefix defaulted (the
	// previous all-or-nothing check here produced empty segments like
	// "prod::application:x").
	return &Client{
		UniversalClient: client,
		keyBuilder:      NewKeyBuilder(cfg.Namespace),
		poolSize:        poolSize,
	}, nil
}

// EffectivePoolOptions reports the pool size and the pool timeout the RUNNING
// client holds, read from the client rather than from the configuration it was
// built with.
//
// The difference is not academic, and this repo has the scar: PoolTimeoutSeconds
// arrives as 0 whenever nobody sets it, NewClient then leaves opts.PoolTimeout at
// 0, and go-redis substitutes its own default of ReadTimeout+1s -- with
// ReadTimeout itself defaulting to 5s. The deadline every relay actually runs
// against is 6 seconds, while config/redis.go claimed 4 and claimed that 0 waits
// forever. A gauge fed from the config would have published 0 and certified the
// request instead of what runs, which is the same mistake as a GOMAXPROCS
// literal sitting under a comment claiming it matched the CPU limit.
//
// A type switch and not a plain call because redis.UniversalClient does NOT
// expose Options() -- its interface carries PoolStats and no accessor for the
// options. NewClient and NewFailoverClient both return *redis.Client, so
// standalone and sentinel share a branch; cluster has its own options type.
//
// ok is false for a client type this cannot ask. Callers must report that
// rather than publish a zero: a zero pool timeout reads as "no limit", which is
// the opposite of the truth.
func (c *Client) EffectivePoolOptions() (EffectivePool, bool) {
	return EffectivePoolOf(c.UniversalClient)
}

// EffectivePoolOf asks the same of a bare client. The batching publisher holds a
// redis.UniversalClient and not a *Client, and one type switch serving both is
// one that cannot drift from a second copy.
func EffectivePoolOf(client redis.UniversalClient) (EffectivePool, bool) {
	switch cl := client.(type) {
	case *redis.Client:
		o := cl.Options()
		return EffectivePool{PoolSize: o.PoolSize, MinIdleConns: o.MinIdleConns, PoolTimeout: o.PoolTimeout}, true
	case *redis.ClusterClient:
		o := cl.Options()
		return EffectivePool{PoolSize: o.PoolSize, MinIdleConns: o.MinIdleConns, PoolTimeout: o.PoolTimeout}, true
	default:
		return EffectivePool{}, false
	}
}

// EffectivePool is what a running client's connection pool is actually set to.
type EffectivePool struct {
	PoolSize     int
	MinIdleConns int
	PoolTimeout  time.Duration
}

// newSentinelClient creates a Redis Sentinel client.
// URL format: redis-sentinel://[:password@]host1:port1,host2:port2/master_name[?db=N]
func newSentinelClient(u *url.URL, maxRetries, poolSize, minIdleConns, poolTimeoutSeconds, connMaxIdleTimeSeconds int) (redis.UniversalClient, error) {
	// Parse master name from path
	masterName := strings.TrimPrefix(u.Path, "/")
	if masterName == "" {
		return nil, fmt.Errorf("sentinel URL must include master name in path")
	}

	// Parse sentinel addresses
	addrs := strings.Split(u.Host, ",")
	if len(addrs) == 0 {
		return nil, fmt.Errorf("sentinel URL must include at least one sentinel address")
	}

	// Parse password
	password := ""
	if u.User != nil {
		password, _ = u.User.Password()
	}

	// Parse DB number
	db := 0
	if dbStr := u.Query().Get("db"); dbStr != "" {
		var err error
		db, err = strconv.Atoi(dbStr)
		if err != nil {
			return nil, fmt.Errorf("invalid db number: %w", err)
		}
	}

	opts := &redis.FailoverOptions{
		MasterName:    masterName,
		SentinelAddrs: addrs,
		Password:      password,
		DB:            db,
		MaxRetries:    maxRetries,
		PoolSize:      poolSize,
		MinIdleConns:  minIdleConns,
	}

	// Apply timeout settings
	if poolTimeoutSeconds > 0 {
		opts.PoolTimeout = time.Duration(poolTimeoutSeconds) * time.Second
	}
	if connMaxIdleTimeSeconds > 0 {
		opts.ConnMaxIdleTime = time.Duration(connMaxIdleTimeSeconds) * time.Second
	}

	return redis.NewFailoverClient(opts), nil
}

// newClusterClient creates a Redis Cluster client.
// URL format: redis-cluster://[:password@]host1:port1,host2:port2[?db=N]
func newClusterClient(u *url.URL, maxRetries, poolSize, minIdleConns, poolTimeoutSeconds, connMaxIdleTimeSeconds int) (redis.UniversalClient, error) {
	// Parse cluster addresses
	addrs := strings.Split(u.Host, ",")
	if len(addrs) == 0 {
		return nil, fmt.Errorf("cluster URL must include at least one node address")
	}

	// Parse password
	password := ""
	if u.User != nil {
		password, _ = u.User.Password()
	}

	opts := &redis.ClusterOptions{
		Addrs:        addrs,
		Password:     password,
		MaxRetries:   maxRetries,
		PoolSize:     poolSize,
		MinIdleConns: minIdleConns,
	}

	// Apply timeout settings
	if poolTimeoutSeconds > 0 {
		opts.PoolTimeout = time.Duration(poolTimeoutSeconds) * time.Second
	}
	if connMaxIdleTimeSeconds > 0 {
		opts.ConnMaxIdleTime = time.Duration(connMaxIdleTimeSeconds) * time.Second
	}

	return redis.NewClusterClient(opts), nil
}
