package redis

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
)

// allKeyBuilderOutputs exercises every KeyBuilder method with fixed sample
// arguments and returns method name → produced string. New KB methods MUST
// be added here — the property tests below only protect what they can see.
//
// StreamAddress and SupplierStateAddress return (string, bool), not a bare
// string like every builder here, so their "output" is the two results
// joined with "|" -- close enough to a golden string for the convention
// scanner (internal/conventions) to pin them by name, which is the actual
// point: it does not care about shape, only that every exported method has a
// row here and in the golden map.
func allKeyBuilderOutputs(kb *KeyBuilder) map[string]string {
	streamAddr, streamAddrOK := kb.StreamAddress(kb.StreamKey("pokt1abc"))
	supplierAddr, supplierAddrOK := kb.SupplierStateAddress(kb.SupplierStateKey("pokt1a"))
	extractors := map[string]string{
		"StreamAddress":        fmt.Sprintf("%s|%v", streamAddr, streamAddrOK),
		"SupplierStateAddress": fmt.Sprintf("%s|%v", supplierAddr, supplierAddrOK),
	}

	out := map[string]string{
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
		"MeterPrefix":                     kb.MeterPrefix(),
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
	for k, v := range extractors {
		out[k] = v
	}
	return out
}

// TestKeyBuilder_PartialNamespaceNeverProducesEmptySegments is the anti-`::`
// property test: no method may emit an empty segment.
//
// It used to guard per-field defaulting, because a namespace that set only
// base_prefix once left every other segment empty ("prod::application:x"). That
// cannot happen any more -- the segments are constants -- so the axis worth
// varying is the base, including the empty one. The test stays because the
// property is about the METHODS: a new one that forgets a segment still
// produces "::", and nothing else would catch it.
func TestKeyBuilder_PartialNamespaceNeverProducesEmptySegments(t *testing.T) {
	partials := []config.RedisNamespaceConfig{
		{},                   // empty → base defaulted
		{BasePrefix: "prod"}, // the footgun that produced "prod::..."
		{BasePrefix: "p"},
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
		"MeterPrefix":                     "ha:meter",
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

		// Extractors (review 2026-08-21): the "output" is result|ok, not a
		// bare key -- see allKeyBuilderOutputs' doc comment for why.
		"StreamAddress":        "pokt1abc|true",
		"SupplierStateAddress": "pokt1a|true",
	}
	outputs := allKeyBuilderOutputs(kb)
	for method, want := range golden {
		assert.Equalf(t, want, outputs[method], "golden string drift on %s (BREAKING for mixed-version fleets)", method)
	}
}

// TestKeyBuilder_AddressExtractorsRoundTrip pins StreamAddress and
// SupplierStateAddress: extractors, not builders, so they don't fit
// allKeyBuilderOutputs' single-string shape and are pinned here instead.
// Both exist so nothing outside this file hand-builds
// StreamPrefix()/SupplierKeyPrefix()+":" to trim a scanned key back to an
// address (review 2026-08-20, LOW; recurred review 2026-08-21) — a hand-built
// trim silently drifts from the builder the moment either format changes,
// exactly like a hand-built SCAN pattern would.
func TestKeyBuilder_AddressExtractorsRoundTrip(t *testing.T) {
	kb := NewKeyBuilder(config.RedisNamespaceConfig{})

	streamKey := kb.StreamKey("pokt1abc")
	addr, ok := kb.StreamAddress(streamKey)
	assert.True(t, ok)
	assert.Equal(t, "pokt1abc", addr)

	stateKey := kb.SupplierStateKey("pokt1abc")
	addr, ok = kb.SupplierStateAddress(stateKey)
	assert.True(t, ok)
	assert.Equal(t, "pokt1abc", addr)

	// A key from a foreign namespace/prefix must not be misread as an
	// address — this is the failure mode a drifted hand-built trim produces.
	_, ok = kb.StreamAddress("ha:cache:application:pokt1abc")
	assert.False(t, ok, "a non-stream key must not parse as a stream address")
	_, ok = kb.SupplierStateAddress("ha:cache:application:pokt1abc")
	assert.False(t, ok, "a non-supplier-state key must not parse as a supplier address")

	// The prefix alone, with nothing after the separator, must not extract
	// an empty address.
	_, ok = kb.StreamAddress(kb.StreamPrefix() + ":")
	assert.False(t, ok, "an empty address must not round-trip as valid")

	// A non-default namespace changes both prefixes' text. An extractor that
	// hand-built its own trim prefix at the CALL SITE instead of asking
	// StreamPrefix()/SupplierKeyPrefix() for it would still trim against the
	// DEFAULT text and silently misparse every key under this namespace --
	// exactly the drift this pair of methods exists to make impossible.
	custom := NewKeyBuilder(config.RedisNamespaceConfig{
		BasePrefix: "prod",
	})
	addr, ok = custom.StreamAddress(custom.StreamKey("pokt1xyz"))
	assert.True(t, ok)
	assert.Equal(t, "pokt1xyz", addr)
	addr, ok = custom.SupplierStateAddress(custom.SupplierStateKey("pokt1xyz"))
	assert.True(t, ok)
	assert.Equal(t, "pokt1xyz", addr)
}

// TestKeyBuilder_NoTwoMethodsCollideUnderAnyNamespace is the guard for the
// footgun that motivated deleting the registry's per-supplier key: two methods
// producing the SAME string for the same entity. SupplierStateKey is built from
// the configurable ns.SupplierPrefix while SupplierRegistryKey hardcoded
// "suppliers", so supplier_prefix: "suppliers" made them identical -- two
// different JSON structs writing one key, and because they shared the "status"
// and "services" fields the cross-read did not even fail, it returned a
// half-populated struct (no "staked" -> IsActive() false -> relays refused).
// Nothing validated it and no test caught it.
//
// It walks the methods by REFLECTION rather than through allKeyBuilderOutputs,
// for two reasons. Reflection cannot go stale: a method added tomorrow is
// covered without a table to remember. And it can feed every method the SAME
// argument, which allKeyBuilderOutputs deliberately does not -- it passes
// "pokt1a" to one supplier method and "pokt1abc" to another, so two layouts
// that collide for one address produce different strings there and the
// collision stays invisible. That is exactly why this bug survived a golden
// test of every method.
//
// Prefix methods are excluded: a prefix is not a key anything writes (it is the
// input to a pattern or a trim), so two prefixes agreeing corrupts nothing on
// its own. The extractors are excluded because they parse a key rather than
// build one.
//
// Equality is only half of it; the glob half is
// TestKeyBuilder_PatternsMatchOnlyTheirOwnFamily below, which is what the key
// layout becoming constant made checkable at all.
func TestKeyBuilder_NoTwoMethodsCollideUnderAnyNamespace(t *testing.T) {
	// Only the base varies. The per-family prefixes this list used to bend --
	// SupplierPrefix "suppliers", CachePrefix "supplier" -- are constants now, so
	// setting them here would produce output byte-identical to {} while the code
	// claimed to be exercising "the exact footgun". The footgun itself is pinned
	// where it now lives: change a constant in namespace.go and this test, plus
	// TestKeyBuilder_PatternsMatchOnlyTheirOwnFamily, go red.
	namespaces := []config.RedisNamespaceConfig{
		{},
		{BasePrefix: "prod"},
		{BasePrefix: "ha-2"},
	}

	notAKey := map[string]bool{"StreamAddress": true, "SupplierStateAddress": true}

	for _, ns := range namespaces {
		kb := NewKeyBuilder(ns)
		seen := map[string]string{} // output -> first method that produced it

		for method, out := range keyBuilderOutputsWithUniformArgs(t, kb, notAKey) {
			if first, dup := seen[out]; dup {
				t.Errorf("namespace %+v: %s and %s both produce %q -- "+
					"two writers on one key silently corrupt each other",
					ns, first, method, out)

				continue
			}

			seen[out] = method
		}
	}
}

// keyBuilderOutputsWithUniformArgs calls every exported KeyBuilder method whose
// arguments are all strings or integers, passing ONE canonical value per kind,
// and returns method name -> produced string. Methods with other argument or
// return shapes are skipped, as are the excluded names and anything ending in
// "Prefix".
func keyBuilderOutputsWithUniformArgs(
	t *testing.T,
	kb *KeyBuilder,
	exclude map[string]bool,
) map[string]string {
	t.Helper()

	const sampleString = "pokt1abc"
	const sampleNumber = 42

	out := map[string]string{}
	kbValue := reflect.ValueOf(kb)

	for i := range kbValue.NumMethod() {
		method := kbValue.Type().Method(i)
		if exclude[method.Name] || strings.HasSuffix(method.Name, "Prefix") {
			continue
		}

		signature := kbValue.Method(i).Type()
		if signature.NumOut() == 0 || signature.Out(0).Kind() != reflect.String {
			continue
		}

		args := make([]reflect.Value, 0, signature.NumIn())
		callable := true

		for j := range signature.NumIn() {
			switch kind := signature.In(j).Kind(); kind {
			case reflect.String:
				args = append(args, reflect.ValueOf(sampleString))
			case reflect.Int64, reflect.Int:
				args = append(args, reflect.New(signature.In(j)).Elem())
				args[j].SetInt(sampleNumber)
			// SetInt panics on an unsigned Value ("reflect: call of
			// reflect.Value.SetInt on uint64 Value"). No KeyBuilder method takes
			// a uint64 today, so folding it into the signed case was latent --
			// and it would have made the first one added PANIC this guard rather
			// than fail it, in a helper whose whole point is not going stale.
			case reflect.Uint64, reflect.Uint:
				args = append(args, reflect.New(signature.In(j)).Elem())
				args[j].SetUint(sampleNumber)
			default:
				callable = false
			}

			if !callable {
				break
			}
		}

		if !callable {
			continue
		}

		out[method.Name] = kbValue.Method(i).Call(args)[0].String()
	}

	require.NotEmpty(t, out, "reflection found no callable KeyBuilder methods")

	return out
}

// TestKeyBuilder_PatternsMatchOnlyTheirOwnFamily is the glob half of the
// collision guard, and it only became possible once the key layout stopped being
// operator-configurable.
//
// Two methods do not have to produce the SAME key to corrupt each other: a SCAN
// pattern eating another family's keys is worse, because a pattern feeds
// deletion (`redis cache --type supplier --invalidate` deletes what it scans).
// While the segments were config, this was reachable: supplier_prefix
// "suppliers" made SupplierStatePattern() "ha:suppliers:*", which matched the
// fleet index, and cache_prefix "supplier" made it match every cache key. No
// test could pin the property, because it depended on values an operator chose.
//
// Now every segment below the base is a constant, so what a pattern matches is a
// property of the code and nothing else. Each pattern must match its OWN family
// and nothing more.
func TestKeyBuilder_PatternsMatchOnlyTheirOwnFamily(t *testing.T) {
	expected := map[string][]string{
		"StreamPattern":           {"StreamKey"},
		"SupplierStatePattern":    {"SupplierStateKey"},
		"SMSTNodesPattern":        {"SMSTNodesKey"},
		"SMSTSessionNodesPattern": {"SMSTNodesKey"},
		"MeterSessionMetaPattern": {"MeterMetaKey"},
		"TxTrackPattern":          {"TxTrackKey"},
		"TxTrackAllPattern":       {"TxTrackKey"},
		// Matches nothing: the family it scanned for is gone, and it is kept
		// only so an upgrade can clear what older versions left behind.
		"LegacyParamsPattern": {},
	}

	notAKey := map[string]bool{"StreamAddress": true, "SupplierStateAddress": true}

	// Several bases, because the base is the one thing still configurable and it
	// must not change which family a pattern reaches.
	for _, ns := range []config.RedisNamespaceConfig{{}, {BasePrefix: "prod"}, {BasePrefix: "ha-2"}} {
		kb := NewKeyBuilder(ns)
		outputs := keyBuilderOutputsWithUniformArgs(t, kb, notAKey)

		for name, pattern := range outputs {
			if !strings.HasSuffix(name, "Pattern") || name == "AllKeysPattern" {
				continue
			}

			want, listed := expected[name]
			require.Truef(t, listed,
				"%s is a new SCAN pattern with no expectation here. Add it: a pattern "+
					"nobody pinned is a pattern nobody noticed was eating another family", name)

			var matched []string
			for other, key := range outputs {
				if other == name || strings.HasSuffix(other, "Pattern") {
					continue
				}
				if ok, err := filepath.Match(pattern, key); err == nil && ok {
					matched = append(matched, other)
				}
			}

			require.ElementsMatchf(t, want, matched,
				"namespace %+v: %s (%q) matches %v, expected %v -- a pattern reaching "+
					"another family is a scan-and-delete reaching data it does not own",
				ns, name, pattern, matched, want)
		}
	}
}

// TestKeyBuilder_AllKeysPatternMatchesEverything pins the one pattern that is
// SUPPOSED to be total, so the test above excluding it stays honest: if this
// ever stopped matching some family, a cleanup that relies on it would silently
// skip that family instead of failing.
func TestKeyBuilder_AllKeysPatternMatchesEverything(t *testing.T) {
	kb := NewKeyBuilder(config.RedisNamespaceConfig{})
	outputs := keyBuilderOutputsWithUniformArgs(t, kb,
		map[string]bool{"StreamAddress": true, "SupplierStateAddress": true})

	all := outputs["AllKeysPattern"]
	require.NotEmpty(t, all)

	for name, key := range outputs {
		// ConsumerGroup is not a key: it is a Redis Streams consumer-group name,
		// and it is joined with a dash ("ha-miners") precisely so it does not sit
		// in the keyspace. No key pattern should reach it.
		if strings.HasSuffix(name, "Pattern") || name == "ConsumerGroup" {
			continue
		}
		ok, err := filepath.Match(all, key)
		require.NoError(t, err)
		require.Truef(t, ok, "AllKeysPattern (%q) does not match %s (%q)", all, name, key)
	}
}

// TestSupplierStateAddress_RejectsForeignKeys pins that the extractor answers
// only for keys it actually owns.
//
// A supplier state key has exactly one segment after the prefix. The check used
// to be "the trim changed something", which answers confidently for any key
// sharing the prefix: under the retired cache_prefix "supplier" a CACHE key came
// back as the address "application:k1". Neither call site can produce that today
// -- both scan SupplierStatePattern(), which reaches only its own family -- so
// this pins the extractor's own contract rather than a reachable bug.
func TestSupplierStateAddress_RejectsForeignKeys(t *testing.T) {
	kb := NewKeyBuilder(config.RedisNamespaceConfig{})

	addr, ok := kb.SupplierStateAddress(kb.SupplierStateKey("pokt1abc"))
	require.True(t, ok, "premise: a real supplier state key must resolve")
	require.Equal(t, "pokt1abc", addr)

	for _, foreign := range []string{
		"ha:supplier:application:k1", // the shape the retired cache_prefix produced
		"ha:cache:application:k1",
		"ha:suppliers:index",
		"ha:supplier:",
		"ha:supplier",
	} {
		_, ok := kb.SupplierStateAddress(foreign)
		require.Falsef(t, ok, "%q is not a supplier state key and must not resolve to one", foreign)
	}
}
