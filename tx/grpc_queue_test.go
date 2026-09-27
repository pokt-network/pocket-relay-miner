//go:build test

package tx

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

// blockingAuthServer answers Account only once every caller has arrived, so a
// test can hold N RPCs open at the same time without a sleep.
type blockingAuthServer struct {
	authtypes.UnimplementedQueryServer
	arrived chan struct{}
	release chan struct{}
}

func (s *blockingAuthServer) Account(
	ctx context.Context,
	req *authtypes.QueryAccountRequest,
) (*authtypes.QueryAccountResponse, error) {
	select {
	case s.arrived <- struct{}{}:
	default:
	}
	<-s.release
	return &authtypes.QueryAccountResponse{}, nil
}

// TestStreamQueueIsMeasuredWhenTheCeilingIsHit is the whole point of the
// handler: prove the wait is observable at all.
//
// The ceiling cannot be reached in the live gate -- the axis is SUPPLIERS, not
// RPS, and localnet runs 15 of them, so 400 RPS never approaches 100 concurrent
// streams. It CAN be manufactured here, because grpc.MaxConcurrentStreams(1)
// makes the server actually emit SETTINGS_MAX_CONCURRENT_STREAMS (the server
// omits that frame only when its limit is math.MaxUint32), which drops the
// client's stream quota to 1. So the second RPC parks in
// checkForStreamQuota until the first finishes.
//
// Net: the ceiling is proved at level 2 or it is not proved.
func TestStreamQueueIsMeasuredWhenTheCeilingIsHit(t *testing.T) {
	srv := &blockingAuthServer{
		arrived: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	// ONE concurrent stream: the ceiling, made small enough to reach.
	gs := grpc.NewServer(grpc.MaxConcurrentStreams(1))
	authtypes.RegisterQueryServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	const connLabel = "queue-test"
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(observability.NewGRPCStreamQueueStats(connLabel)),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	before := queueObservations(t, connLabel)

	q := authtypes.NewQueryClient(conn)
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// First RPC takes the only stream and stays there.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = q.Account(ctx, &authtypes.QueryAccountRequest{Address: "a"})
	}()
	<-srv.arrived // it is in the handler, holding the stream

	// Second RPC has nowhere to go: it parks on the stream quota. Nothing in
	// grpc-go reports that -- this is the only signal it produces.
	second := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(second)
		_, _ = q.Account(ctx, &authtypes.QueryAccountRequest{Address: "b"})
	}()
	<-second

	close(srv.release)
	wg.Wait()

	after := queueObservations(t, connLabel)
	require.Greaterf(t, after, before+1,
		"both RPCs must be observed: got %v new observations. If the handler is "+
			"not wired, the wait a caller spends parked on the stream quota is "+
			"invisible -- no error, no log, nothing exported", after-before)
}

// TestStreamQueueSeriesExistsBeforeAnyTraffic: an absent histogram and a
// histogram that recorded nothing look identical on a dashboard, and the gate
// has to be able to tell "no queueing" from "not measured".
func TestStreamQueueSeriesExistsBeforeAnyTraffic(t *testing.T) {
	const connLabel = "queue-preexist"
	observability.NewGRPCStreamQueueStats(connLabel)

	families, err := observability.SharedRegistry.Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if f.GetName() != "ha_grpc_stream_queue_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "conn" && l.GetValue() == connLabel {
					found = true
				}
			}
		}
	}
	require.True(t, found,
		"the child series must exist as soon as the connection does: otherwise "+
			"\"nothing queued\" and \"nobody is measuring\" are the same reading")
}

// queueObservations returns how many samples the histogram holds for conn.
func queueObservations(t *testing.T, conn string) uint64 {
	t.Helper()
	families, err := observability.SharedRegistry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "ha_grpc_stream_queue_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "conn" && l.GetValue() == conn {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// compile-time: the handler must satisfy the interface grpc.WithStatsHandler
// wants, so a signature drift breaks the build instead of silently detaching
// the only observer this path has.
var _ stats.Handler = (*observability.GRPCStreamQueueStats)(nil)
var _ = prometheus.Labels{}
var _ = testutil.CollectAndCount

// countingAuthServer records how many Account calls are in the handler at once
// and holds them all until the test lets go.
type countingAuthServer struct {
	authtypes.UnimplementedQueryServer
	inFlight chan string
	release  chan struct{}
}

func (s *countingAuthServer) Account(
	ctx context.Context,
	req *authtypes.QueryAccountRequest,
) (*authtypes.QueryAccountResponse, error) {
	s.inFlight <- req.Address
	<-s.release
	any, err := codectypes.NewAnyWithValue(&authtypes.BaseAccount{Address: req.Address})
	if err != nil {
		return nil, err
	}
	return &authtypes.QueryAccountResponse{Account: any}, nil
}

// TestColdAccountLookupsForDifferentSuppliersRunInParallel is RED against the
// tree before this commit.
//
// getAccount used to take the write lock and `defer` its release across the
// Account RPC, so on a cold cache -- the normal state after a restart or a
// rebalance handoff -- every supplier's first signature queued behind every
// other supplier's, for addresses that share nothing but a map.
//
// It matters beyond its own latency: it sits IN FRONT of the connection on the
// signing path, so any measurement of stream concurrency taken with it in place
// measures this mutex first and reports it as the connection's ceiling.
func TestColdAccountLookupsForDifferentSuppliersRunInParallel(t *testing.T) {
	srv := &countingAuthServer{
		inFlight: make(chan string, 2),
		release:  make(chan struct{}),
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	gs := grpc.NewServer()
	authtypes.RegisterQueryServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	cdc, _ := createCodecAndTxConfig()
	tc := &TxClient{
		authQuerier:  authtypes.NewQueryClient(conn),
		accountCache: make(map[string]*authtypes.BaseAccount),
		codec:        cdc,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, addr := range []string{"pokt1aaa", "pokt1bbb"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tc.getAccount(ctx, addr)
		}()
	}

	// BOTH must reach the server before either is allowed to finish. Under the
	// old shape the second cannot: it is blocked on accountCacheMu, which the
	// first holds until its RPC returns -- and its RPC cannot return until this
	// test releases it, which it never does. The deadline is what fails.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case a := <-srv.inFlight:
			seen[a] = true
		case <-ctx.Done():
			close(srv.release)
			wg.Wait()
			t.Fatalf("only %d of 2 cold account lookups reached the chain: the "+
				"account cache mutex is held across the RPC, so suppliers that "+
				"share nothing but this map sign one at a time", len(seen))
		}
	}
	close(srv.release)
	wg.Wait()

	require.Len(t, seen, 2, "both addresses must have been queried")
}
