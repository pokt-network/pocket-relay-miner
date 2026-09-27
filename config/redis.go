package config

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultPoolTimeoutSeconds is how long a caller waits for a pooled connection
// when nothing sets pool_timeout_seconds.
//
// It reproduces EXACTLY the value go-redis v9.22.0 was silently applying
// (ReadTimeout + 1s, with ReadTimeout defaulting to 5s), so making it explicit
// changed no behaviour on the day it landed. That is the only reason it is 6:
// nobody has chosen it against evidence yet, and choosing a better one needs the
// per-command latency this commit starts collecting. A shorter deadline fails
// faster but spends the client's retry budget sooner -- go-redis retries a pool
// timeout on its own, so the real worst case is MaxRetries+1 times this value.
const DefaultPoolTimeoutSeconds = 6

// RedisConfig contains Redis connection configuration shared between miner and relayer.
type RedisConfig struct {
	// URL is the Redis connection URL.
	// Supports: redis://, rediss://, redis-sentinel://, redis-cluster://
	URL string `yaml:"url"`

	// PoolSize is the maximum number of socket connections.
	// Default (0): 50 in the miner (transport/redis.NewClient); the relayer
	// sizes its own from its workers (WorkerSizing.RedisPoolSize).
	PoolSize int `yaml:"pool_size,omitempty"`

	// MinIdleConns is the minimum number of idle connections to maintain.
	// Keeping idle connections warm eliminates connection dial latency (~1-5ms).
	// Unset (0) means PoolSize / 4, which is what both binaries' validation
	// messages have always said ("0 = use default") and what this field did NOT
	// do until 2026-09-12: the value was passed through raw, arrived as 0, and
	// every burst paid the dial. There is no "disable" any more -- the previous
	// comment claimed 0 disabled pre-warming, which contradicted the validation
	// beside it and described the bug rather than the intent.
	MinIdleConns int `yaml:"min_idle_conns,omitempty"`

	// PoolTimeout is how long a caller waits for a connection before failing.
	//
	// Unset (0) means DefaultPoolTimeoutSeconds. The previous comment here said
	// "Default: 4 seconds" and "Set to 0 to wait indefinitely", and BOTH were
	// false against go-redis v9.22.0: leaving this at 0 made go-redis substitute
	// ReadTimeout+1s, and ReadTimeout itself defaults to 5s, so every relay ran
	// against a 6-second deadline that nobody in this repository had chosen and
	// no comment described. It is ours now, and the effective value is published
	// as ha_transport_redis_pool_timeout_seconds_effective, read from the client.
	PoolTimeoutSeconds int `yaml:"pool_timeout_seconds,omitempty"`

	// ConnMaxIdleTime is the maximum amount of time a connection can be idle.
	// Idle connections older than this are closed.
	// Default (0): the go-redis default, 30 minutes.
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

// basePrefixRejected is what a base prefix must not contain: a colon,
// whitespace or a glob metacharacter (* ? [ ]), so the prefix is ONE flat
// segment. Anything else is accepted.
//
// Why the colon too, and not just the globs. A glob is the loud hazard -- it
// reaches every SCAN pattern the KeyBuilder builds, and one of those feeds a
// delete -- but ':' is the quiet one. The base prefix is a NAMESPACE, one
// segment, and a colon turns it into a hierarchy the key layout does not model:
//
//   - a trailing colon yields the empty segment this package promises cannot
//     exist: base "prod:" makes CacheKey("application","x") = "prod::cache:..."
//   - two fleets nested by colon are NOT disjoint. Base "ha" has
//     AllKeysPattern() "ha:*", which matches "ha:prod:miner:sessions:...", and
//     `redis flush --all` deletes every key that pattern scans. One fleet wipes
//     the other, in exactly the shared-Redis deployment the docs describe.
//
// Operators separating fleets have a mechanism that actually isolates: a
// different Redis database, or a different server. A colon hierarchy only looks
// like one (ruling: 2026-08-27).
// The rule rejects what Redis itself treats as structure -- a namespace
// separator and the glob metacharacters a SCAN pattern is built from -- and
// nothing else. It was a whitelist until 2026-08-28, and a whitelist rejected
// characters with no stated hazard: a fleet running base_prefix "prod.v1" today
// is disjoint, carries no glob and no colon, and would have refused to start
// after the upgrade, with the only remedy a rename that relocates the entire
// keyspace including the WAL the miner is consuming -- the exact outcome this
// guard exists to prevent (ruling: Jorge, 2026-08-28).
var basePrefixRejected = regexp.MustCompile(`[:*?\[\]]|\s`)

// basePrefixAccepted reports whether the effective base prefix is usable.
func basePrefixAccepted(prefix string) bool {
	return prefix != "" && !basePrefixRejected.MatchString(prefix)
}

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
	// The EFFECTIVE prefix, not the raw field: an omitted base_prefix is valid
	// and means the default. WithDefaults runs later, at KeyBuilder
	// construction, so validating ns.BasePrefix directly would reject every
	// config that simply does not set it.
	if effective := ns.WithDefaults().BasePrefix; !basePrefixAccepted(effective) {
		return fmt.Errorf(
			"redis.namespace.base_prefix %q is not a single namespace segment. It must not be "+
				"empty and must not contain ':', whitespace, or the glob characters * ? [ ]. "+
				"A glob character reaches every SCAN pattern the key builder produces and one of those "+
				"feeds a delete; a ':' is worse for being quiet: a trailing one produces keys with an "+
				"empty segment (%q would build \"prod::cache:application:x\"), and two fleets nested by "+
				"colon are not disjoint -- base \"ha\" scans \"ha:*\", which matches every key of a fleet "+
				"based at \"ha:prod\", so `redis flush --all` on one deletes the other. To separate "+
				"fleets use a different Redis database or server, which isolates for real. "+
				"If this value is already in use, do NOT simply rename it: that relocates the entire "+
				"keyspace, including the relay WAL the miner consumes -- drain the fleet and migrate. "+
				"To inspect the keyspace of a config that no longer starts, the CLI takes "+
				"--base-prefix",
			ns.BasePrefix, "prod:")
	}

	if retired := ns.retiredNamespaceFields(); len(retired) > 0 {
		return fmt.Errorf(
			"redis.namespace no longer supports per-family prefixes, and yours customizes %d of them: %s. "+
				"The key layout below base_prefix is now fixed in code, so starting with this config would "+
				"relocate that data: the fleet would come up healthy against an empty keyspace and leave its "+
				"sessions, meters and relay WAL behind under the old names. Remove those lines. If keys really "+
				"live under them today, drain the fleet and migrate before upgrading. Do NOT fold the old value "+
				"into base_prefix: that moves the ENTIRE keyspace instead of one family. "+
				"consumer_group_prefix is the exception and its hazard is different: the consumer "+
				"group is a NAME, not a key (it is dash-joined, \"ha-miners\", so it never sits in the "+
				"keyspace). Removing that line renames the group, which orphans its pending-entries "+
				"list -- relays already delivered and not yet acked are then never reclaimed",
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
