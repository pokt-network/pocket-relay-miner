package redis

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// allKeyBuilderOutputs exercises every KeyBuilder method with fixed sample
// arguments and returns method name → produced string. New KB methods MUST
// be added here — the property tests below only protect what they can see.
func allKeyBuilderOutputs(kb *KeyBuilder) map[string]string {
	return map[string]string{
		"CacheKey":                  kb.CacheKey("application", "k1"),
		"CacheLockKey":              kb.CacheLockKey("application", "k1"),
		"CacheKnownKey":             kb.CacheKnownKey("applications"),
		"EventChannel":              kb.EventChannel("supplier", "invalidate"),
		"StreamPrefix":              kb.StreamPrefix(),
		"ConsumerGroup":             kb.ConsumerGroup(),
		"MinerSessionKey":           kb.MinerSessionKey("sup1", "sess1"),
		"SupplierKeyPrefix":         kb.SupplierKeyPrefix(),
		"SuppliersRegistryPrefix":   kb.SuppliersRegistryPrefix(),
		"SuppliersRegistryIndexKey": kb.SuppliersRegistryIndexKey(),
		"CachePrefix":               kb.CachePrefix(),
		"EventsCachePrefix":         kb.EventsCachePrefix(),
		"MinerSessionsPrefix":       kb.MinerSessionsPrefix(),
		"GlobalLeaderKey":           kb.GlobalLeaderKey(),
		"ParamsProofKey":            kb.ParamsProofKey(),
		"ParamsProofLockKey":        kb.ParamsProofLockKey(),
		"ParamsSharedCacheKey":      kb.ParamsSharedCacheKey(),
		"ParamsSharedLockKey":       kb.ParamsSharedLockKey(),
		"ParamsSessionKey":          kb.ParamsSessionKey(),
		"MeterCleanupChannel":       kb.MeterCleanupChannel(),
		"MeterActiveSessionsKey":    kb.MeterActiveSessionsKey(),
		"SupplierUpdateChannel":     kb.SupplierUpdateChannel(),
		"BlockEventChannel":         kb.BlockEventChannel(),
		"SMSTNodesKey":              kb.SMSTNodesKey("sup1", "sess1"),
		"SMSTNodesPattern":          kb.SMSTNodesPattern(),
		"SMSTNodesPrefix":           kb.SMSTNodesPrefix(),
		"SMSTRootKey":               kb.SMSTRootKey("sup1", "sess1"),
		"SMSTStatsKey":              kb.SMSTStatsKey("sup1", "sess1"),
		"SMSTLiveRootKey":           kb.SMSTLiveRootKey("sup1", "sess1"),
		"ServiceFactorDefaultKey":   kb.ServiceFactorDefaultKey(),
		"ServiceFactorServiceKey":   kb.ServiceFactorServiceKey("svc1"),
		"MinerClaimKey":             kb.MinerClaimKey("sup1"),
		"MinerActiveSetKey":         kb.MinerActiveSetKey(),
		"MinerInstanceKey":          kb.MinerInstanceKey("inst1"),

		"SupplierParamsInvalidateChannel":     kb.SupplierParamsInvalidateChannel(),
		"SharedParamsHeightInvalidateChannel": kb.SharedParamsHeightInvalidateChannel(),
		"SessionRewardableChannel":            kb.SessionRewardableChannel(),
		"MinerLeaderPrefix":                   kb.MinerLeaderPrefix(),
		"MinerDedupPrefix":                    kb.MinerDedupPrefix(),
		"MinerSessionStateIndexKey":           kb.MinerSessionStateIndexKey("sup1", "proved"),
		"MinerSessionsIndexKey":               kb.MinerSessionsIndexKey("sup1"),
		"TxTrackKey":                          kb.TxTrackKey("sup1", 100, "sess1"),
		"TxTrackPattern":                      kb.TxTrackPattern("sup1"),
		"TxTrackAllPattern":                   kb.TxTrackAllPattern(),
		"AllKeysPattern":                      kb.AllKeysPattern(),
		"RebroadcastKey":                      kb.RebroadcastKey("claim", "sup1", 100),
		"RebroadcastIndexKey":                 kb.RebroadcastIndexKey("claim"),
		"MinerDedupSessionKey":                kb.MinerDedupSessionKey("sess1"),
		"MeterSessionKey":                     kb.MeterSessionKey("sess1"),
		"AppStakeKey":                         kb.AppStakeKey("app1"),
		"ServiceComputeUnitsKey":              kb.ServiceComputeUnitsKey("svc1"),
	}
}

// TestKeyBuilder_PartialNamespaceNeverProducesEmptySegments is the anti-`::`
// property test: an operator setting ONLY base_prefix (the realistic partial
// config) must still get every sub-prefix defaulted, on every method.
func TestKeyBuilder_PartialNamespaceNeverProducesEmptySegments(t *testing.T) {
	partials := []config.RedisNamespaceConfig{
		{},                     // fully empty → all defaults
		{BasePrefix: "prod"},   // the footgun that produced "prod::..."
		{CachePrefix: "kache"}, // sub-prefix only, base defaulted
		{BasePrefix: "p", MinerPrefix: "m"},
	}
	for _, ns := range partials {
		kb := NewKeyBuilder(ns)
		for method, out := range allKeyBuilderOutputs(kb) {
			assert.NotContainsf(t, out, "::", "%s produced an empty segment with partial ns %+v: %q", method, ns, out)
			assert.Falsef(t, strings.HasPrefix(out, ":"), "%s starts with ':' under %+v: %q", method, ns, out)
		}
	}
}

