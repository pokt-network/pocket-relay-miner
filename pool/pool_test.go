//go:build test

package pool

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- BackendEndpoint Tests ---

func TestNewBackendEndpoint(t *testing.T) {
	t.Run("starts healthy", func(t *testing.T) {
		ep, err := NewBackendEndpoint("", "http://node1:8545")
		require.NoError(t, err)
		require.True(t, ep.IsHealthy())
	})

	t.Run("name derived from URL hostname:port when not provided", func(t *testing.T) {
		ep, err := NewBackendEndpoint("", "http://node1:8545")
		require.NoError(t, err)
		require.Equal(t, "node1:8545", ep.Name)
	})

	t.Run("explicit name preserved when provided", func(t *testing.T) {
		ep, err := NewBackendEndpoint("primary", "http://node1:8545")
		require.NoError(t, err)
		require.Equal(t, "primary", ep.Name)
	})

	t.Run("URL parsed correctly", func(t *testing.T) {
		ep, err := NewBackendEndpoint("", "http://node1:8545/path")
		require.NoError(t, err)
		require.Equal(t, "http://node1:8545/path", ep.RawURL)
		require.Equal(t, "node1:8545", ep.URL.Host)
	})

	t.Run("invalid URL returns error", func(t *testing.T) {
		_, err := NewBackendEndpoint("", "://invalid")
		require.Error(t, err)
	})

	t.Run("empty URL returns error", func(t *testing.T) {
		_, err := NewBackendEndpoint("", "")
		require.Error(t, err)
	})

	t.Run("scheme-less host:port URL accepted (gRPC style)", func(t *testing.T) {
		ep, err := NewBackendEndpoint("", "backend:50051")
		require.NoError(t, err)
		require.Equal(t, "backend:50051", ep.URL.Host)
		require.Equal(t, "backend:50051", ep.Name)
		require.Equal(t, "backend:50051", ep.RawURL)
	})

	t.Run("scheme-less host:port with path accepted", func(t *testing.T) {
		ep, err := NewBackendEndpoint("grpc-backend", "backend:50051/service")
		require.NoError(t, err)
		require.Equal(t, "backend:50051", ep.URL.Host)
		require.Equal(t, "grpc-backend", ep.Name)
	})
}

func TestBackendEndpointHealth(t *testing.T) {
	t.Run("SetHealthy and SetUnhealthy toggle state", func(t *testing.T) {
		ep, err := NewBackendEndpoint("test", "http://node:8545")
		require.NoError(t, err)
		require.True(t, ep.IsHealthy())

		ep.SetUnhealthy()
		require.False(t, ep.IsHealthy())

		ep.SetHealthy()
		require.True(t, ep.IsHealthy())
	})

	t.Run("IncrementFailures and ResetFailures", func(t *testing.T) {
		ep, err := NewBackendEndpoint("test", "http://node:8545")
		require.NoError(t, err)

		require.Equal(t, int32(0), ep.ConsecutiveFailures())

		count := ep.IncrementFailures()
		require.Equal(t, int32(1), count)
		require.Equal(t, int32(1), ep.ConsecutiveFailures())

		ep.IncrementFailures()
		ep.IncrementFailures()
		require.Equal(t, int32(3), ep.ConsecutiveFailures())

		ep.ResetFailures()
		require.Equal(t, int32(0), ep.ConsecutiveFailures())
	})

	t.Run("SetLastCheck and LastCheck", func(t *testing.T) {
		ep, err := NewBackendEndpoint("test", "http://node:8545")
		require.NoError(t, err)

		now := time.Now()
		ep.SetLastCheck(now)
		got := ep.LastCheck()
		require.Equal(t, now.UnixNano(), got.UnixNano())
	})
}

func TestBackendEndpointConcurrency(t *testing.T) {
	ep, err := NewBackendEndpoint("test", "http://node:8545")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				ep.SetHealthy()
			} else {
				ep.SetUnhealthy()
			}
			_ = ep.IsHealthy()
			ep.IncrementFailures()
			_ = ep.ConsecutiveFailures()
			ep.ResetFailures()
			ep.SetLastCheck(time.Now())
			_ = ep.LastCheck()
		}(i)
	}
	wg.Wait()
	// No assertion needed - test passes if no race detected with -race flag
}

// --- Pool Tests ---

func TestNewPool(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")

	p := NewPool("test-pool", []*BackendEndpoint{ep1, ep2}, &FirstHealthySelector{}, "first_healthy(test)")

	require.Equal(t, 2, p.Len())
	require.Equal(t, "test-pool", p.PoolName())
	require.Len(t, p.All(), 2)

	// All() returns a copy, not the original slice
	all := p.All()
	all[0] = nil
	require.NotNil(t, p.All()[0], "All() must return a copy")
}

func TestPoolNext_AllHealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")

	p := NewPool("test", []*BackendEndpoint{ep1, ep2}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.Next()
	require.NotNil(t, got)
}

func TestPoolNext_SomeUnhealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep1.SetUnhealthy()

	p := NewPool("test", []*BackendEndpoint{ep1, ep2}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.Next()
	require.NotNil(t, got)
	require.Equal(t, "b", got.Name)
}

