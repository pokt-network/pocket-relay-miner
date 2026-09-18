package miner

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	stdhttp "net/http"
	"runtime"
	"sync"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/cometbft/cometbft/rpc/client/http"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"github.com/pokt-network/pocket-relay-miner/transport/grpcconn"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/pokt-network/pocket-relay-miner/tx"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
)

// SupplierWorkerConfig contains configuration for supplier processing.
// This runs on ALL miners (not just leader) when distributed claiming is enabled.
type SupplierWorkerConfig struct {
	// Core dependencies
	Logger      logging.Logger
	RedisClient *redistransport.Client
	KeyManager  keys.KeyManager
	Config      *Config

	// StoreHealth says whether Redis can take writes; see SupplierManagerConfig.
	StoreHealth *redistransport.StoreHealth

	// Blockchain connection config
	QueryNodeRPCUrl  string
	QueryNodeGRPCUrl string
	GRPCInsecure     bool
	ChainID          string
}

// SupplierWorker manages supplier processing for all miners.
// Unlike LeaderController, this runs on ALL miners when distributed claiming is enabled.
// Each miner claims a subset of suppliers and processes relays for those suppliers.
type SupplierWorker struct {
	logger logging.Logger
	config SupplierWorkerConfig

	// Resources created at startup
	queryClients            *query.Clients
	redisBlockSubscriber    *cache.RedisBlockSubscriber
	redisBlockClientAdapter *cache.RedisBlockClientAdapter
	supplierCache           *cache.SupplierCache
	txClient                *tx.TxClient
	proofChecker            *ProofRequirementChecker
	supplierManager         *SupplierManager
	supplierRegistry        *SupplierRegistry
	masterPool              pond.Pool

	// discovered dedups app/service addresses already written to the Redis
	// known-sets, keeping the per-relay discovery write off the hot path after
	// first sight.
	discovered *xsync.Map[string, struct{}]

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	mu       sync.Mutex
	active   bool
}

// NewSupplierWorker creates a new supplier worker.
func NewSupplierWorker(config SupplierWorkerConfig) *SupplierWorker {
	return &SupplierWorker{
		logger:     logging.ForComponent(config.Logger, "supplier_worker"),
		config:     config,
		discovered: xsync.NewMap[string, struct{}](),
	}
}

// recordDiscovered persists app/service addresses seen in relay traffic to the
// shared Redis known-sets so the leader's CacheOrchestrator refreshes them. The
// orchestrator runs only on the leader, but any replica can own a supplier and
// see its apps, so every replica feeds the sets. A local dedup map keeps this
// off the hot path after each entity's first sighting. Best-effort.
func (w *SupplierWorker) recordDiscovered(ctx context.Context, appAddr, serviceID string) {
	rc := w.config.RedisClient
	if rc == nil || w.discovered == nil {
		return
	}
	if appAddr != "" {
		if _, seen := w.discovered.LoadOrStore("app:"+appAddr, struct{}{}); !seen {
			if err := rc.SAdd(ctx, rc.KB().CacheKnownKey("applications"), appAddr).Err(); err != nil {
				w.discovered.Delete("app:" + appAddr) // allow retry on next sighting
				w.logger.Debug().Err(err).Str("app", appAddr).Msg("failed to record discovered application")
			}
		}
	}
	if serviceID != "" {
		if _, seen := w.discovered.LoadOrStore("svc:"+serviceID, struct{}{}); !seen {
			if err := rc.SAdd(ctx, rc.KB().CacheKnownKey("services"), serviceID).Err(); err != nil {
				w.discovered.Delete("svc:" + serviceID) // allow retry on next sighting
				w.logger.Debug().Err(err).Str(logging.FieldServiceID, serviceID).Msg("failed to record discovered service")
			}
		}
	}
}

// blockTimeProvider is the object the transaction client anchors on, and the
// one the startup seed must reach. It is named once so the two cannot drift:
// the adapter keeps its own last block, and nothing reads the anchor from it.
func (w *SupplierWorker) blockTimeProvider() *cache.RedisBlockSubscriber {
	return w.redisBlockSubscriber
}

// seedChainState tells the process where the chain is before the first block
// event reaches it. In the process that wins the election the leader that
// publishes those events only starts after this returns, so without the seed
// the height is zero for as long as a block lasts -- and so is the time every
// unordered transaction is anchored on, which the chain then refuses.
//
// The height and the time go to different objects on purpose: the height to the
// block adapter, and the time to the provider the transaction client reads.
func (w *SupplierWorker) seedChainState(startHeight int64, startBlockTime time.Time) {
	w.redisBlockClientAdapter.SeedHeight(startHeight)
	w.blockTimeProvider().SeedBlockTime(startBlockTime)
}

