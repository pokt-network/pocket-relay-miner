//go:build test

package miner

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// firstRebalanceAfter runs only the claimer's rebalance loop and returns how
// long its first rebalance took to come.
func firstRebalanceAfter(t *testing.T, cfg SupplierClaimerConfig) time.Duration {
	t.Helper()
	client, _ := newTestRedis(t)
	c := NewSupplierClaimer(zerolog.Nop(), client, "instance-rebalance-offset", cfg)
	first := make(chan time.Time, 1)
	c.logger = zerolog.New(io.Discard).Hook(zerolog.HookFunc(func(_ *zerolog.Event, _ zerolog.Level, msg string) {
		if msg == "rebalance check" {
			select {
			case first <- time.Now():
			default:
			}
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx, c.cancelFn = ctx, cancel
	start := time.Now()
	c.wg.Add(1)
	go c.rebalanceLoop()
	t.Cleanup(func() { cancel(); c.wg.Wait() })
	select {
	case at := <-first:
		return at.Sub(start)
	case <-time.After(30 * time.Second):
		t.Fatal("no rebalance came")
		return 0
	}
}

// TestRebalance_TheFirstTickIsHalfARenewalOutOfStep: the renewal and the
// rebalance loops start together, and with the default rebalance interval a
// multiple of the renewal rate every rebalance landed on a renewal pass.
// Only a lower bound is asserted: the wait is at least that long by
// construction.
func TestRebalance_TheFirstTickIsHalfARenewalOutOfStep(t *testing.T) {
	cfg := SupplierClaimerConfig{RenewRate: 2 * time.Second, RebalanceInterval: 20 * time.Millisecond}
	require.GreaterOrEqual(t, firstRebalanceAfter(t, cfg), cfg.RenewRate/2,
		"the first rebalance must wait half a renewal period")
}

// TestRebalance_ASmallRenewalRateDelaysTheFirstTickLittle is the inverse
// control: the offset is the configured renewal rate's, so with a small one
// the first rebalance comes about when its interval says.
func TestRebalance_ASmallRenewalRateDelaysTheFirstTickLittle(t *testing.T) {
	cfg := SupplierClaimerConfig{RenewRate: 20 * time.Millisecond, RebalanceInterval: 20 * time.Millisecond}
	require.Less(t, firstRebalanceAfter(t, cfg), 2*time.Second,
		"the offset must follow the configured renewal rate, not hold the first rebalance back further")
}

// TestRecentReleaseCooldown_FollowsTheConfiguredRebalanceInterval: the
// cooldown was twice the DEFAULT interval, so a miner configured with a longer
// one re-claimed a supplier before its peers had a tick to take it.
func TestRecentReleaseCooldown_FollowsTheConfiguredRebalanceInterval(t *testing.T) {
	client, _ := newTestRedis(t)
	for _, tc := range []struct {
		name     string
		interval time.Duration
		ago      time.Duration
		cooling  bool
	}{
		{"a longer interval keeps it cooling longer", 60 * time.Second, 90 * time.Second, true},
		{"a shorter interval lets it go sooner", time.Second, 2500 * time.Millisecond, false},
		{"control: the default interval", RebalanceInterval, 50 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewSupplierClaimer(zerolog.Nop(), client, "instance-cooldown", SupplierClaimerConfig{RebalanceInterval: tc.interval})
			c.recentlyReleasedMu.Lock()
			c.recentlyReleased["pokt1cooldown"] = time.Now().Add(-tc.ago)
			c.recentlyReleasedMu.Unlock()
			require.Equal(t, tc.cooling, c.inRecentReleaseCooldown("pokt1cooldown"))
		})
	}
}
