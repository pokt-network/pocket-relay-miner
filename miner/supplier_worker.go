package miner

import (
	"context"
	"crypto/tls"
	"fmt"
	stdhttp "net/http"
	"runtime"
	"sync"

	"github.com/alitto/pond/v2"
	"github.com/cometbft/cometbft/rpc/client/http"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/transport"
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

	// Create query clients
	var err error
	w.queryClients, err = query.NewQueryClients(
		w.logger,
		query.ClientConfig{
			GRPCEndpoint: w.config.QueryNodeGRPCUrl,
			QueryTimeout: w.config.Config.GetQueryTimeout(),
			UseTLS:       !w.config.GRPCInsecure,
		},
	)
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	w.logger.Info().
		Str("grpc_endpoint", w.config.QueryNodeGRPCUrl).
		Msg("query clients initialized")

	// Advisory: warn once if the chain runs shared params that poktroll's own validation
	// would reject (genesis skips it, MsgUpdateParams does not).
	//
	// Runs on the master pool rather than inline: Start() holds w.mu and still has to
	// build the RPC client, block client and session pipeline, and this is a blocking
	// gRPC round trip bounded only by the operator-configurable query timeout. Nothing
	// downstream reads its result — it only logs — so an unreachable node must not
	// lengthen every miner start.
	sharedForAdvisory := w.queryClients.Shared()
	if advisoryErr := w.masterPool.Go(func() {
		advisoryCtx, cancelAdvisory := context.WithTimeout(w.ctx, sharedParamsAdvisoryTimeout)
		defer cancelAdvisory()

		sharedParams, sharedErr := sharedForAdvisory.GetParams(advisoryCtx)
		if sharedErr != nil {
			// Warn, not Debug: "the params are fine" and "we never looked" must not
			// produce the same silence, and an unreachable node is exactly the case
			// this advisory exists to surface.
			w.logger.Warn().Err(sharedErr).
				Msg("shared params advisory skipped: could not read shared params from the chain")
			return
		}
		LogSharedParamsAdvisory(w.logger, sharedParams)
	}); advisoryErr != nil {
		// The pool refused the task (already stopped). Advisory only — never a reason
		// to fail startup, but say so rather than letting it vanish.
		w.logger.Warn().Err(advisoryErr).
			Msg("shared params advisory skipped: worker pool would not accept the task")
	}

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
		cache.SupplierCacheConfig{FailOpen: false},
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
			GRPCConn:                 w.queryClients.GRPCConnection(),
			ChainID:                  chainID,
			GasLimit:                 w.config.Config.GetTxGasLimit(),
			GasPrice:                 gasPrice,
			GasAdjustment:            w.config.Config.GetTxGasAdjustment(),
			TxTimeoutMin:             w.config.Config.GetTxTimeoutMin(),
			TxTimeoutMax:             w.config.Config.GetTxTimeoutMax(),
			TxTimeoutDefault:         w.config.Config.GetTxTimeoutDefault(),
			TxTimeoutClockSkewBuffer: w.config.Config.GetTxTimeoutClockSkewBuffer(),
			// Anchor tx timeoutTimestamp on the chain's latest_block_time.
			// The RedisBlockSubscriber is already wired and tracking
			// block events for every replica (leader and standby), so
			// passing it here gives the tx client the exact clock the
			// cosmos-sdk ante handler uses to validate unordered-tx
			// TTLs. Prevents `unordered tx ttl exceeds 10m0s` rejections
			// when the chain's block time lags wall clock. See
			// BlockTimeProvider in tx/tx_client.go for the full rationale.
			BlockTimeProvider: w.redisBlockSubscriber,
		},
	)
	if err != nil {
		w.cleanup()
		return fmt.Errorf("failed to create tx client: %w", err)
	}
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
			ConsumerName:                   w.config.Config.Redis.ConsumerName,
			SessionTTL:                     w.config.Config.GetSessionTTL(), // Uses CacheTTL if not explicitly set
			CacheTTL:                       w.config.Config.GetCacheTTL(),
			SMSTLiveRootCheckpointInterval: w.config.Config.SMSTLiveRootCheckpointInterval,
			BatchSize:                      w.config.Config.BatchSize,
			ClaimIdleTimeout:               w.config.Config.GetClaimIdleTimeout(),
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
			DisableClaimBatching:             w.config.Config.Transaction.DisableClaimBatching,
			DisableProofBatching:             w.config.Config.Transaction.DisableProofBatching,
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

	// Persist apps/services seen in relay traffic to the shared Redis known-sets
	// so the leader's CacheOrchestrator refreshes them (dedup'd; off the hot path
	// after first sight).
	w.recordDiscovered(ctx, msg.Message.ApplicationAddress, msg.Message.ServiceId)

	// Check session state BEFORE updating SMST. A store error falls through on
	// purpose: it says nothing about the session, and dropping a relay on a
	// Redis hiccup is the expensive direction.
	if state.SessionStore != nil {
		snapshot, storeErr := state.SessionStore.Get(ctx, msg.Message.SessionId)

		// Hoisted because it is STORE-INDEPENDENT: the claim window is decided by
		// the message's own session end height against the observed block height,
		// and reads nothing the store could fail to answer. Until 2026-08-28 it
		// sat inside the switch, so a transient Redis Get error fell through to
		// the first case and skipped it -- admitting a relay whose window had
		// definitively closed, which then created or extended a session the sweep
		// could only carry to claim_window_closed and delete. The store error is
		// still a reason to keep the relay when the window is OPEN; it is not a
		// reason to stop knowing what the height already says.
		windowClosed := state.SessionCoordinator != nil &&
			state.SessionCoordinator.ClaimWindowClosed(msg.Message.SessionEndHeight)

		switch {
		case storeErr != nil && !windowClosed:
			// fall through and process the relay

		case snapshot != nil && snapshot.State.IsTerminal():
			w.logger.Debug().
				Str("session_id", msg.Message.SessionId).
				Str("supplier", supplierAddr).
				Str("session_state", string(snapshot.State)).
				Msg("LATE_RELAY: dropping relay - session already in terminal state")
			RecordRelayRejected(supplierAddr, "session_sealed", msg.Message.ServiceId)
			return nil

		case windowClosed:
			// The claim window has closed, so this relay cannot reach a claim
			// whatever else is true. Deliberately NOT conditioned on the
			// snapshot being absent: a session sitting in active or claiming
			// past its window is not terminal yet -- the sweep has not reached
			// it -- so the case above does not catch it, and without this the
			// relay would be added to a tree nothing will ever claim.
			//
			// With no snapshot at all it is the same verdict for a different
			// reason: processing on would CREATE a session the sweep could only
			// carry to claim_window_closed and delete again.
			//
			// No money counter is booked here on purpose. This runs BEFORE the
			// duplicate check, so a late copy of a relay that was already
			// processed and paid would land in it; counting that as lost
			// revenue would be wrong. relays_rejected says what happened
			// without claiming the work went unpaid.
			// Named sessionState, not state: `state` is the *SupplierState
			// this function already holds.
			sessionState := "absent"
			if snapshot != nil {
				sessionState = string(snapshot.State)
			}
			w.logger.Debug().
				Str("session_id", msg.Message.SessionId).
				Str("supplier", supplierAddr).
				Str("session_state", sessionState).
				Int64("session_end_height", msg.Message.SessionEndHeight).
				Msg("LATE_RELAY: dropping relay - claim window already closed")
			RecordRelayRejected(supplierAddr, "claim_window_closed", msg.Message.ServiceId)
			return nil
		}
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
	// Running it on every delivery is safe: OnSessionCreated goes through
	// CreateIfAbsent, a first-write-wins gate, so it costs one round-trip and
	// cannot double-create.
	state.SessionCoordinator.EnsureSession(
		ctx,
		msg.Message.SessionId,
		msg.Message.SupplierOperatorAddress,
		msg.Message.ServiceId,
		msg.Message.ApplicationAddress,
		msg.Message.SessionStartHeight,
		msg.Message.SessionEndHeight,
	)

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
				return nil
			}
		}
	}

	// Update SMST with relay bytes
	if err := state.SMSTManager.UpdateTree(
		ctx,
		msg.Message.SessionId,
		msg.Message.RelayHash,
		msg.Message.RelayBytes,
		msg.Message.ComputeUnitsPerRelay,
	); err != nil {
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
			RecordRelayRejected(supplierAddr, "session_sealed", msg.Message.ServiceId)
			RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, "session_sealed")
			return nil // ACK and discard - no point retrying
		}
		// Unknown/unexpected errors - log and discard (don't retry forever)
		w.logger.Warn().
			Err(err).
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Msg("unexpected SMST error - discarding relay")
		RecordRelayFailedSMST(supplierAddr, msg.Message.ServiceId, "unexpected_error")
		return nil // ACK and discard - unknown errors shouldn't block processing
	}

	// Mark relay hash as processed in the deduplicator. This runs on every
	// relay (not only reclaims) because if the current consumer crashes
	// after the SMST update but before the stream ACK, the next consumer
	// will reclaim the message and needs the dedup set to recognize it as
	// already processed. The SADD result is the correctness gate for
	// OnRelayProcessed below: the reclaim skips entries this consumer owns
	// and takes only those idle past the timeout, but idleness cannot
	// distinguish a dead consumer from a slow-but-alive one — so the
	// duplicate can be the ORIGINAL copy arriving with IsReclaim=false after
	// another consumer already processed the reclaimed one, and this is the
	// only place that catches that ordering.
	// Ordering matters: MarkProcessed runs BEFORE OnRelayProcessed
	// (IncrementRelayCount) below so that a crash between them leaves the
	// counter under-counted rather than over-counted — under-count is the
	// safe direction (economic viability predicts lower rewards and skips
	// marginal sessions instead of claiming unprofitable ones).
	firstProcessing := true
	if dedup := w.supplierManager.Deduplicator(); dedup != nil && len(msg.Message.RelayHash) > 0 {
		added, markErr := dedup.MarkProcessed(ctx, msg.Message.RelayHash, msg.Message.SessionId)
		switch {
		case markErr != nil:
			// Fail-open: count the relay anyway. Better to risk a rare
			// double-count during Redis degradation than to drop billing
			// for a valid relay.
			w.logger.Debug().
				Err(markErr).
				Str("session_id", msg.Message.SessionId).
				Msg("deduplicator mark_processed failed")
		case !added:
			firstProcessing = false
			w.logger.Debug().
				Str("session_id", msg.Message.SessionId).
				Str("supplier", supplierAddr).
				Msg("relay already marked processed - skipping session counter increment")
			RecordRelayRejected(supplierAddr, "duplicate", msg.Message.ServiceId)
		}
	}

	// MEMORY OPTIMIZATION: Clear RelayBytes and RelayHash after SMST update
	// The SMST has copied the data to Redis - these fields are no longer needed.
	// This allows GC to reclaim the memory early instead of holding until message is ACK'd.
	msg.Message.RelayBytes = nil
	msg.Message.RelayHash = nil

	// Track relay successfully added to SMST
	RecordRelayAddedToSMST(supplierAddr, msg.Message.ServiceId)

	// Track relay in session coordinator, gated by the MarkProcessed result
	// above: only the FIRST processing of a relay increments the COUNTERS.
	// Counting one relay twice inflates the claim, so this half stays gated
	// while the creation above does not.
	//
	// ACK-and-log on failure: SMST + dedup are the sources of truth for
	// the claim; SessionCoordinator (snapshot.TotalComputeUnits) is
	// derived state used for economic-viability decisions and operator
	// observability. Returning an error here would leave the stream
	// message un-ACK'd and the reclaim would take it on idle timeout;
	// the dedup gate above rejects the duplicate increment on that
	// redelivery (unless the dedup set entry already expired — treat the
	// call as best-effort). Log at WARN for operator visibility, then ACK.
	if !firstProcessing {
		return nil // ACK: SMST already holds the relay; counters already incremented once
	}
	if err := state.SessionCoordinator.OnRelayProcessed(
		ctx,
		msg.Message.SessionId,
		msg.Message.ComputeUnitsPerRelay,
		msg.Message.SupplierOperatorAddress,
		msg.Message.ServiceId,
		msg.Message.ApplicationAddress,
		msg.Message.SessionStartHeight,
		msg.Message.SessionEndHeight,
	); err != nil {
		w.logger.Debug().
			Err(err).
			Str("session_id", msg.Message.SessionId).
			Str("supplier", supplierAddr).
			Msg("session coordinator update failed — ACKing relay (SMST and dedup already committed)")
	}

	return nil
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
func (w *SupplierWorker) GetSupplierManager() *SupplierManager {
	return w.supplierManager
}