// Start initializes and starts the supplier worker.
func (w *SupplierWorker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.active {
		return fmt.Errorf("supplier worker already active")
	}

	w.ctx, w.cancelFn = context.WithCancel(ctx)
	w.logger.Info().Msg("starting supplier worker")

	// Create master worker pool
	// Formula: max(cpu × cpu_multiplier, suppliers × workers_per_supplier) + overhead
	// This scales with both CPU count and supplier count for optimal parallelism
	numSuppliers := len(w.config.KeyManager.ListSuppliers())
	masterPoolSize := w.config.Config.GetMasterPoolSize(numSuppliers)
	w.masterPool = pond.NewPool(
		masterPoolSize,
		pond.WithQueueSize(pond.Unbounded),
		pond.WithNonBlocking(true),
	)
	w.logger.Info().
		Int("max_workers", masterPoolSize).
		Int("num_suppliers", numSuppliers).
		Int("num_cpu", runtime.NumCPU()).
		Msg("created master worker pool (auto-sized based on supplier count and CPU)")

	// Surface under-provisioning / discouraged config (batching off, too few CPU
	// for the supplier count, capped pool) as startup warnings — for operators
	// who deploy fast without reading the docs.
	w.config.Config.LogStartupCapacityAdvisory(w.logger, numSuppliers)

	// Start worker pool metrics ticker for Prometheus monitoring
	StartWorkerPoolMetricsTicker(w.ctx, w.logger, w.masterPool, "supplier_worker", masterPoolSize)

	// Where this process dials the full node, derived ONCE. This worker opens
	// two connections to the same node -- queries and transactions -- and
	// deriving UseTLS separately for each is how they end up disagreeing.
	nodeTarget := grpcconn.Target{
		Endpoint: w.config.QueryNodeGRPCUrl,
		UseTLS:   !w.config.GRPCInsecure,
	}

	// Create query clients
	var err error
	w.queryClients, err = query.NewQueryClients(
		w.logger,
		query.ClientConfig{
			GRPCEndpoint: nodeTarget.Endpoint,
			QueryTimeout: w.config.Config.GetQueryTimeout(),
			UseTLS:       nodeTarget.UseTLS,
		},
	)
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	w.logger.Info().
		Str("grpc_endpoint", w.config.QueryNodeGRPCUrl).
		Msg("query clients initialized")

	// The node's network, the chain's shared params and its committed height,
	// read synchronously and required: the miner does not start without all
	// three. The network must be the chain ID transactions are signed for. The
	// entry decides from the params and the height whether a relay arrives too
	// late for its session's claim, and the height is where the block adapter
	// starts, so the first relays are not judged against height 0 while the
	// first block event is still on its way. cache_ttl validation and the
	// shared-params advisory read this same result.
	sharedParams, startHeight, startBlockTime, err := readStartupChainState(w.ctx, w.config.ChainID, startupChainReaders{
		network: nodeNetworkReader(w.queryClients.GRPCConnection()),
		params:  w.queryClients.Shared().GetParams,
		height:  committedHeightReader(w.queryClients.GRPCConnection()),
	})
	if err != nil {
		w.cleanup()
		return err
	}
	if err := checkCacheTTL(w.config.Config, w.logger, sharedParams, nil); err != nil {
		w.cleanup()
		return err
	}
	LogSharedParamsAdvisory(w.logger, sharedParams)

	// Create RPC client for querying specific block heights (needed for proof generation)
	// This is CRITICAL - proof generation needs to query the exact block at a specific height
	// to get the canonical BlockID.Hash that matches what the validator stores
	var rpcClient *http.HTTP
	if w.config.QueryNodeRPCUrl != "" {
		if w.config.GRPCInsecure {
			rpcClient, err = http.New(w.config.QueryNodeRPCUrl, "/websocket")
		} else {
			tlsConfig := &tls.Config{
				MinVersion: tls.VersionTLS12,
			}
			httpClient := &stdhttp.Client{
				Transport: &stdhttp.Transport{
					TLSClientConfig: tlsConfig,
				},
			}
			rpcClient, err = http.NewWithClient(w.config.QueryNodeRPCUrl, "/websocket", httpClient)
		}
		if err != nil {
			w.cleanup()
			return fmt.Errorf("failed to create CometBFT RPC client: %w", err)
		}
		w.logger.Info().
			Str("rpc_endpoint", w.config.QueryNodeRPCUrl).
			Msg("created CometBFT RPC client for block queries")
	} else {
		w.logger.Warn().Msg("QueryNodeRPCUrl not configured - proof generation may fail")
	}

	// Create Redis block subscriber (subscribes to block events via Redis pub/sub)
	// This allows non-leader miners to receive block events published by the leader
	w.redisBlockSubscriber = cache.NewRedisBlockSubscriber(
		w.logger,
		w.config.RedisClient,
		nil, // No direct blockchain client - events come from leader via Redis
	)
	if err = w.redisBlockSubscriber.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start redis block subscriber: %w", err)
	}
	w.logger.Info().Msg("redis block subscriber started (receiving events from leader)")

	// Create Redis block client adapter to implement client.BlockClient interface
	// Pass the RPC client so it can query specific block heights for proof generation
	w.redisBlockClientAdapter = cache.NewRedisBlockClientAdapter(
		w.logger,
		w.redisBlockSubscriber,
		rpcClient, // For GetBlockAtHeight() - critical for proof generation
	)
	if err = w.redisBlockClientAdapter.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start redis block client adapter: %w", err)
	}
	w.logger.Info().Msg("redis block client adapter started")

	// Seeded here, before the supplier manager starts and so before any relay is
	// consumed. Not by waiting for a block event: in the process that wins the
	// election, the leader that publishes them only starts after this Start
	// returns.
	w.seedChainState(startHeight, startBlockTime)

	// NOTE: The worker does NOT build its own shared/session/proof param caches.
	// The economic paths read the raw query clients (GetParamsAtHeight for
	// session-bound reads; live GetParams for proof requirement), and the only
	// orchestrator-refreshed param caches that need to exist live on the leader
	// controller. Worker-local copies were dead duplicates.

	// Create supplier cache (for publishing supplier state). Constructed with
	// SupplierCacheConfig{} — no TTL set — so it starts on cache.
	// defaultSupplierCacheTTL immediately: HIGH-1's write path (SetSupplierState)
	// always writes a bounded TTL from the moment this cache exists, never TTL=0.
	w.supplierCache = cache.NewSupplierCache(
		w.logger,
		w.config.RedisClient,
		cache.SupplierCacheConfig{},
	)
	if err = w.supplierCache.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start supplier cache: %w", err)
	}

	// Refine that default to the chain's own session length
	// (cache.SupplierCacheTTLFromParams, num_blocks_per_session x
	// block_time_seconds — NOT the unbonding period; see that function's doc
	// comment for why) once shared params are available, via SetTTL.
	//
	// Dispatched off the master pool, same as the advisory above and for the
	// same reason: this used to be a synchronous GetParams() call here,
	// duplicating the advisory's own chain round trip and reintroducing the
	// exact "an unreachable node must not lengthen every miner start" problem
	// the advisory was written to avoid (review 2026-08-21). Nothing blocks
	// on this result — the cache already has a safe default the moment it
	// was constructed above — so there is no correctness reason for it to be
	// synchronous either.
	//
	// qcForTTL is captured HERE, before dispatch — same as sharedForAdvisory
	// above — not read as w.queryClients inside the closure. cleanup() sets
	// w.queryClients = nil before calling masterPool.Stop(), and pond's
	// Stop() does not wait for queued tasks to finish (StopAndWait() does;
	// this call site does not use it, see leader_controller.go:517 for the
	// blocking sibling). A task still queued when Start() fails a later step
	// and Close() runs would read w.queryClients.Shared() on a nil receiver
	// and panic — silently, since the pool's default panicRecovery swallows
	// it (review 2026-08-21, verified against pond/v2 v2.6.0's Stop()/Task
	// semantics). Reading the captured qcForTTL instead of the field closes
	// that window the same way the advisory block already does.
	qcForTTL := w.queryClients
	supplierCache := w.supplierCache
	if ttlErr := w.masterPool.Go(func() {
		w.refineSupplierCacheTTL(qcForTTL, supplierCache)
	}); ttlErr != nil {
		w.logger.Warn().Err(ttlErr).
			Msg("supplier cache TTL refinement skipped: worker pool would not accept the task")
	}

	// Get chain ID - required for transaction signing
	// ChainID comes from config (defaults to "pocket" if not explicitly set)
	chainID := w.config.ChainID
	w.logger.Info().Str("chain_id", chainID).Msg("using chain ID for transaction signing")

	// Create proof checker
	w.proofChecker = NewProofRequirementChecker(
		w.logger,
		w.queryClients.Proof(),
		w.queryClients.Shared(),
		w.queryClients.ServiceDifficulty(),
	)

	// Create tx client
	// Parse gas price from config
	gasPrice, err := cosmostypes.ParseDecCoin(w.config.Config.GetTxGasPrice())
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to parse gas price: %w", err)
	}

	w.txClient, err = tx.NewTxClient(
		w.logger,
		w.config.KeyManager,
		tx.TxClientConfig{
			// Its OWN connection, not the query one. Transactions and
			// queries share a per-connection ceiling of 100 concurrent
			// HTTP/2 streams, and past it grpc-go parks the caller with no
			// error and no log -- so a burst of queries could stall a claim
			// silently. They also have opposite shapes: queries never stop,
			// transactions only move inside windows.
			GRPCEndpoint:      nodeTarget.Endpoint,
			UseTLS:            nodeTarget.UseTLS,
			ConnProbeInterval: w.config.Config.GetTxConnProbeInterval(),
			MaxConcurrent:     w.config.Config.GetTxMaxConcurrent(),
			TxRPCTimeout:      w.config.Config.GetTxRPCTimeout(),
			ChainID:           chainID,
			GasLimit:          w.config.Config.GetTxGasLimit(),
			GasPrice:          gasPrice,
			GasAdjustment:     w.config.Config.GetTxGasAdjustment(),
			// Anchor tx timeoutTimestamp on the chain's latest_block_time.
			// The RedisBlockSubscriber is already wired and tracking
			// block events for every replica (leader and standby), so
			// passing it here gives the tx client the exact clock the
			// cosmos-sdk ante handler uses to validate unordered-tx
			// TTLs. Prevents `unordered tx ttl exceeds 10m0s` rejections
			// when the chain's block time lags wall clock. See
			// BlockTimeProvider in tx/tx_client.go for the full rationale.
			BlockTimeProvider: w.blockTimeProvider(),
		},
	)
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to create tx client: %w", err)
	}
	// grpc.NewClient is lazy, so nothing above proves the connection works.
	// Without this, a wrong endpoint or wrong credentials stays quiet until
	// the first claim -- which happens inside a closing window.
	if verifyErr := w.txClient.VerifyConn(ctx); verifyErr != nil {
		if errors.Is(verifyErr, tx.ErrTxConnMisconfigured) {
			w.cleanup()
			return fmt.Errorf("transaction connection is misconfigured: %w", verifyErr)
		}
		// Anything else is the node being unreachable right now. Starting
		// anyway is deliberate: a full node that is briefly down at miner
		// startup is not a reason to stay down with it, and the probe keeps
		// checking.
		w.logger.Warn().
			Err(verifyErr).
			Str("endpoint", nodeTarget.Endpoint).
			Msg("transaction connection unverified at startup; starting anyway and probing")
	}

	// Resolve, once and now, whether this node can answer the post-inclusion
	// read -- the query that says WHY a claim or proof is missing from the
	// chain rather than only that it is.
	//
	// It runs at startup and NOT lazily on the first failure, because a signal
	// that only appears once something has gone wrong is a signal the operator
	// meets during the incident it exists to explain. And it never blocks
	// startup, unlike VerifyConn above: without the transaction connection
	// nothing can be mined, while this read is optional by construction -- a
	// node without a transaction index costs its operator the CAUSE of a
	// verdict, not the ability to mine.
	readState := w.txClient.ProbeInclusionRead(ctx)
	SetInclusionReadState(readState)
	w.logger.Info().
		Str("state", string(readState)).
		Msg("post-inclusion read state resolved")

	w.logger.Info().Msg("transaction client initialized")

	// Create supplier registry
	w.supplierRegistry = NewSupplierRegistry(
		w.logger,
		w.config.RedisClient,
		SupplierRegistryConfig{
			IndexKey: w.config.RedisClient.KB().SuppliersRegistryIndexKey(),
		},
	)

	// Create supplier manager with distributed claiming enabled
	w.supplierManager = NewSupplierManager(
		w.logger,
		w.config.KeyManager,
		w.supplierRegistry,
		SupplierManagerConfig{
			RedisClient:                    w.config.RedisClient,
			StoreHealth:                    w.config.StoreHealth,
			ConsumerName:                   w.config.Config.Redis.ConsumerName,
			SessionTTL:                     w.config.Config.GetSessionTTL(), // Uses CacheTTL if not explicitly set
			CacheTTL:                       w.config.Config.GetCacheTTL(),
			SMSTLiveRootCheckpointInterval: w.config.Config.SMSTLiveRootCheckpointInterval,
			BatchSize:                      w.config.Config.BatchSize,
			ClaimIdleTimeout:               w.config.Config.GetClaimIdleTimeout(),
			RelayBatchFlushInterval:        w.config.Config.GetRelayBatchFlushInterval(),
			SupplierCache:                  w.supplierCache,
			MinerID:                        w.config.Config.Redis.ConsumerName,
			SupplierQueryClient:            w.queryClients.Supplier(),
			WorkerPool:                     w.masterPool,
			TxClient:                       w.txClient,
			BlockClient:                    w.redisBlockClientAdapter, // Use Redis pub/sub for block events
			SharedClient:                   w.queryClients.Shared(),
			SessionClient:                  w.queryClients.Session(),
			ProofChecker:                   w.proofChecker,
			ProofQueryClient:               w.queryClients.Proof(),
			InclusionReconcilerConfig:      w.config.Config.Transaction.InclusionReconcilerConfig(),
			ServiceClient:                  w.queryClients.Service(),
			SessionLifecycleConfig: SessionLifecycleConfig{
				CheckInterval:            0, // Event-driven via Redis pub/sub
				MaxConcurrentTransitions: w.config.Config.GetSessionLifecycleMaxConcurrentTransitions(),
			},
			ClaimerConfig:                    w.config.Config.GetSupplierClaimingConfig(),
			DisablePreProofClaimVerification: w.config.Config.Transaction.DisablePreProofClaimVerification,
			SubmissionTrackingTTL:            w.config.Config.GetSubmissionTrackingTTL(),
			QueryWorkers:                     w.config.Config.GetQueryWorkers(),
			SupplierReconcileInterval:        w.config.Config.GetSupplierReconcileInterval(),
			BlockTimeSeconds:                 w.config.Config.GetBlockTimeSeconds(),
		},
	)

	// Set relay handler
	w.supplierManager.SetRelayHandler(w.handleRelay)

	// Start supplier manager
	if err = w.supplierManager.Start(ctx); err != nil {
		w.cleanup()
		return fmt.Errorf("failed to start supplier manager: %w", err)
	}

	w.active = true
	w.logger.Info().Msg("supplier worker started - processing relays with distributed claiming")
	return nil
}