func TestPoolNext_AllUnhealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep1.SetUnhealthy()
	ep2.SetUnhealthy()

	p := NewPool("test", []*BackendEndpoint{ep1, ep2}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.Next()
	require.Nil(t, got)
}

func TestPoolNext_SingleEndpoint(t *testing.T) {
	ep, _ := NewBackendEndpoint("single", "http://node1:8545")

	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.Next()
	require.NotNil(t, got)
	require.Equal(t, "single", got.Name)
}

func TestPoolHealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep1.SetUnhealthy()

	p := NewPool("test", []*BackendEndpoint{ep1, ep2}, &FirstHealthySelector{}, "first_healthy(test)")

	healthy := p.Healthy()
	require.Len(t, healthy, 1)
	require.Equal(t, "b", healthy[0].Name)
}

// --- FirstHealthySelector Tests ---

func TestFirstHealthySelector(t *testing.T) {
	sel := &FirstHealthySelector{}

	t.Run("returns index of first healthy endpoint", func(t *testing.T) {
		ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
		ep2, _ := NewBackendEndpoint("b", "http://node2:8545")

		idx := sel.Select([]*BackendEndpoint{ep1, ep2})
		require.Equal(t, 0, idx)
	})

	t.Run("skips unhealthy endpoints", func(t *testing.T) {
		ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
		ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
		ep1.SetUnhealthy()

		idx := sel.Select([]*BackendEndpoint{ep1, ep2})
		require.Equal(t, 1, idx)
	})

	t.Run("returns -1 when none healthy", func(t *testing.T) {
		ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
		ep1.SetUnhealthy()

		idx := sel.Select([]*BackendEndpoint{ep1})
		require.Equal(t, -1, idx)
	})

	t.Run("returns -1 for empty slice", func(t *testing.T) {
		idx := sel.Select(nil)
		require.Equal(t, -1, idx)
	})
}

// --- RoundRobinSelector Tests ---

func TestRoundRobinSelector_EvenDistribution(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	endpoints := []*BackendEndpoint{ep1, ep2, ep3}

	sel := &RoundRobinSelector{}
	counts := make(map[int]int)

	for i := 0; i < 300; i++ {
		idx := sel.Select(endpoints)
		require.GreaterOrEqual(t, idx, 0)
		counts[idx]++
	}

	require.Equal(t, 100, counts[0], "endpoint 0 should get exactly 100 calls")
	require.Equal(t, 100, counts[1], "endpoint 1 should get exactly 100 calls")
	require.Equal(t, 100, counts[2], "endpoint 2 should get exactly 100 calls")
}

func TestRoundRobinSelector_SkipsUnhealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	ep2.SetUnhealthy() // middle endpoint unhealthy
	endpoints := []*BackendEndpoint{ep1, ep2, ep3}

	sel := &RoundRobinSelector{}
	for i := 0; i < 100; i++ {
		idx := sel.Select(endpoints)
		require.NotEqual(t, 1, idx, "should never return unhealthy endpoint index 1")
		require.NotEqual(t, -1, idx, "should always find a healthy endpoint")
	}
}

func TestRoundRobinSelector_AllUnhealthy(t *testing.T) {
	ep, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep.SetUnhealthy()

	sel := &RoundRobinSelector{}
	idx := sel.Select([]*BackendEndpoint{ep})
	require.Equal(t, -1, idx)
}

func TestRoundRobinSelector_EmptyEndpoints(t *testing.T) {
	sel := &RoundRobinSelector{}

	require.Equal(t, -1, sel.Select(nil))
	require.Equal(t, -1, sel.Select([]*BackendEndpoint{}))
}

func TestRoundRobinSelector_SingleEndpoint(t *testing.T) {
	ep, _ := NewBackendEndpoint("single", "http://node1:8545")
	endpoints := []*BackendEndpoint{ep}

	sel := &RoundRobinSelector{}
	for i := 0; i < 10; i++ {
		idx := sel.Select(endpoints)
		require.Equal(t, 0, idx)
	}
}

func TestRoundRobinSelector_Concurrent(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	endpoints := []*BackendEndpoint{ep1, ep2, ep3}

	sel := &RoundRobinSelector{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				idx := sel.Select(endpoints)
				if idx < 0 || idx >= len(endpoints) {
					t.Errorf("invalid index: %d", idx)
				}
			}
		}()
	}
	wg.Wait()
	// No assertion needed beyond race detector - test passes if no race detected
}

func BenchmarkRoundRobinSelector_Select(b *testing.B) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	ep4, _ := NewBackendEndpoint("d", "http://node4:8545")
	ep5, _ := NewBackendEndpoint("e", "http://node5:8545")
	endpoints := []*BackendEndpoint{ep1, ep2, ep3, ep4, ep5}

	sel := &RoundRobinSelector{}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sel.Select(endpoints)
		}
	})
}

// --- Pool StrategyLabel Tests ---

