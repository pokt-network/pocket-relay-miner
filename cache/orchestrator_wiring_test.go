//go:build test

package cache

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	pond "github.com/alitto/pond/v2"
	"github.com/pokt-network/poktroll/pkg/client"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// This file is the wiring smoke test the CUPR incident demanded: it wires the
// REAL CacheOrchestrator + REAL cache-pkg entity caches + miniredis + a fake
// chain, drives a real block event through the leader's refresh worker, and
// asserts the full propagation chain end-to-end:
//
//	Redis known-set populated  →  leader RefreshEntity(force)  →  L2 written
//	→  pub/sub invalidation published  →  a SECOND (relayer-simulating) cache's
//	L1 follows the on-chain change.
//
// Unit tests with fake caches passed during the CUPR incident because they hid
// exactly this wiring. The negative-control test below proves the Redis
// known-set is load-bearing: an entity absent from it is NEVER force-refreshed —
// the precise break that froze the relayer on a stale CUPR.

// ---- race-safe fake chain clients (the worker mutates these concurrently) ----

// smokeServiceClient is a race-safe ServiceQueryClient that models the query
// layer's frozen in-process cache: it keeps serving a value until
// InvalidateService is called, then re-reads the (possibly changed) chain value.
type smokeServiceClient struct {
	mu              sync.Mutex
	chainCUPR       uint64
	served          *uint64
	invalidateCalls int
}

func (c *smokeServiceClient) GetService(_ context.Context, serviceID string) (*sharedtypes.Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.served == nil {
		v := c.chainCUPR
		c.served = &v
	}
	return &sharedtypes.Service{Id: serviceID, ComputeUnitsPerRelay: *c.served}, nil
}

func (c *smokeServiceClient) InvalidateService(_ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateCalls++
	c.served = nil
}

func (c *smokeServiceClient) setChainCUPR(v uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chainCUPR = v
}

func (c *smokeServiceClient) invalidations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invalidateCalls
}

// smokeAppClient is the application analogue of smokeServiceClient.
type smokeAppClient struct {
	mu              sync.Mutex
	chainDelegatees []string
	served          *[]string
	invalidateCalls int
}

func (c *smokeAppClient) GetApplication(_ context.Context, address string) (*apptypes.Application, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.served == nil {
		v := append([]string(nil), c.chainDelegatees...)
		c.served = &v
	}
	return &apptypes.Application{
		Address:                   address,
		DelegateeGatewayAddresses: append([]string(nil), *c.served...),
	}, nil
}

func (c *smokeAppClient) InvalidateApplication(_ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateCalls++
	c.served = nil
}

func (c *smokeAppClient) setChainDelegatees(d []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chainDelegatees = append([]string(nil), d...)
}

func (c *smokeAppClient) invalidations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invalidateCalls
}

// smokeSupplierParamsClient satisfies client.SupplierQueryClient (embed) and only
// answers GetParams — enough for the supplier-params refresher to not error.
type smokeSupplierParamsClient struct {
	client.SupplierQueryClient
}

func (s *smokeSupplierParamsClient) GetParams(_ context.Context) (*suppliertypes.Params, error) {
	p := suppliertypes.DefaultParams()
	return &p, nil
}

// smokeProofClient answers the proof module GetParams for the proof-params refresher.
type smokeProofClient struct{}

func (s *smokeProofClient) GetParams(_ context.Context) (*prooftypes.Params, error) {
	p := prooftypes.DefaultParams()
	return &p, nil
}

// controllableBlockSubscriber is a test BlockHeightSubscriber whose
// PublishBlockHeight drives the leader's refresh worker on demand.
type controllableBlockSubscriber struct {
	ch chan BlockEvent
}

func newControllableBlockSubscriber() *controllableBlockSubscriber {
	return &controllableBlockSubscriber{ch: make(chan BlockEvent, 16)}
}

func (s *controllableBlockSubscriber) Subscribe(_ context.Context) <-chan BlockEvent { return s.ch }
func (s *controllableBlockSubscriber) PublishBlockHeight(_ context.Context, e BlockEvent) error {
	s.ch <- e
	return nil
}
func (s *controllableBlockSubscriber) Start(_ context.Context) error { return nil }
func (s *controllableBlockSubscriber) Close() error                  { return nil }

// ---- harness ----

type smokeHarness struct {
	orch       *CacheOrchestrator
	svcClient  *smokeServiceClient
	appClient  *smokeAppClient
	subscriber *controllableBlockSubscriber
	leaderSvc  KeyedEntityCache[string, *sharedtypes.Service]
	relayerSvc KeyedEntityCache[string, *sharedtypes.Service]
}