// handleRelay processes a relay message.
func (w *SupplierWorker) handleRelay(ctx context.Context, supplierAddr string, msg *transport.StreamMessage) error {
	// CRITICAL: Log that we're processing a relay
	w.logger.Debug().
		Str("supplier", supplierAddr).
		Str("session_id", msg.Message.SessionId).
		Str("service_id", msg.Message.ServiceId).
		Uint64("compute_units", msg.Message.ComputeUnitsPerRelay).
		Msg("handleRelay: processing relay message")

	state, ok := w.supplierManager.GetSupplierState(supplierAddr)
	if !ok {
		// Supplier state not found - this can happen during shutdown or if supplier was removed.
		// This is a permanent condition (retrying won't help), so ACK and discard the relay.
		w.logger.Debug().
			Str("supplier", supplierAddr).
			Str("session_id", msg.Message.SessionId).
			Msg("supplier state not found - discarding relay (supplier may have been removed)")
		RecordRelayRejected(supplierAddr, "supplier_not_found", msg.Message.ServiceId)
		return nil // ACK and discard
	}

	// Cut at the entry, before this relay touches Redis: a relay for a session
	// whose claim flush has stopped waiting, or whose claim window has closed,
	// is dropped from the observed height and the params at its session end
	// height alone, without the session read below. See claimWindowReached.
	if reason := w.supplierManager.claimWindowReached(ctx, msg.Message.SessionEndHeight); reason != "" {
		w.logger.Debug().
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Str("reason", reason).
			Int64("session_end_height", msg.Message.SessionEndHeight).
			Msg("LATE_RELAY: dropping relay - its session's claim no longer waits for it")
		RecordRelayRejected(supplierAddr, dropReason(reason, msg.IsReclaim), msg.Message.ServiceId)
		return w.ackWithBatch(ctx, state, msg)
	}

	// A relay of a session whose tree this process deleted -- the lifecycle does
	// when the session reaches a terminal state -- is dropped here, before it
	// touches Redis: processing on would start an empty tree under the deleted
	// keys. It is the drop the per-relay session read below used to make.
	if state.SMSTManager.SessionDeleted(msg.Message.SessionId) {
		w.logger.Debug().
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Msg("LATE_RELAY: dropping relay - session tree already deleted (session terminal)")
		RecordRelayRejected(supplierAddr, dropReason("session_sealed", msg.IsReclaim), msg.Message.ServiceId)
		return w.ackWithBatch(ctx, state, msg)
	}

	// Persist apps/services seen in relay traffic to the shared Redis known-sets
	// so the leader's CacheOrchestrator refreshes them (dedup'd; off the hot path
	// after first sight).
	w.recordDiscovered(ctx, msg.Message.ApplicationAddress, msg.Message.ServiceId)

	// The session read that stood here ran for every relay: one TYPE+HGETALL,
	// used to drop the relays of a terminal session and handed to EnsureSession.
	// It is gone. A relay of a session this process deleted is dropped before
	// the discovery write, a sealed or claimed tree refuses the relay in
	// UpdateTreeGen, the relay batch's script does not count relays of a
	// terminal session, and EnsureSession asks the store only until the session
	// is confirmed. The closed-window drop it held stays below, reading nothing.
	//
	// // Check session state BEFORE updating SMST. A store error falls through on
	// // purpose: it says nothing about the session, and dropping a relay on a
	// // Redis hiccup is the expensive direction.
	// //
	// // The read is kept for EnsureSession below, which would otherwise read the
	// // same session again. Only an ANSWERED read is kept: after a store error
	// // EnsureSession reads for itself, because "the read failed" is not "the
	// // session does not exist".
	// var sessionRead SessionRead
	// if state.SessionStore != nil {
	// 	snapshot, storeErr := state.SessionStore.Get(ctx, msg.Message.SessionId)
	// 	if storeErr == nil {
	// 		sessionRead = SessionRead{Snapshot: snapshot, Answered: true}
	// 	}
	//
	// 	// Hoisted because it is STORE-INDEPENDENT: the claim window is decided by
	// 	// the message's own session end height against the observed block height,
	// 	// and reads nothing the store could fail to answer. Until 2026-08-28 it
	// 	// sat inside the switch, so a transient Redis Get error fell through to
	// 	// the first case and skipped it -- admitting a relay whose window had
	// 	// definitively closed, which then created or extended a session the sweep
	// 	// could only carry to claim_window_closed and delete. The store error is
	// 	// still a reason to keep the relay when the window is OPEN; it is not a
	// 	// reason to stop knowing what the height already says.
	// 	windowClosed := state.SessionCoordinator != nil &&
	// 		state.SessionCoordinator.ClaimWindowClosed(msg.Message.SessionEndHeight)
	//
	// 	switch {
	// 	case storeErr != nil && !windowClosed:
	// 		// fall through and process the relay
	//
	// 	case snapshot != nil && snapshot.State.IsTerminal():
	// 		w.logger.Debug().
	// 			Str("session_id", msg.Message.SessionId).
	// 			Str("supplier", supplierAddr).
	// 			Str("session_state", string(snapshot.State)).
	// 			Msg("LATE_RELAY: dropping relay - session already in terminal state")
	// 		RecordRelayRejected(supplierAddr, dropReason("session_sealed", msg.IsReclaim), msg.Message.ServiceId)
	// 		return nil
	//
	// 	case windowClosed:
	// 		// The claim window has closed, so this relay cannot reach a claim
	// 		// whatever else is true. Deliberately NOT conditioned on the
	// 		// snapshot being absent: a session sitting in active or claiming
	// 		// past its window is not terminal yet -- the sweep has not reached
	// 		// it -- so the case above does not catch it, and without this the
	// 		// relay would be added to a tree nothing will ever claim.
	// 		//
	// 		// With no snapshot at all it is the same verdict for a different
	// 		// reason: processing on would CREATE a session the sweep could only
	// 		// carry to claim_window_closed and delete again.
	// 		//
	// 		// No money counter is booked here on purpose. This runs BEFORE the
	// 		// duplicate check, so a late copy of a relay that was already
	// 		// processed and paid would land in it; counting that as lost
	// 		// revenue would be wrong. relays_rejected says what happened
	// 		// without claiming the work went unpaid.
	// 		// Named sessionState, not state: `state` is the *SupplierState
	// 		// this function already holds.
	// 		sessionState := "absent"
	// 		if snapshot != nil {
	// 			sessionState = string(snapshot.State)
	// 		}
	// 		w.logger.Debug().
	// 			Str("session_id", msg.Message.SessionId).
	// 			Str("supplier", supplierAddr).
	// 			Str("session_state", sessionState).
	// 			Int64("session_end_height", msg.Message.SessionEndHeight).
	// 			Msg("LATE_RELAY: dropping relay - claim window already closed")
	// 		RecordRelayRejected(supplierAddr, dropReason("claim_window_closed", msg.IsReclaim), msg.Message.ServiceId)
	// 		return nil
	// 	}
	// }
	if state.SessionStore != nil && state.SessionCoordinator != nil &&
		state.SessionCoordinator.ClaimWindowClosed(msg.Message.SessionEndHeight) {
		// The claim window has closed, so this relay cannot reach a claim
		// whatever the session's state; processing on would add it to a tree
		// nothing will claim, or create a session the sweep could only carry
		// to claim_window_closed and delete again.
		w.logger.Debug().
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Int64("session_end_height", msg.Message.SessionEndHeight).
			Msg("LATE_RELAY: dropping relay - claim window already closed")
		RecordRelayRejected(supplierAddr, dropReason("claim_window_closed", msg.IsReclaim), msg.Message.ServiceId)
		return w.ackWithBatch(ctx, state, msg)
	}

	// Defensive recompute: if the publisher somehow shipped a MinedRelayMessage
	// with an empty RelayHash (e.g. a bug on any new transport path), recompute
	// it locally from RelayBytes before inserting into the SMST. An empty key
	// would otherwise collide with every other empty-keyed leaf and collapse
	// legitimate events into a single SMST entry, silently losing claims.
	if len(msg.Message.RelayHash) == 0 && len(msg.Message.RelayBytes) > 0 {
		recomputed := protocol.GetRelayHashFromBytes(msg.Message.RelayBytes)
		msg.Message.RelayHash = recomputed[:]
		w.logger.Warn().
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Str("service_id", msg.Message.ServiceId).
			Msg("MinedRelayMessage arrived with empty RelayHash; recomputed from RelayBytes (publisher bug upstream)")
	}

	// Early duplicate check on reclaims only, as an optimization: a reclaimed
	// message is likely to have been processed already, and detecting it here
	// skips the SMST work. This check is NOT the correctness gate — that is
	// the MarkProcessed result below, which covers the case the reclaim cannot:
	// the ORIGINAL copy still buffered in a slow-but-alive consumer, processed
	// with IsReclaim=false after another consumer processed the reclaim.
	// Create the session if it does not exist yet, BEFORE any path that can
	// return early.
	//
	// This has to sit above the reclaim guard below, not below it. The whole
	// scenario is: consumer A updates the SMST and marks the hash, then dies
	// before the ACK; B reclaims the message with IsReclaim=true, finds the
	// hash already marked, and drops it. That drop is correct for the relay —
	// it is already in the tree — but it used to take the session creation
	// with it, and if that relay was the session's first, nothing ever claimed
	// the tree. Unpaid work, on the one path the fix exists for.
	//
	// Running it is safe: OnSessionCreated goes through CreateIfAbsent, a
	// first-write-wins gate, so it cannot double-create. It runs until the store
	// has answered that the session exists; that answer is then kept on the
	// session's tree (ConfirmSession, after UpdateTreeGen) and later deliveries
	// skip the round trip. Only a positive answer is kept: a session that could
	// not be read or created is asked for again by the next delivery. The
	// answer goes with the tree, which DeleteTree and the corruption eviction
	// drop. NOT verified: that a session's snapshot cannot expire while its tree
	// is resident. Its TTL is refreshed by every counted relay; a session that
	// only receives duplicates for longer than that TTL was not tested.
	sessionConfirmed := state.SMSTManager.SessionConfirmed(msg.Message.SessionId)
	if !sessionConfirmed {
		sessionConfirmed = state.SessionCoordinator.EnsureSession(
			ctx,
			SessionRead{},
			msg.Message.SessionId,
			msg.Message.SupplierOperatorAddress,
			msg.Message.ServiceId,
			msg.Message.ApplicationAddress,
			msg.Message.SessionStartHeight,
			msg.Message.SessionEndHeight,
		)
	}

	if msg.IsReclaim {
		if dedup := w.supplierManager.Deduplicator(); dedup != nil && len(msg.Message.RelayHash) > 0 {
			isDup, dupErr := dedup.IsDuplicate(ctx, msg.Message.RelayHash, msg.Message.SessionId)
			if dupErr != nil {
				// Fail-open: log and continue. Better to risk a rare double-count than drop a valid relay.
				w.logger.Debug().
					Err(dupErr).
					Str("session_id", msg.Message.SessionId).
					Msg("deduplicator check failed, proceeding with reclaimed relay")
			} else if isDup {
				w.logger.Debug().
					Str("session_id", msg.Message.SessionId).
					Str("supplier", supplierAddr).
					Msg("dropping reclaimed relay (already processed by previous consumer)")
				RecordRelayRejected(supplierAddr, "duplicate", msg.Message.ServiceId)
				return w.ackWithBatch(ctx, state, msg)
			}
		}
	}

	// Update SMST with relay bytes. The generation of the tree it went into
	// goes with the relay to the batch (see relayBatch.flushSession).
	gen, err := state.SMSTManager.UpdateTreeGen(
		ctx,
		msg.Message.SessionId,
		msg.Message.RelayHash,
		msg.Message.RelayBytes,
		msg.Message.ComputeUnitsPerRelay,
	)
	if err != nil {
		// Shutdown-origin cancellations: ACK-and-discard. The worker is
		// about to exit and will not process a retry; the stream message
		// is re-delivered by XREADGROUP to the next consumer after
		// restart, so no relay is lost. Classifying this as retryable
		// would leave the message permanently pending in XPENDING.
		if IsShutdownCancelError(err) || IsShutdownCancelError(ctx.Err()) {
			w.logger.Debug().
				Err(err).
				Str("session_id", msg.Message.SessionId).
				Str("supplier", supplierAddr).
				Msg("discarding relay on shutdown cancel (message will be redelivered on restart)")
			RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, "shutdown_cancel")
			return nil // ACK and discard
		}
		// IMPORTANT: Check retryable BEFORE permanent. When FlushPipeline fails with
		// OOM, the error is wrapped with ErrSMSTCommitFailed (permanent sentinel) but
		// the underlying cause is transient (OOM clears when keys expire). Checking
		// retryable first ensures transient errors are retried even when wrapped.
		if IsRetryableError(err) {
			RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, "transient_error")
			return fmt.Errorf("transient SMST error (will retry): %w", err)
		}
		// Check for permanent SMST errors (late relays, sealed/claimed sessions)
		if IsPermanentSMSTError(err) {
			w.logger.Debug().
				Err(err).
				Str("session_id", msg.Message.SessionId).
				Str("supplier", supplierAddr).
				Msg("dropping relay - permanent SMST error (session sealed/claimed)")
			RecordRelayRejected(supplierAddr, dropReason("session_sealed", msg.IsReclaim), msg.Message.ServiceId)
			RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, "session_sealed")
			return w.ackWithBatch(ctx, state, msg) // ACK and discard - no point retrying
		}
		// Unknown/unexpected errors - log and discard (don't retry forever)
		w.logger.Warn().
			Err(err).
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Msg("unexpected SMST error - discarding relay")
		RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, "unexpected_error")
		return w.ackWithBatch(ctx, state, msg) // ACK and discard - unknown errors shouldn't block processing
	}
	if sessionConfirmed {
		state.SMSTManager.ConfirmSession(msg.Message.SessionId)
	}

	// Track relay successfully added to SMST
	RecordRelayAddedToSMST(supplierAddr, msg.Message.ServiceId)

	session := relaySessionOf(msg.Message)
	relayHash := msg.Message.RelayHash
	computeUnits := msg.Message.ComputeUnitsPerRelay

	// MEMORY OPTIMIZATION: Clear RelayBytes and RelayHash after SMST update
	// The SMST has copied the data to Redis - these fields are no longer needed.
	// This allows GC to reclaim the memory early instead of holding until message is ACK'd.
	msg.Message.RelayBytes = nil
	msg.Message.RelayHash = nil

	// The relay is in the tree. What is left -- dedup mark, counters, stream
	// acknowledgement -- goes to the supplier's batch, which does it for the
	// whole session in one script (see relayBatch). A relay with no hash has
	// nothing to deduplicate by and is finished here, as is any relay the batch
	// refuses.
	if state.relayBatch != nil && len(relayHash) > 0 &&
		state.relayBatch.Add(ctx, session, batchedRelay{id: msg.ID, hash: bytes.Clone(relayHash), computeUnits: computeUnits, gen: gen}) {
		return ErrRelayBatched
	}

	// Not batched: this call's return acknowledges the relay, and an acknowledged
	// relay is never delivered again, so its nodes go to Redis first -- the
	// relay batch commits before it acknowledges for the same reason.
	// Not stored, the relay is handed back rather than acknowledged, as the relay
	// batch keeps a batch whose checkpoint fails: the redelivery puts it in the
	// tree again and commits again. It is counted as a transient failure, which
	// is what a retry is. A shutdown-origin cancellation keeps the rule the
	// UpdateTreeGen error above applies to it: acknowledged and discarded.
	resident, err := state.SMSTManager.CommitTree(ctx, session.sessionID)
	if err != nil {
		if IsShutdownCancelError(err) || IsShutdownCancelError(ctx.Err()) {
			w.logger.Debug().
				Err(err).
				Str("session_id", session.sessionID).
				Str("supplier", supplierAddr).
				Msg("discarding relay on shutdown cancel (message will be redelivered on restart)")
			RecordRelayFailedSMST(supplierAddr, session.serviceID, "shutdown_cancel")
			return nil // ACK and discard
		}
		RecordRelayFailedSMST(supplierAddr, session.serviceID, "transient_error")
		return fmt.Errorf("session %s: storing the relay's nodes before its acknowledgement failed, handing the relay back: %w", session.sessionID, err)
	}
	if !resident {
		// Evicted after corruption since UpdateTreeGen: the nodes went with the
		// tree. Handed back, the redelivery puts the relay in the next tree.
		return fmt.Errorf("session %s: tree evicted before the relay's nodes were stored, handing the relay back", session.sessionID)
	}

	countRelayOnce(ctx, w.logger, w.supplierManager.Deduplicator(), state.SessionCoordinator, supplierAddr, session, relayHash, computeUnits)
	return nil // ACK
}