func TestPoolStrategyLabel(t *testing.T) {
	ep, _ := NewBackendEndpoint("a", "http://node1:8545")

	t.Run("returns strategy label", func(t *testing.T) {
		p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "first_healthy(auto)")
		require.Equal(t, "first_healthy(auto)", p.StrategyLabel())
	})

	t.Run("round_robin label", func(t *testing.T) {
		p := NewPool("test", []*BackendEndpoint{ep}, &RoundRobinSelector{}, "round_robin(explicit)")
		require.Equal(t, "round_robin(explicit)", p.StrategyLabel())
	})
}

// --- HasHealthy Tests ---

func TestHasHealthy_AllHealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")

	p := NewPool("test", []*BackendEndpoint{ep1, ep2, ep3}, &FirstHealthySelector{}, "first_healthy(test)")
	require.True(t, p.HasHealthy())
}

func TestHasHealthy_SomeUnhealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	ep1.SetUnhealthy()
	ep2.SetUnhealthy()

	p := NewPool("test", []*BackendEndpoint{ep1, ep2, ep3}, &FirstHealthySelector{}, "first_healthy(test)")
	require.True(t, p.HasHealthy())
}

func TestHasHealthy_AllUnhealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	ep1.SetUnhealthy()
	ep2.SetUnhealthy()
	ep3.SetUnhealthy()

	p := NewPool("test", []*BackendEndpoint{ep1, ep2, ep3}, &FirstHealthySelector{}, "first_healthy(test)")
	require.False(t, p.HasHealthy())
}

func TestHasHealthy_SingleEndpoint(t *testing.T) {
	ep, _ := NewBackendEndpoint("single", "http://node1:8545")

	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "first_healthy(test)")
	require.True(t, p.HasHealthy())

	ep.SetUnhealthy()
	require.False(t, p.HasHealthy())
}

// --- NextExcluding Tests ---

func TestNextExcluding_SkipsExcluded(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")

	p := NewPool("test", []*BackendEndpoint{ep1, ep2, ep3}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.NextExcluding(ep1)
	require.NotNil(t, got)
	require.NotEqual(t, "a", got.Name, "NextExcluding should never return the excluded endpoint")
}

func TestNextExcluding_NoAlternate(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")

	p := NewPool("test", []*BackendEndpoint{ep1}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.NextExcluding(ep1)
	require.Nil(t, got, "NextExcluding should return nil when only endpoint is excluded")
}

func TestNextExcluding_AllOthersUnhealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	ep2.SetUnhealthy()
	ep3.SetUnhealthy()

	p := NewPool("test", []*BackendEndpoint{ep1, ep2, ep3}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.NextExcluding(ep1)
	require.Nil(t, got, "NextExcluding should return nil when all others are unhealthy")
}

// --- IsRetryable Tests ---

func TestIsRetryable_ConnectionRefused(t *testing.T) {
	// Connection refused is retryable -- another backend may be available
	err := &net.OpError{Op: "dial", Err: &net.DNSError{IsNotFound: false}}
	require.True(t, IsRetryable(0, fmt.Errorf("connection refused: %w", err)))
}

func TestIsRetryable_5xx(t *testing.T) {
	// 5xx status codes are retryable (backend errors may be transient)
	require.True(t, IsRetryable(500, nil), "500 should be retryable")
	require.True(t, IsRetryable(502, nil), "502 should be retryable")
	require.True(t, IsRetryable(503, nil), "503 should be retryable")
}

func TestIsRetryable_Timeout(t *testing.T) {
	// Timeouts are NOT retryable -- shared timeout budget is likely exhausted
	require.False(t, IsRetryable(0, context.DeadlineExceeded), "deadline exceeded should not be retryable")
}

func TestIsRetryable_4xx(t *testing.T) {
	// Client errors are NOT retryable -- retry won't help
	require.False(t, IsRetryable(400, nil), "400 should not be retryable")
	require.False(t, IsRetryable(404, nil), "404 should not be retryable")
}

func TestIsRetryable_Success(t *testing.T) {
	// Successful responses are NOT retryable
	require.False(t, IsRetryable(200, nil), "200 should not be retryable")
}

func TestIsRetryable_DNSError(t *testing.T) {
	// DNS resolution failure is retryable -- different backend may resolve
	dnsErr := &net.DNSError{Err: "no such host", Name: "backend1.example.com", IsNotFound: true}
	require.True(t, IsRetryable(0, dnsErr), "DNS error should be retryable")
}

func TestNextExcluding_SkipsUnhealthy(t *testing.T) {
	ep1, _ := NewBackendEndpoint("a", "http://node1:8545")
	ep2, _ := NewBackendEndpoint("b", "http://node2:8545")
	ep3, _ := NewBackendEndpoint("c", "http://node3:8545")
	ep2.SetUnhealthy()

	p := NewPool("test", []*BackendEndpoint{ep1, ep2, ep3}, &FirstHealthySelector{}, "first_healthy(test)")

	got := p.NextExcluding(ep1)
	require.NotNil(t, got)
	require.Equal(t, "c", got.Name, "NextExcluding should skip unhealthy ep2 and return ep3")
}
