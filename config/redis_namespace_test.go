//go:build test

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRedisNamespaceValidate covers the migration guard, which exists because
// the failure it catches is silent: a config that customized a per-family prefix
// was writing keys under names this version no longer builds, so without the
// guard the fleet starts healthy against an EMPTY keyspace and leaves its
// sessions, meters and relay WAL behind under the old names.
func TestRedisNamespaceValidate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		ns      RedisNamespaceConfig
		wantErr string
	}{
		{
			name: "empty namespace is the default",
			ns:   RedisNamespaceConfig{},
		},
		{
			name: "a custom base prefix is the supported knob",
			ns:   RedisNamespaceConfig{BasePrefix: "prod"},
		},
		{
			name: "a retired prefix set to the value it already had moves nothing",
			ns:   RedisNamespaceConfig{BasePrefix: "prod", CachePrefix: "cache", MinerPrefix: "miner"},
		},
		{
			name:    "the prefix that used to collide with the registry family",
			ns:      RedisNamespaceConfig{SupplierPrefix: "suppliers"},
			wantErr: `supplier_prefix: "suppliers"`,
		},
		{
			name:    "the prefix that used to make the supplier scan eat the cache",
			ns:      RedisNamespaceConfig{CachePrefix: "supplier"},
			wantErr: `cache_prefix: "supplier"`,
		},
		{
			name:    "several at once are all named, so one fix is enough",
			ns:      RedisNamespaceConfig{MeterPrefix: "m", StreamsPrefix: "s"},
			wantErr: "customizes 2 of them",
		},
		{
			name:    "a glob in the base prefix would end up inside every SCAN pattern",
			ns:      RedisNamespaceConfig{BasePrefix: "ha:*"},
			wantErr: "single namespace segment",
		},
		{
			name:    "and so would a space",
			ns:      RedisNamespaceConfig{BasePrefix: "two words"},
			wantErr: "single namespace segment",
		},
		{
			name:    "a bracket is a glob character too",
			ns:      RedisNamespaceConfig{BasePrefix: "ha[12]"},
			wantErr: "single namespace segment",
		},
		// The two cases that caught the first version of this rule. It was
		// written as `[*?\[\]\\s]`, which in a Go raw string is the class
		// {* ? [ ] \ s} -- the LETTER s, and no whitespace at all. So it locked
		// out every base containing an "s" and let a space straight through,
		// which is the opposite of the rule on both counts. The cases above did
		// not catch it: "two words" errored on the "s" of "words" and "ha:*" on
		// the star, so both passed for the wrong reason.
		{
			name: "a base containing 's' is ordinary text and must be accepted",
			ns:   RedisNamespaceConfig{BasePrefix: "prod-us"},
		},
		{
			name: "and so is one that is mostly s",
			ns:   RedisNamespaceConfig{BasePrefix: "suppliers"},
		},
		{
			name:    "a bare space, with no other suspicious character",
			ns:      RedisNamespaceConfig{BasePrefix: "ha prod"},
			wantErr: "single namespace segment",
		},
		{
			name:    "a tab, likewise",
			ns:      RedisNamespaceConfig{BasePrefix: "ha\tprod"},
			wantErr: "single namespace segment",
		},
		// A colon does NOT "only add a segment". The base prefix is one
		// namespace segment; a colon turns it into a hierarchy the key layout
		// does not model, and two fleets nested that way are not disjoint --
		// base "ha" scans "ha:*", which matches every key of a fleet based at
		// "ha:prod", and that pattern feeds `redis flush --all`.
		{
			name:    "a colon nests one fleet inside another's scan pattern",
			ns:      RedisNamespaceConfig{BasePrefix: "ha:prod"},
			wantErr: "single namespace segment",
		},
		{
			// The empty segment this package promises cannot exist:
			// "prod:" builds "prod::cache:application:x".
			name:    "a trailing colon produces the empty segment",
			ns:      RedisNamespaceConfig{BasePrefix: "prod:"},
			wantErr: "single namespace segment",
		},
		{
			// Accepted since 2026-08-28. A dot is not a Redis namespace
			// separator, carries no glob, and a fleet based at "pocket.ha" is
			// disjoint from every other -- so rejecting it only meant an existing
			// fleet would refuse to start after upgrading, with the sole remedy a
			// rename that relocates the whole keyspace including the WAL the
			// miner is consuming. The rule rejects what Redis treats as
			// structure, not everything unfamiliar (ruling: Jorge, 2026-08-28).
			name: "a dot is not a separator, so it is accepted",
			ns:   RedisNamespaceConfig{BasePrefix: "pocket.ha"},
		},
		{
			// An omitted base_prefix is VALID and means the default. Validate
			// runs before WithDefaults, so checking the raw field would reject
			// every config that simply does not set it.
			name: "an omitted base_prefix falls to the default and must start",
			ns:   RedisNamespaceConfig{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ns.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err, "this config moves no keys and must start")
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr,
				"the error must name the offending field: an operator has to know WHICH line to remove")
		})
	}
}

// TestRedisNamespaceWithDefaultsOnlyFillsTheBase pins that defaulting no longer
// has anything else to fill. It used to fill eight fields, and a partial
// namespace that skipped that path produced keys with empty segments
// ("prod::application:x"); now those segments are constants and cannot be empty.
func TestRedisNamespaceWithDefaultsOnlyFillsTheBase(t *testing.T) {
	got := RedisNamespaceConfig{}.WithDefaults()

	require.Equal(t, "ha", got.BasePrefix)
	require.Equal(t, RedisNamespaceConfig{BasePrefix: "ha"}, got,
		"WithDefaults must not resurrect the retired fields: anything non-empty here "+
			"would make Validate reject a config the operator never wrote")
}