// ackWithBatch finishes a relay that is acknowledged without being counted -- a
// rejection -- through the relay batch, which acknowledges rejections together
// when it flushes instead of one XACKDEL per relay. Without a batch, or when the
// batch will not take the entry, the relay is acknowledged by this call's
// return, as before.
func (w *SupplierWorker) ackWithBatch(ctx context.Context, state *SupplierState, msg *transport.StreamMessage) error {
	if state.relayBatch != nil && state.relayBatch.AddAck(ctx, msg.ID) {
		return ErrRelayBatched
	}
	return nil
}

// dropReason names why a relay was dropped as unpayable -- its tree sealed, its
// claim window closed -- and says whether the copy dropped was a REDELIVERY.
// The difference is who can be blamed: a relay that reaches the miner late the
// first time was late from its client, and the drop accounts for it. A
// redelivered copy was delivered before, to a consumer that did not finish it:
// either the relay is already in the tree, and nothing is missing, or it was
// lost in a handoff, and the drop must not explain the loss away. The live gate
// excuses a missing relay only by the reason without the suffix. Bounded: two
// reasons, two values each.
func dropReason(reason string, redelivered bool) string {
	if redelivered {
		return reason + "_redelivered"
	}
	return reason
}

// countRelayOnce is how a relay is finished one at a time: mark it processed in
// the deduplicator and, only on its FIRST processing, increment the session
// counters. The caller acknowledges it afterwards. handleRelay uses it for a
// relay the batch does not take, and the batch for its per-relay fallback.
//
// The mark runs on every relay (not only reclaims) because if the consumer
// crashes after the SMST update but before the stream ACK, the next consumer
// will reclaim the message and needs the dedup set to recognize it as already
// processed. The SADD result is the correctness gate for OnRelayProcessed: the
// reclaim skips entries this consumer owns and takes only those idle past the
// timeout, but idleness cannot distinguish a dead consumer from a slow-but-alive
// one — so the duplicate can be the ORIGINAL copy arriving with IsReclaim=false
// after another consumer already processed the reclaimed one, and this is the
// only place that catches that ordering.
//
// Ordering matters: the mark runs BEFORE the counters so that a crash between
// them leaves the counter under-counted rather than over-counted. Neither
// direction moves money -- the claim and its economic viability come from the
// SMST root -- but the counter feeds the relay metrics and the claim-time
// comparison of leaves against relays counted, and an over-count there would
// hide real loss.
//
// ACK on a counter failure: SMST + dedup are the sources of truth for the claim,
// and returning an error would leave the entry pending for the reclaim, whose
// dedup check rejects the increment anyway (unless the set already expired).
func countRelayOnce(
	ctx context.Context,
	logger logging.Logger,
	dedup Deduplicator,
	coordinator *SessionCoordinator,
	supplierAddr string,
	s relaySession,
	relayHash []byte,
	computeUnits uint64,
) {
	if dedup != nil && len(relayHash) > 0 {
		added, markErr := dedup.MarkProcessed(ctx, relayHash, s.sessionID)
		switch {
		case markErr != nil:
			// Fail-open: count the relay anyway. Better to risk a rare
			// double-count during Redis degradation than to drop billing
			// for a valid relay.
			logger.Debug().
				Err(markErr).
				Str("session_id", s.sessionID).
				Msg("deduplicator mark_processed failed")
		case !added:
			logger.Debug().
				Str("session_id", s.sessionID).
				Str("supplier", supplierAddr).
				Msg("relay already marked processed - skipping session counter increment")
			RecordRelayRejected(supplierAddr, "duplicate", s.serviceID)
			return // SMST already holds the relay; counters already incremented once
		}
	}

	if err := coordinator.OnRelayProcessed(
		ctx,
		s.sessionID,
		computeUnits,
		s.supplier,
		s.serviceID,
		s.application,
		s.startHeight,
		s.endHeight,
	); err != nil {
		logger.Debug().
			Err(err).
			Str("session_id", s.sessionID).
			Str("supplier", supplierAddr).
			Msg("session coordinator update failed — ACKing relay (SMST and dedup already committed)")
	}
}

