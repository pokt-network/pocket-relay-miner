package config

import (
	"fmt"
	"regexp"
	"strings"
)

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

// RedisNamespaceConfig contains Redis key namespace configuration.
//
// Only the base prefix is configurable. Everything below it is a constant owned
// by transport/redis.KeyBuilder, which is the single authority on how a key is
// built. The sub-prefixes below are retained ONLY to detect a config written
// against the versions that had them, and are ignored when building keys.
type RedisNamespaceConfig struct {
	// BasePrefix is the root prefix for all Redis keys (default: "ha").
	// It is the first segment of every key, so two base prefixes are two
	// disjoint keyspaces -- one Redis, several fleets.
	BasePrefix string `yaml:"base_prefix,omitempty"`

	// The fields below are RETIRED. They were operator knobs until the key
	// layout moved into the KeyBuilder, and they are the reason two families
	// could be made to collide: one family read its segment from config while
	// its twin hardcoded a literal, so supplier_prefix: "suppliers" produced one
	// key with two writers, and cache_prefix: "supplier" made the supplier scan
	// pattern match every cache key -- reachable from a command that deletes
	// what it scans.
	//
	// They are still parsed so Validate can TELL an operator their config no
	// longer does what it says, instead of silently relocating their keyspace on
	// upgrade. Nothing reads them when building keys.
	CachePrefix         string `yaml:"cache_prefix,omitempty"`
	EventsPrefix        string `yaml:"events_prefix,omitempty"`
	StreamsPrefix       string `yaml:"streams_prefix,omitempty"`
	MinerPrefix         string `yaml:"miner_prefix,omitempty"`
	SupplierPrefix      string `yaml:"supplier_prefix,omitempty"`
	MeterPrefix         string `yaml:"meter_prefix,omitempty"`
	ParamsPrefix        string `yaml:"params_prefix,omitempty"`
	ConsumerGroupPrefix string `yaml:"consumer_group_prefix,omitempty"`
}

// DefaultRedisNamespaceConfig returns the default namespace configuration.
func DefaultRedisNamespaceConfig() RedisNamespaceConfig {
	return RedisNamespaceConfig{BasePrefix: "ha"}
}

// basePrefixRejected matches what a base prefix may NOT contain: Redis glob
// metacharacters and whitespace.
//
// The ban is narrow on purpose. A glob character ends up inside every SCAN
// pattern the KeyBuilder produces, so a base of "*" makes a pattern match the
// whole database -- and one of those patterns feeds a delete. Everything else is
// just text: a colon or a dot ("ha:prod", "pocket.ha") only adds segments, and
// those bases work today. Rejecting them would lock out a running deployment
// whose only alternative is renaming the base, which relocates its ENTIRE
// keyspace including the WAL the miner consumes.
var basePrefixRejected = regexp.MustCompile(`[*?\[\]\\s]`)

// retiredNamespaceFields returns the retired sub-prefix fields an operator has
// set, by their YAML name, together with the constant now used instead.
func (ns RedisNamespaceConfig) retiredNamespaceFields() []string {
	var set []string
	for _, f := range []struct{ name, value, now string }{
		{"cache_prefix", ns.CachePrefix, "cache"},
		{"events_prefix", ns.EventsPrefix, "events"},
		{"streams_prefix", ns.StreamsPrefix, "relays"},
		{"miner_prefix", ns.MinerPrefix, "miner"},
		{"supplier_prefix", ns.SupplierPrefix, "supplier"},
		{"meter_prefix", ns.MeterPrefix, "meter"},
		{"params_prefix", ns.ParamsPrefix, "params"},
		{"consumer_group_prefix", ns.ConsumerGroupPrefix, "miners"},
	} {
		if f.value != "" && f.value != f.now {
			set = append(set, fmt.Sprintf("%s: %q (keys now use %q)", f.name, f.value, f.now))
		}
	}
	return set
}

// Validate rejects a namespace whose base prefix is unusable, and a config that
// still customizes a retired sub-prefix.
//
// The retired case is a HARD ERROR, not a warning, and deliberately so: the keys
// that config was writing move the moment this version starts, so a fleet would
// come up healthy against an empty keyspace and leave its sessions, meters and
// WAL behind under the old names. Setting one to the value it already had is
// accepted -- those keys do not move -- so an operator who copied the shipped
// example and uncommented it is not locked out.
func (ns RedisNamespaceConfig) Validate() error {
	if basePrefixRejected.MatchString(ns.BasePrefix) {
		return fmt.Errorf(
			"redis.namespace.base_prefix %q contains a glob metacharacter or whitespace. It is the first "+
				"segment of every key AND of every SCAN pattern, and one of those patterns feeds a delete, "+
				"so a glob here can match keys this fleet does not own. Everything else is allowed, "+
				"including ':' and '.'. If this value is already in use, do NOT simply rename it: that "+
				"relocates the entire keyspace, including the relay WAL the miner consumes. Drain the fleet "+
				"and migrate",
			ns.BasePrefix)
	}

	if retired := ns.retiredNamespaceFields(); len(retired) > 0 {
		return fmt.Errorf(
			"redis.namespace no longer supports per-family prefixes, and yours customizes %d of them: %s. "+
				"The key layout below base_prefix is now fixed in code, so starting with this config would "+
				"relocate that data: the fleet would come up healthy against an empty keyspace and leave its "+
				"sessions, meters and relay WAL behind under the old names. Remove those lines. If keys really "+
				"live under them today, drain the fleet and migrate before upgrading. Do NOT fold the old value "+
				"into base_prefix: that moves the ENTIRE keyspace instead of one family",
			len(retired), strings.Join(retired, "; "))
	}

	return nil
}

// WithDefaults returns the namespace with the base prefix defaulted. There is
// nothing else left to default: every other segment is a constant in
// transport/redis, so a partial namespace cannot produce an empty segment.
func (ns RedisNamespaceConfig) WithDefaults() RedisNamespaceConfig {
	if ns.BasePrefix == "" {
		ns.BasePrefix = DefaultRedisNamespaceConfig().BasePrefix
	}
	return ns
}
