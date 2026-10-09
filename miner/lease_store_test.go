//go:build test

package miner

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// eachLeaseStore runs body against the shared Redis lease store and the
// exclusive one.
func eachLeaseStore(t *testing.T, body func(t *testing.T, s leaseStore)) {
	t.Run("redis", func(t *testing.T) {
		client, _ := newTestRedis(t)
		body(t, &redisLeaseStore{client: client})
	})
	t.Run("exclusive", func(t *testing.T) { body(t, newExclusiveLeaseStore()) })
}

func TestLeaseStore_ALeaseHasOneOwner(t *testing.T) {
	eachLeaseStore(t, func(t *testing.T, s leaseStore) {
		ctx := context.Background()
		_, err := s.owner(ctx, "sup")
		require.ErrorIs(t, err, errLeaseAbsent)
		renewed, err := s.expire(ctx, "sup", time.Minute)
		require.NoError(t, err)
		require.False(t, renewed, "no lease to renew")

		took, err := s.setNX(ctx, "sup", "a", time.Minute)
		require.NoError(t, err)
		require.True(t, took)
		took, err = s.setNX(ctx, "sup", "b", time.Minute)
		require.NoError(t, err)
		require.False(t, took, "held by a")
		owner, err := s.owner(ctx, "sup")
		require.NoError(t, err)
		require.Equal(t, "a", owner)
		exists, err := s.exists(ctx, "sup")
		require.NoError(t, err)
		require.True(t, exists)

		released, err := s.releaseIfOwner(ctx, "sup", "b")
		require.NoError(t, err)
		require.False(t, released, "b does not hold it")
		require.NoError(t, s.extendIfOwner(ctx, "sup", "b", time.Second))
		owner, err = s.owner(ctx, "sup")
		require.NoError(t, err)
		require.Equal(t, "a", owner, "still a's")

		released, err = s.releaseIfOwner(ctx, "sup", "a")
		require.NoError(t, err)
		require.True(t, released)
		exists, err = s.exists(ctx, "sup")
		require.NoError(t, err)
		require.False(t, exists)
	})
}

func TestLeaseStore_TheActiveSet(t *testing.T) {
	eachLeaseStore(t, func(t *testing.T, s leaseStore) {
		ctx := context.Background()
		require.NoError(t, s.setInstance(ctx, "i1", time.Minute))
		require.NoError(t, s.addActive(ctx, "i1"))
		require.NoError(t, s.addActive(ctx, "i2")) // in the set, never alive

		n, err := s.activeCount(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(2), n)
		members, err := s.activeMembers(ctx)
		require.NoError(t, err)
		sort.Strings(members)
		require.Equal(t, []string{"i1", "i2"}, members)
		alive, err := s.instanceAlive(ctx, "i1")
		require.NoError(t, err)
		require.True(t, alive)
		alive, err = s.instanceAlive(ctx, "i2")
		require.NoError(t, err)
		require.False(t, alive)

		s.removeActive(ctx, "i2")
		s.deleteInstance(ctx, "i1")
		n, err = s.activeCount(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(1), n)
		alive, err = s.instanceAlive(ctx, "i1")
		require.NoError(t, err)
		require.False(t, alive)
	})
}

// A standalone process takes every supplier when its claimer starts, with the
// same claimer as over Redis: alone in the active set, its fair share is all.
// A restarted process (a new store, a new identity) does too, at once.
func TestSupplierClaimer_OverTheExclusiveStoreTakesEverySupplier(t *testing.T) {
	suppliers := make([]string, 7)
	for i := range suppliers {
		suppliers[i] = fmt.Sprintf("pokt1sup%d", i)
	}
	for run, instance := range []string{"standalone-pid1", "standalone-pid2"} {
		c := newSupplierClaimer(zerolog.Nop(), newExclusiveLeaseStore(), instance, SupplierClaimerConfig{})
		var mu sync.Mutex
		started := map[string]bool{}
		c.SetCallbacks(func(_ context.Context, supplier string) error {
			mu.Lock()
			defer mu.Unlock()
			started[supplier] = true
			return nil
		}, func(context.Context, string, string) error { return nil })

		require.NoError(t, c.Start(context.Background(), suppliers))
		claimed := c.ClaimedSuppliers()
		sort.Strings(claimed)
		require.Equal(t, suppliers, claimed, "run %d", run)
		mu.Lock()
		require.Len(t, started, len(suppliers), "every supplier started, run %d", run)
		mu.Unlock()
		c.StopLoops()
		c.FinishShutdown(context.Background())
	}
}