// refineSupplierCacheTTL fetches shared params via qc and, on success, sets
// supplierCache's TTL from them.
//
// qc and supplierCache are explicit parameters, not w.queryClients /
// w.supplierCache, BY DESIGN: this runs on the master pool, dispatched from
// Start() before it returns, and cleanup() can nil w.queryClients out from
// under a still-queued task (see the capture comment at the Start() call
// site). Reading only the parameters makes that race impossible to
// reintroduce here without changing this signature.
func (w *SupplierWorker) refineSupplierCacheTTL(qc *query.Clients, supplierCache *cache.SupplierCache) {
	ttlCtx, cancelTTL := context.WithTimeout(w.ctx, sharedParamsAdvisoryTimeout)
	defer cancelTTL()

	sharedParamsForTTL, sharedErr := qc.Shared().GetParams(ttlCtx)
	if sharedErr != nil {
		w.logger.Warn().Err(sharedErr).
			Msg("could not read shared params for supplier cache TTL; keeping the default")
		return
	}
	supplierCache.SetTTL(cache.SupplierCacheTTLFromParams(sharedParamsForTTL, w.config.Config.GetBlockTimeSeconds()))
}

// Close shuts down the supplier worker.
func (w *SupplierWorker) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.active {
		return nil
	}

	w.logger.Info().Msg("stopping supplier worker")

	if w.cancelFn != nil {
		w.cancelFn()
	}

	w.cleanup()
	w.active = false
	w.logger.Info().Msg("supplier worker stopped")
	return nil
}