// newSmokeOrchestrator wires the full orchestrator with real caches + miniredis +
// fake chain clients. It does NOT start the leader elector; the test drives
// leadership deterministically via onBecameLeader (no heartbeat timing).
func newSmokeOrchestrator(t *testing.T, svcCUPR uint64, appDelegatees []string) *smokeHarness {
	t.Helper()
	log := logging.NewLoggerFromConfig(logging.DefaultConfig())
	redisClient := newTestRedis(t)

	svcClient := &smokeServiceClient{chainCUPR: svcCUPR}
	appClient := &smokeAppClient{chainDelegatees: appDelegatees}

	leaderSvc := NewServiceCache(log, redisClient, svcClient)
	appCache := NewApplicationCache(log, redisClient, appClient)
	sharedCache := NewSharedParamsCache(log, redisClient, &stubSharedQueryClient{}, 10)
	proofCache := NewProofParamsCache(log, redisClient, &smokeProofClient{}, &stubSharedQueryClient{}, 10)
	supplierParams := NewRedisSupplierParamCache(log, redisClient, &smokeSupplierParamsClient{}, CacheConfig{})
	supplierCache := NewSupplierCache(log, redisClient, SupplierCacheConfig{})
	sessionCache := NewRedisSessionCache(log, redisClient,
		&frozenSessionQueryClient{frozenID: "s", endHeight: 110},
		&stubSharedQueryClient{}, &stubBlockClient{height: 100}, CacheConfig{})

	// A second service cache instance simulates the RELAYER: it has no
	// orchestrator and follows only via pub/sub + its own L1 TTL.
	relayerSvc := NewServiceCache(log, redisClient, &smokeServiceClient{chainCUPR: svcCUPR})

	ctx := context.Background()
	for _, c := range []interface{ Start(context.Context) error }{leaderSvc, appCache, sharedCache, proofCache, supplierParams, relayerSvc} {
		require.NoError(t, c.Start(ctx))
	}
	t.Cleanup(func() {
		for _, c := range []interface{ Close() error }{leaderSvc, appCache, sharedCache, proofCache, supplierParams, relayerSvc} {
			_ = c.Close()
		}
	})

	subscriber := newControllableBlockSubscriber()
	elector := leader.NewGlobalLeaderElectorWithConfig(log, redisClient, "smoke-instance",
		leader.GlobalLeaderElectorConfig{LeaderTTL: 30 * time.Second, HeartbeatRate: time.Second})

	orch := NewCacheOrchestrator(
		log,
		CacheOrchestratorConfig{RefreshIntervalBlocks: 1},
		elector,
		subscriber,
		redisClient,
		sharedCache,
		proofCache,
		supplierParams,
		appCache,
		leaderSvc,
		supplierCache,
		sessionCache,
		// Mirror NewCacheOrchestrator's own master-pool sizing assumption
		// (numCPU*8) so its 15% refresh subpool fits.
		pond.NewPool(runtime.NumCPU()*8),
	)

	return &smokeHarness{
		orch:       orch,
		svcClient:  svcClient,
		appClient:  appClient,
		subscriber: subscriber,
		leaderSvc:  leaderSvc,
		relayerSvc: relayerSvc,
	}
}

// becomeLeaderAndStart starts the orchestrator and deterministically drives it
// into the leader path (block subscription + refresh worker) without depending on
// the elector's heartbeat timing.
func (h *smokeHarness) becomeLeaderAndStart(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.orch.Start(ctx))
	t.Cleanup(func() { _ = h.orch.Close() })
	// Elector is unstarted (IsLeader()==false), so Start did not auto-enter the
	// leader path. Enter it explicitly — this is the real leader entrypoint.
	h.orch.onBecameLeader(h.orch.ctx)
}

func seedKnown(t *testing.T, h *smokeHarness, entityType string, ids ...string) {
	t.Helper()
	ctx := context.Background()
	key := h.orch.redisClient.KB().CacheKnownKey(entityType)
	require.NoError(t, h.orch.redisClient.SAdd(ctx, key, ids).Err())
}

