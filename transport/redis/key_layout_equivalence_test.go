//go:build test

package redis_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// A key layout must have exactly ONE constructor, and it must be the
// KeyBuilder's. Several components were instead handed the FIRST half of a
// layout (a prefix from a KeyBuilder method) and assembled the SECOND half
// themselves with Sprintf, which is the half that goes out of sync — the shape
// that produced the meter split, where the writer used five segments and the
// reader three.
//
// These tests pin the equivalence at the moment the concatenations were
// replaced by the KeyBuilder methods. They are a characterisation test, not a
// design statement: they exist so that the replacement provably changed no key
// on the wire, and they keep failing if a future edit moves one method's format
// without moving the other.
//
// A changed key format is a BREAKING cross-version change. A mixed fleet stops
// seeing each other's state — the miner writes where the relayer does not look.

func kb(t *testing.T, ns config.RedisNamespaceConfig) *redisutil.KeyBuilder {
	t.Helper()
	return redisutil.NewKeyBuilder(ns)
}

// namespaces exercises the default and a fully non-default configuration:
// an equivalence that only holds for "ha" would hide a method that ignores a
// configured sub-prefix.
func namespaces() []config.RedisNamespaceConfig {
	return []config.RedisNamespaceConfig{
		{},
		{
			BasePrefix:     "prod",
			MinerPrefix:    "mining",
			SupplierPrefix: "suppliers-state",
			StreamsPrefix:  "wal",
			CachePrefix:    "caching",
		},
	}
}

func TestSupplierStateKey_MatchesPrefixPlusAddress(t *testing.T) {
	const addr = "pokt1supplier_equivalence"
	for _, ns := range namespaces() {
		builder := kb(t, ns)
		require.Equal(t,
			fmt.Sprintf("%s:%s", builder.SupplierKeyPrefix(), addr),
			builder.SupplierStateKey(addr),
			"cache/supplier_cache.go and cmd/redis/supplier.go built this by hand; "+
				"SupplierStateKey must produce the identical string")
	}
}

func TestMinerSessionKeys_MatchSessionsPrefixConcatenation(t *testing.T) {
	const (
		supplier  = "pokt1session_equivalence"
		sessionID = "sess-equivalence"
		state     = "active"
	)
	for _, ns := range namespaces() {
		builder := kb(t, ns)
		prefix := builder.MinerSessionsPrefix()

		require.Equal(t,
			fmt.Sprintf("%s:%s:%s", prefix, supplier, sessionID),
			builder.MinerSessionKey(supplier, sessionID),
			"miner/session_store.go built the session key by hand")
		require.Equal(t,
			fmt.Sprintf("%s:%s:index", prefix, supplier),
			builder.MinerSessionsIndexKey(supplier),
			"miner/session_store.go built the index key by hand")
		require.Equal(t,
			fmt.Sprintf("%s:%s:state:%s", prefix, supplier, state),
			builder.MinerSessionStateIndexKey(supplier, state),
			"miner/session_store.go built the state index key by hand")
	}
}

func TestSupplierRegistryKey_MatchesRegistryPrefixPlusAddress(t *testing.T) {
	const addr = "pokt1registry_equivalence"
	for _, ns := range namespaces() {
		builder := kb(t, ns)
		require.Equal(t,
			fmt.Sprintf("%s:%s", builder.SuppliersRegistryPrefix(), addr),
			builder.SupplierRegistryKey(addr),
			"miner/supplier_registry.go built this by hand in three places")
	}
}

func TestStreamKey_MatchesStreamPrefixPlusSupplier(t *testing.T) {
	const supplier = "pokt1stream_equivalence"
	for _, ns := range namespaces() {
		builder := kb(t, ns)
		require.Equal(t,
			fmt.Sprintf("%s:%s", builder.StreamPrefix(), supplier),
			builder.StreamKey(supplier),
			"cmd/redis/streams.go built this by hand")
	}
}

// TestSupplierStreamName_AgreesWithTheKeyBuilder ties the relay WAL's own
// constructor to the KeyBuilder's.
//
// transport.SupplierStreamName stays: the transport layer takes a bare
// redis.UniversalClient and must not depend on the KeyBuilder, and it is
// already the SINGLE constructor the publisher and the consumer share, so
// those two cannot drift from each other. What it could drift from is the
// KeyBuilder — and therefore from the debug CLI and from anything else that
// asks where a supplier's relays live. That is what this pins.
//
// If the WAL layout ever has to change, both must change together, and this
// test is what says so.
func TestSupplierStreamName_AgreesWithTheKeyBuilder(t *testing.T) {
	const supplier = "pokt1stream_wire_contract"
	for _, ns := range namespaces() {
		builder := kb(t, ns)
		require.Equal(t,
			builder.StreamKey(supplier),
			transport.SupplierStreamName(builder.StreamPrefix(), supplier),
			"the WAL's stream name and the KeyBuilder's must be the same string")
	}
}