// TestKeyBuilder_DefaultGoldenStrings pins the default-namespace output of
// every method. These strings are the cross-version wire contract: a mixed
// fleet (old miner, new relayer) only keeps working if both build the SAME
// keys and channels. Changing any value here is a BREAKING change — do not
// update an expectation without a migration plan.
func TestKeyBuilder_DefaultGoldenStrings(t *testing.T) {
	kb := NewKeyBuilder(config.RedisNamespaceConfig{})
	golden := map[string]string{
		"CacheKey":                  "ha:cache:application:k1",
		"CacheLockKey":              "ha:cache:lock:application:k1",
		"CacheKnownKey":             "ha:cache:known:applications",
		"EventChannel":              "ha:events:cache:supplier:invalidate",
		"StreamPrefix":              "ha:relays",
		"ConsumerGroup":             "ha-miners",
		"MinerSessionKey":           "ha:miner:sessions:sup1:sess1",
		"SupplierKeyPrefix":         "ha:supplier",
		"SuppliersRegistryPrefix":   "ha:suppliers",
		"SuppliersRegistryIndexKey": "ha:suppliers:index",
		"CachePrefix":               "ha:cache",
		"EventsCachePrefix":         "ha:events:cache",
		"MinerSessionsPrefix":       "ha:miner:sessions",
		"GlobalLeaderKey":           "ha:miner:global_leader",
		"ParamsProofKey":            "ha:cache:proof_params",
		"ParamsProofLockKey":        "ha:cache:lock:proof_params",
		"ParamsSharedCacheKey":      "ha:cache:shared_params",
		"ParamsSharedLockKey":       "ha:cache:lock:shared_params",
		"ParamsSessionKey":          "ha:cache:session_params",
		"MeterCleanupChannel":       "ha:meter:cleanup",
		"MeterActiveSessionsKey":    "ha:meter:active_sessions",
		"SupplierUpdateChannel":     "ha:events:supplier_update",
		"BlockEventChannel":         "ha:events:blocks",

		// Frozen nonstandard channels (subscriber-side effective strings —
		// see each method's doc for why the scheme differs):
		"SupplierParamsInvalidateChannel":     "ha:events:cache:invalidate:supplier_params",
		"SharedParamsHeightInvalidateChannel": "ha:events:invalidate:params",
		"SessionRewardableChannel":            "ha:events:session:rewardable",
		"MinerLeaderPrefix":                   "ha:miner:leader",
		"MinerDedupPrefix":                    "ha:miner:dedup",
		"MinerSessionStateIndexKey":           "ha:miner:sessions:sup1:state:proved",
		"MinerSessionsIndexKey":               "ha:miner:sessions:sup1:index",
		"TxTrackKey":                          "ha:tx:track:sup1:100:sess1",
		"TxTrackPattern":                      "ha:tx:track:sup1:*",
		"TxTrackAllPattern":                   "ha:tx:track:*",
		"AllKeysPattern":                      "ha:*",
		"RebroadcastKey":                      "ha:miner:rebroadcast:{claim}:sup1:100",
		"RebroadcastIndexKey":                 "ha:miner:rebroadcast:{claim}:index",
		"MinerDedupSessionKey":                "ha:miner:dedup:session:sess1",
		"MeterSessionKey":                     "ha:meter:sess1",
		"AppStakeKey":                         "ha:app_stake:app1",
		"ServiceComputeUnitsKey":              "ha:service:svc1:compute_units",
	}
	outputs := allKeyBuilderOutputs(kb)
	for method, want := range golden {
		assert.Equalf(t, want, outputs[method], "golden string drift on %s (BREAKING for mixed-version fleets)", method)
	}
}

// TestWithDefaults_FieldByField proves each field defaults independently.
func TestWithDefaults_FieldByField(t *testing.T) {
	ns := config.RedisNamespaceConfig{BasePrefix: "prod"}.WithDefaults()
	def := config.DefaultRedisNamespaceConfig()

	assert.Equal(t, "prod", ns.BasePrefix, "explicit field preserved")
	assert.Equal(t, def.CachePrefix, ns.CachePrefix)
	assert.Equal(t, def.EventsPrefix, ns.EventsPrefix)
	assert.Equal(t, def.StreamsPrefix, ns.StreamsPrefix)
	assert.Equal(t, def.MinerPrefix, ns.MinerPrefix)
	assert.Equal(t, def.SupplierPrefix, ns.SupplierPrefix)
	assert.Equal(t, def.MeterPrefix, ns.MeterPrefix)
	assert.Equal(t, def.ParamsPrefix, ns.ParamsPrefix)
	assert.Equal(t, def.ConsumerGroupPrefix, ns.ConsumerGroupPrefix)

	full := config.RedisNamespaceConfig{
		BasePrefix: "a", CachePrefix: "b", EventsPrefix: "c", StreamsPrefix: "d",
		MinerPrefix: "e", SupplierPrefix: "f", MeterPrefix: "g", ParamsPrefix: "h",
		ConsumerGroupPrefix: "i",
	}
	assert.Equal(t, full, full.WithDefaults(), "fully-specified namespace untouched")
}