// cleanup closes all resources.
func (w *SupplierWorker) cleanup() {
	if w.supplierManager != nil {
		if err := w.supplierManager.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close supplier manager")
		}
		w.supplierManager = nil
	}

	if w.txClient != nil {
		if err := w.txClient.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close tx client")
		}
		w.txClient = nil
	}

	if w.supplierCache != nil {
		if err := w.supplierCache.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close supplier cache")
		}
		w.supplierCache = nil
	}

	if w.redisBlockClientAdapter != nil {
		w.redisBlockClientAdapter.Close() // Close() doesn't return error
		w.redisBlockClientAdapter = nil
	}

	if w.redisBlockSubscriber != nil {
		err := w.redisBlockSubscriber.Close()
		if err != nil {
			w.logger.Error().Err(err).Msg("failed to close redis block subscriber")
		} else {
			w.redisBlockSubscriber = nil
		}
	}

	if w.queryClients != nil {
		if err := w.queryClients.Close(); err != nil {
			w.logger.Error().Err(err).Msg("failed to close query clients")
		}
		w.queryClients = nil
	}

	if w.masterPool != nil {
		w.masterPool.Stop()
		w.masterPool = nil
	}
}

// GetSupplierManager returns the supplier manager for external access.
// GetSupplierCache returns the worker's supplier cache so leader-only code can
// SHARE it instead of constructing a second one.
//
// The worker's cache is the right one to share because the worker runs on every
// replica for the process's whole life, while LeaderController's resources are
// built on election and torn down on demotion. Two instances in one process
// meant two L1 maps and two subscriptions to the same invalidation channel, so
// the leader processed every invalidation twice -- measured 2026-08-21: the
// leader miner counted +204 supplier invalidations over an idle window where a
// relayer counted +102, exactly 2x.
//
// Returns nil before Start() has built it; callers must handle that.
func (w *SupplierWorker) GetSupplierCache() *cache.SupplierCache {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.supplierCache
}
