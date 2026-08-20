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
		"CacheKey":                    kb.CacheKey("application", "k1"),
		"CacheLockKey":                kb.CacheLockKey("application", "k1"),
		"CacheKnownKey":               kb.CacheKnownKey("applications"),
		"EventChannel":                kb.EventChannel("supplier", "invalidate"),
		"StreamPrefix":                kb.StreamPrefix(),
		"StreamKey":                   kb.StreamKey("pokt1abc"),
		"StreamPattern":               kb.StreamPattern(),
		"ConsumerGroup":               kb.ConsumerGroup(),
		"MinerSessionKey":             kb.MinerSessionKey("sup1", "sess1"),
		"SupplierKeyPrefix":           kb.SupplierKeyPrefix(),
		"SuppliersRegistryPrefix":     kb.SuppliersRegistryPrefix(),
		"SupplierRegistryKey":         kb.SupplierRegistryKey("pokt1abc"),
		"SuppliersRegistryIndexKey":   kb.SuppliersRegistryIndexKey(),
		"CachePrefix":                 kb.CachePrefix(),
		"ParamsSharedAtHeightKey":     kb.ParamsSharedAtHeightKey(42),
		"ParamsSharedAtHeightLockKey": kb.ParamsSharedAtHeightLockKey(42),
		"ParamsSupplierKey":           kb.ParamsSupplierKey(),
		"ParamsSupplierLockKey":       kb.ParamsSupplierLockKey(),
		"SessionCacheKey":             kb.SessionCacheKey("app1", "svc1", 42),
		"MinerSessionsPrefix":         kb.MinerSessionsPrefix(),
		"GlobalLeaderKey":             kb.GlobalLeaderKey(),
		"ParamsProofKey":              kb.ParamsProofKey(),
		"ParamsProofLockKey":          kb.ParamsProofLockKey(),
		"ParamsSharedCacheKey":        kb.ParamsSharedCacheKey(),
		"ParamsSharedLockKey":         kb.ParamsSharedLockKey(),
		"MeterCleanupChannel":         kb.MeterCleanupChannel(),
		"MeterActiveSessionsKey":      kb.MeterActiveSessionsKey(),
		"BlockEventChannel":           kb.BlockEventChannel(),
		"SMSTNodesKey":                kb.SMSTNodesKey("sup1", "sess1"),
		"SMSTNodesPattern":            kb.SMSTNodesPattern(),
		"SMSTNodesPrefix":             kb.SMSTNodesPrefix(),
		"SMSTRootKey":                 kb.SMSTRootKey("sup1", "sess1"),
		"SMSTStatsKey":                kb.SMSTStatsKey("sup1", "sess1"),
		"SMSTLiveRootKey":             kb.SMSTLiveRootKey("sup1", "sess1"),
		"ServiceFactorDefaultKey":     kb.ServiceFactorDefaultKey(),
		"ServiceFactorServiceKey":     kb.ServiceFactorServiceKey("svc1"),
		"MinerClaimKey":               kb.MinerClaimKey("sup1"),
		"MinerActiveSetKey":           kb.MinerActiveSetKey(),
		"MinerInstanceKey":            kb.MinerInstanceKey("inst1"),

		"SupplierParamsInvalidateChannel": kb.SupplierParamsInvalidateChannel(),
		"MinerDedupPrefix":                kb.MinerDedupPrefix(),
		"MinerSessionStateIndexKey":       kb.MinerSessionStateIndexKey("sup1", "proved"),
		"MinerSessionsIndexKey":           kb.MinerSessionsIndexKey("sup1"),
		"TxTrackKey":                      kb.TxTrackKey("sup1", 100, "sess1"),
		"TxTrackPattern":                  kb.TxTrackPattern("sup1"),
		"TxTrackAllPattern":               kb.TxTrackAllPattern(),
		"AllKeysPattern":                  kb.AllKeysPattern(),
		"RebroadcastKey":                  kb.RebroadcastKey("claim", "sup1", 100),
		"RebroadcastIndexKey":             kb.RebroadcastIndexKey("claim"),
		"MinerDedupSessionKey":            kb.MinerDedupSessionKey("sess1"),
		"MeterSessionKey":                 kb.MeterSessionKey("sess1"),
		"MeterMetaKey":                    kb.MeterMetaKey("sess1", "pokt1a"),
		"MeterConsumedKey":                kb.MeterConsumedKey("sess1", "pokt1a"),
		"MeterSessionMetaPattern":         kb.MeterSessionMetaPattern("sess1"),
		"SupplierStateKey":                kb.SupplierStateKey("pokt1a"),
		"SupplierStatePattern":            kb.SupplierStatePattern(),
		"SMSTSessionNodesPattern":         kb.SMSTSessionNodesPattern("sess1"),
		"LegacyParamsPattern":             kb.LegacyParamsPattern(),
		"SimulationReplayKey":             kb.SimulationReplayKey("deadbeef"),
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
		"CacheKey":                    "ha:cache:application:k1",
		"CacheLockKey":                "ha:cache:lock:application:k1",
		"CacheKnownKey":               "ha:cache:known:applications",
		"EventChannel":                "ha:events:cache:supplier:invalidate",
		"StreamPrefix":                "ha:relays",
		"StreamKey":                   "ha:relays:pokt1abc",
		"StreamPattern":               "ha:relays:*",
		"ConsumerGroup":               "ha-miners",
		"MinerSessionKey":             "ha:miner:sessions:sup1:sess1",
		"SupplierKeyPrefix":           "ha:supplier",
		"SuppliersRegistryPrefix":     "ha:suppliers",
		"SupplierRegistryKey":         "ha:suppliers:pokt1abc",
		"SuppliersRegistryIndexKey":   "ha:suppliers:index",
		"CachePrefix":                 "ha:cache",
		"ParamsSharedAtHeightKey":     "ha:cache:params:shared:42",
		"ParamsSharedAtHeightLockKey": "ha:cache:lock:params:shared:42",
		"ParamsSupplierKey":           "ha:cache:params:supplier",
		"ParamsSupplierLockKey":       "ha:cache:lock:params:supplier",
		"SessionCacheKey":             "ha:cache:session:app1:svc1:42",
		"MinerSessionsPrefix":         "ha:miner:sessions",
		"GlobalLeaderKey":             "ha:miner:global_leader",
		"ParamsProofKey":              "ha:cache:proof_params",
		"ParamsProofLockKey":          "ha:cache:lock:proof_params",
		"ParamsSharedCacheKey":        "ha:cache:shared_params",
		"ParamsSharedLockKey":         "ha:cache:lock:shared_params",
		"MeterCleanupChannel":         "ha:meter:cleanup",
		"MeterActiveSessionsKey":      "ha:meter:active_sessions",
		"BlockEventChannel":           "ha:events:blocks",

		// Frozen nonstandard channels (subscriber-side effective strings —
		// see each method's doc for why the scheme differs):
		"SupplierParamsInvalidateChannel": "ha:events:cache:invalidate:supplier_params",
		"MinerDedupPrefix":                "ha:miner:dedup",
		"MinerSessionStateIndexKey":       "ha:miner:sessions:sup1:state:proved",
		"MinerSessionsIndexKey":           "ha:miner:sessions:sup1:index",
		"TxTrackKey":                      "ha:tx:track:sup1:100:sess1",
		"TxTrackPattern":                  "ha:tx:track:sup1:*",
		"TxTrackAllPattern":               "ha:tx:track:*",
		"AllKeysPattern":                  "ha:*",
		"RebroadcastKey":                  "ha:miner:rebroadcast:{claim}:sup1:100",
		"RebroadcastIndexKey":             "ha:miner:rebroadcast:{claim}:index",
		"MinerDedupSessionKey":            "ha:miner:dedup:session:sess1",
		"MeterSessionKey":                 "ha:meter:sess1",
		"MeterMetaKey":                    "ha:meter:sess1:pokt1a:meta",
		"MeterConsumedKey":                "ha:meter:sess1:pokt1a:consumed",
		"MeterSessionMetaPattern":         "ha:meter:sess1:*:meta",
		"SupplierStateKey":                "ha:supplier:pokt1a",
		"SupplierStatePattern":            "ha:supplier:*",
		"SMSTSessionNodesPattern":         "ha:smst:*:sess1:nodes",
		"LegacyParamsPattern":             "ha:params:*",
		"SimulationReplayKey":             "ha:sim:replay:deadbeef",

		// Methods the original golden map omitted (review finding): the SMST
		// family, service factor, and miner coordination keys.
		"SMSTNodesKey":            "ha:smst:sup1:sess1:nodes",
		"SMSTNodesPattern":        "ha:smst:*:*:nodes",
		"SMSTNodesPrefix":         "ha:smst:",
		"SMSTRootKey":             "ha:smst:sup1:sess1:root",
		"SMSTStatsKey":            "ha:smst:sup1:sess1:stats",
		"SMSTLiveRootKey":         "ha:smst:sup1:sess1:live_root",
		"ServiceFactorDefaultKey": "ha:service_factor:default",
		"ServiceFactorServiceKey": "ha:service_factor:service:svc1",
		"MinerClaimKey":           "ha:miner:claim:sup1",
		"MinerActiveSetKey":       "ha:miner:active",
		"MinerInstanceKey":        "ha:miner:instance:inst1",
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
