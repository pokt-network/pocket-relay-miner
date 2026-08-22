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
			wantErr: "is invalid",
		},
		{
			name:    "and so would a space",
			ns:      RedisNamespaceConfig{BasePrefix: "two words"},
			wantErr: "is invalid",
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
