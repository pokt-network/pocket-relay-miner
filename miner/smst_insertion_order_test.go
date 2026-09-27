//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestTheSessionRootDoesNotDependOnInsertionOrder: the relayer writes a
// supplier's relays from several workers at once, so the miner receives the
// same relays in an order that depends on delivery timing. The sealed root is
// what the claim commits to, so the same relays must seal the same root in any
// order.
func TestTheSessionRootDoesNotDependOnInsertionOrder(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: "pokt1order_supplier",
		CacheTTL:        0,
	})

	type leaf struct {
		key, value []byte
		weight     uint64
	}
	const n = 64
	leaves := make([]leaf, n)
	for i := range leaves {
		key := sha256.Sum256([]byte(fmt.Sprintf("relay-%d", i)))
		leaves[i] = leaf{key: key[:], value: []byte(fmt.Sprintf("relay-bytes-%d", i)), weight: uint64(i%7 + 1)}
	}

	insert := func(sessionID string, order []int) []byte {
		t.Helper()
		for _, i := range order {
			require.NoError(t, mgr.UpdateTree(ctx, sessionID, leaves[i].key, leaves[i].value, leaves[i].weight))
		}
		root, err := mgr.FlushTree(ctx, sessionID)
		require.NoError(t, err)
		require.NotEmpty(t, root)
		return root
	}

	forward := make([]int, n)
	reversed := make([]int, n)
	for i := range forward {
		forward[i] = i
		reversed[i] = n - 1 - i
	}
	shuffled := rand.New(rand.NewSource(7)).Perm(n)

	rootForward := insert("sess-order-forward", forward)
	require.Equal(t, rootForward, insert("sess-order-shuffled", shuffled), "a shuffled insertion must seal the same root")
	require.Equal(t, rootForward, insert("sess-order-reversed", reversed), "a reversed insertion must seal the same root")

	require.NotEqual(t, rootForward, insert("sess-order-one-short", forward[:n-1]),
		"control: a different set of relays seals a different root, so the comparison above is not vacuous")
}
