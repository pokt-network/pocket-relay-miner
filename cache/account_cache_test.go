//go:build test

package cache

import (
	"context"
	"testing"
	"time"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// frozenAccountQueryClient models the chain query layer for account pubkeys: it
// returns whatever pubkey it is currently configured to serve. The test mutates
// `pubKey` to simulate a downstream change so we can prove the L1 entry is aged
// out by accountCacheL1TTL instead of frozen for the process lifetime.
type frozenAccountQueryClient struct {
	pubKey cryptotypes.PubKey
}

func (f *frozenAccountQueryClient) GetPubKeyFromAddress(_ context.Context, _ string) (cryptotypes.PubKey, error) {
	return f.pubKey, nil
}

// pubKeyFromByte builds a deterministic, distinct, valid 33-byte secp256k1
// pubkey from a single seed byte so the two cache values are unambiguously
// different (and round-trip through the cache's raw-33-byte L2 encoding).
func pubKeyFromByte(b byte) cryptotypes.PubKey {
	key := make([]byte, 33)
	key[0] = 0x02 // compressed-point prefix so unmarshalPubKey accepts it
	key[32] = b
	return &secp256k1.PubKey{Key: key}
}

// TestAccountCache_L1RefreshesAfterTTL mirrors TestServiceCache_L1RefreshesAfterTTL.
// The account L1 (in-process xsync map) had NO TTL, so once a pubkey was cached
// the entry was frozen for the process lifetime. Pubkeys are immutable, so this
// is a safety floor rather than a correctness bug, but the mandate is that no L1
// entry may live for the whole process lifetime. This test drives a downstream
// change through the REAL account cache + miniredis and asserts L1 caches within
// the TTL and refreshes once accountCacheL1TTL elapses.
func TestAccountCache_L1RefreshesAfterTTL(t *testing.T) {
	client := newTestRedis(t)
	pkA := pubKeyFromByte(0x11)
	pkB := pubKeyFromByte(0x22)
	fq := &frozenAccountQueryClient{pubKey: pkA}
	ac := NewAccountCache(logging.NewLoggerFromConfig(logging.DefaultConfig()), client, fq)

	ctx := context.Background()
	require.NoError(t, ac.Start(ctx))
	t.Cleanup(func() { _ = ac.Close() })

	// Use a large L1 TTL while we prove caching; restore the package default after.
	origTTL := accountCacheL1TTL
	accountCacheL1TTL = time.Hour
	t.Cleanup(func() { accountCacheL1TTL = origTTL })

	const addr = "pokt1testaddress"

	// Initial lazy load caches pkA in L1 (and L2).
	got, err := ac.Get(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, pkA.Bytes(), got.Bytes())

	// Downstream pubkey changes mid-flight. Reproduce the state a stale-L1 re-read
	// hits: L2 empty (delete the redis key) and the L3 query client now serving a
	// new value.
	fq.pubKey = pkB
	require.NoError(t, client.Del(ctx, client.KB().CacheKey(accountCacheType, addr)).Err())

	// Within the (huge) L1 TTL: Get must still serve the cached pkA, even though
	// the downstream value already changed. Proves L1 actually caches.
	got, err = ac.Get(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, pkA.Bytes(), got.Bytes(),
		"L1 must keep serving the cached pubkey while the entry is within accountCacheL1TTL")

	// Expire L1: the next Get must treat L1 as a miss, re-read downstream, and pick
	// up the new pubkey — the exact regression this test guards.
	accountCacheL1TTL = 0
	got, err = ac.Get(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, pkB.Bytes(), got.Bytes(),
		"after accountCacheL1TTL elapses, L1 must refresh and follow the downstream pubkey")
}