// TestOrchestrator_BlockEvent_RefreshesDiscoveredService_FullChain is the
// positive smoke test: a service + application discovered via the Redis
// known-set (as recordDiscovered populates it from relay traffic on any replica)
// is force-refreshed by the leader on a block event, written to L2, the
// invalidation is published, and a separate relayer-simulating cache's L1
// follows the new value. This is the chain that was broken in the CUPR incident.
func TestOrchestrator_BlockEvent_RefreshesDiscoveredService_FullChain(t *testing.T) {
	const svcID = "develop-http"
	const appAddr = "pokt1app"
	ctx := context.Background()

	h := newSmokeOrchestrator(t, 100, []string{"gw-old"})

	// Discovery: another replica saw relays for this service/app and SAdded them.
	seedKnown(t, h, "services", svcID)
	seedKnown(t, h, "applications", appAddr)

	// Prime both the leader and relayer L1 caches with the OLD value.
	svc, err := h.leaderSvc.Get(ctx, svcID)
	require.NoError(t, err)
	require.Equal(t, uint64(100), svc.GetComputeUnitsPerRelay())
	rsvc, err := h.relayerSvc.Get(ctx, svcID)
	require.NoError(t, err)
	require.Equal(t, uint64(100), rsvc.GetComputeUnitsPerRelay())

	// On-chain change mid-session.
	h.svcClient.setChainCUPR(200)
	h.appClient.setChainDelegatees([]string{"gw-old", "gw-new"})

	// Drive a real block event through the leader refresh worker.
	h.becomeLeaderAndStart(t)
	require.NoError(t, h.subscriber.PublishBlockHeight(ctx, BlockEvent{Height: 1000, Timestamp: time.Unix(1000, 0)}))

	// CONSUMER-SIDE assertions (not a Redis key): the leader force-refreshed the
	// service (InvalidateService called) and its L1 now serves the new CUPR.
	require.Eventually(t, func() bool {
		return h.svcClient.invalidations() >= 1
	}, 20*time.Second, 20*time.Millisecond, "leader must force-refresh the known service")

	require.Eventually(t, func() bool {
		s, e := h.leaderSvc.Get(ctx, svcID)
		return e == nil && s.GetComputeUnitsPerRelay() == 200
	}, 20*time.Second, 20*time.Millisecond, "leader L1 must follow the on-chain CUPR change")

	// The application path uses the identical known-set + RefreshEntity wiring.
	require.Eventually(t, func() bool {
		return h.appClient.invalidations() >= 1
	}, 20*time.Second, 20*time.Millisecond, "leader must force-refresh the known application")

	// Cross-process: the relayer-simulating cache received the pub/sub
	// invalidation and its L1 followed to the new value (read through L2).
	require.Eventually(t, func() bool {
		s, e := h.relayerSvc.Get(ctx, svcID)
		return e == nil && s.GetComputeUnitsPerRelay() == 200
	}, 20*time.Second, 20*time.Millisecond,
		"relayer-side cache L1 must follow via pub/sub invalidation (the CUPR-incident gap)")
}

// TestOrchestrator_KnownSetIsLoadBearing_NegativeControl proves the Redis
// known-set is the load-bearing gate. With ONLY the application seeded (service
// absent), a block event refreshes the application but NEVER the service — the
// exact shape of the CUPR break (empty known:services → relayer frozen). A naive
// Redis-key test would not catch this; this one does.
func TestOrchestrator_KnownSetIsLoadBearing_NegativeControl(t *testing.T) {
	const svcID = "develop-http"
	const appAddr = "pokt1app"
	ctx := context.Background()

	h := newSmokeOrchestrator(t, 100, []string{"gw-old"})

	// Seed ONLY the application known-set; the service is deliberately NOT discovered.
	seedKnown(t, h, "applications", appAddr)

	// Prime the leader L1 with the OLD service value.
	svc, err := h.leaderSvc.Get(ctx, svcID)
	require.NoError(t, err)
	require.Equal(t, uint64(100), svc.GetComputeUnitsPerRelay())

	// On-chain change for both.
	h.svcClient.setChainCUPR(200)
	h.appClient.setChainDelegatees([]string{"gw-old", "gw-new"})

	h.becomeLeaderAndStart(t)
	require.NoError(t, h.subscriber.PublishBlockHeight(ctx, BlockEvent{Height: 1000, Timestamp: time.Unix(1000, 0)}))

	// The application IS in the known-set → it gets refreshed. Waiting on this
	// guarantees the refresh cycle has run.
	require.Eventually(t, func() bool {
		return h.appClient.invalidations() >= 1
	}, 20*time.Second, 20*time.Millisecond, "application in the known-set must be refreshed")

	// The service is NOT in the known-set → the refresh cycle (which has now run,
	// per the app assertion) never touched it. Its query client was never
	// invalidated, so the leader is stuck on the old CUPR. This is the CUPR bug,
	// reproduced as a guard: if discovery/known-set wiring regresses, this stays 0.
	require.Equal(t, 0, h.svcClient.invalidations(),
		"a service absent from the known-set must NOT be force-refreshed (CUPR-break shape)")
}
