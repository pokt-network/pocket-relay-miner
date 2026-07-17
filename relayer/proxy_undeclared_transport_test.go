//go:build test

package relayer

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
)

// newWarnProxy builds a ProxyServer with just the pieces warnUndeclaredTransport
// touches: a logger and the dedup map.
func newWarnProxy() *ProxyServer {
	return &ProxyServer{
		logger:                    testLogger(),
		warnedUndeclaredTransport: xsync.NewMap[string, struct{}](),
	}
}

func stateWithEndpoints(eps ...cache.StakedEndpoint) *cache.SupplierState {
	return &cache.SupplierState{
		Status:          cache.SupplierStatusActive,
		Staked:          true,
		OperatorAddress: gateTestSupplier,
		StakedEndpoints: eps,
	}
}

func TestWarnUndeclaredTransport_UndeclaredIncrementsMetricAndWarnsOnce(t *testing.T) {
	p := newWarnProxy()
	before := testutil.ToFloat64(undeclaredTransportServed.WithLabelValues("eth", "rest"))

	// eth staked over jsonrpc only; a rest relay is undeclared.
	state := stateWithEndpoints(cache.StakedEndpoint{ServiceID: "eth", RpcType: "jsonrpc"})

	p.warnUndeclaredTransport(state, gateTestSupplier, "eth", "rest")
	p.warnUndeclaredTransport(state, gateTestSupplier, "eth", "rest")

	// Metric counts EVERY occurrence (2), the warn log is deduped (1 map entry).
	got := testutil.ToFloat64(undeclaredTransportServed.WithLabelValues("eth", "rest"))
	require.InDelta(t, before+2, got, 0.0001, "metric must count every undeclared serve")
	require.Equal(t, 1, p.warnedUndeclaredTransport.Size(), "warn deduped to one tuple")
}

func TestWarnUndeclaredTransport_DeclaredIsSilent(t *testing.T) {
	p := newWarnProxy()
	before := testutil.ToFloat64(undeclaredTransportServed.WithLabelValues("eth", "jsonrpc"))

	state := stateWithEndpoints(cache.StakedEndpoint{ServiceID: "eth", RpcType: "jsonrpc"})
	p.warnUndeclaredTransport(state, gateTestSupplier, "eth", "jsonrpc")

	got := testutil.ToFloat64(undeclaredTransportServed.WithLabelValues("eth", "jsonrpc"))
	require.InDelta(t, before, got, 0.0001, "a declared transport must not increment")
	require.Equal(t, 0, p.warnedUndeclaredTransport.Size())
}

func TestWarnUndeclaredTransport_FailOpenOnEmptyEndpoints(t *testing.T) {
	p := newWarnProxy()
	before := testutil.ToFloat64(undeclaredTransportServed.WithLabelValues("eth", "grpc"))

	// Old miner: no StakedEndpoints published → fail-open, never warn.
	state := &cache.SupplierState{Status: cache.SupplierStatusActive, Staked: true, Services: []string{"eth"}}
	p.warnUndeclaredTransport(state, gateTestSupplier, "eth", "grpc")

	got := testutil.ToFloat64(undeclaredTransportServed.WithLabelValues("eth", "grpc"))
	require.InDelta(t, before, got, 0.0001, "empty StakedEndpoints must fail open (no warn)")
	require.Equal(t, 0, p.warnedUndeclaredTransport.Size())
}

func TestWarnUndeclaredTransport_NilStateNoPanic(t *testing.T) {
	p := newWarnProxy()
	// Optimistic boot: nil state must not panic and must not warn.
	require.NotPanics(t, func() {
		p.warnUndeclaredTransport(nil, gateTestSupplier, "eth", "grpc")
	})
	require.Equal(t, 0, p.warnedUndeclaredTransport.Size())
}

func TestWarnUndeclaredTransport_DistinctTuplesWarnSeparately(t *testing.T) {
	p := newWarnProxy()
	state := stateWithEndpoints(cache.StakedEndpoint{ServiceID: "eth", RpcType: "jsonrpc"})

	p.warnUndeclaredTransport(state, gateTestSupplier, "eth", "rest")
	p.warnUndeclaredTransport(state, gateTestSupplier, "eth", "websocket")
	p.warnUndeclaredTransport(state, "pokt1other", "eth", "rest")

	require.Equal(t, 3, p.warnedUndeclaredTransport.Size(),
		"distinct (supplier,service,transport) tuples each warn once")
}
