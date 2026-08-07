package client

import (
	"context"
	"crypto/tls"
	"fmt"
	stdhttp "net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cometbft/cometbft/types"
	"github.com/hashicorp/go-version"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

const (
	// newBlockHeaderQuery is the WebSocket subscription query for new blocks.
	// Uses 'NewBlockHeader' instead of 'NewBlock' for efficiency (only header data, no full tx list).
	newBlockHeaderQuery = "tm.event='NewBlockHeader'"

	// subscriptionClientID is the client identifier for the WebSocket subscription.
	subscriptionClientID = "ha-block-subscriber"

	// reconnectBaseDelay is the base delay for exponential backoff reconnection.
	reconnectBaseDelay = 1 * time.Second

	// reconnectMaxDelay is the maximum delay between reconnection attempts.
	reconnectMaxDelay = 15 * time.Second

	// reconnectBackoffFactor is the multiplier for exponential backoff.
	reconnectBackoffFactor = 2

	// defaultQueryTimeout is the default timeout for RPC queries (Block, ABCIInfo, Status).
	defaultQueryTimeout = 5 * time.Second

	// defaultSubscriberBufferSize is the default buffer size for subscriber channels.
	defaultSubscriberBufferSize = 100
)

// SimpleBlock implements client.Block interface with timestamp support.
// This is a lightweight implementation used by both BlockPoller (deprecated)
// and BlockSubscriber for representing blockchain blocks.
type SimpleBlock struct {
	height    int64
	hash      []byte
	timestamp time.Time
}

func (b *SimpleBlock) Height() int64   { return b.height }
func (b *SimpleBlock) Hash() []byte    { return b.hash }
func (b *SimpleBlock) Time() time.Time { return b.timestamp }

// NewSimpleBlock creates a new SimpleBlock with the given height, hash, and timestamp.
// This constructor is used by components that need to create SimpleBlock instances
// from external sources (e.g., Redis pub/sub events).
func NewSimpleBlock(height int64, hash []byte, timestamp time.Time) *SimpleBlock {
	return &SimpleBlock{
		height:    height,
		hash:      hash,
		timestamp: timestamp,
	}
}

// Ensure SimpleBlock implements client.Block
var _ client.Block = (*SimpleBlock)(nil)

// subscriberInfo tracks metadata about a subscriber for debugging.
type subscriberInfo struct {
	id         uint64
	ch         chan *SimpleBlock
	callerFunc string // Function that created the subscriber
	callerFile string // File that created the subscriber
	callerLine int    // Line that created the subscriber
}

// cometbftServerIdleTimeout is how long a CometBFT RPC server keeps an idle
// keep-alive connection before closing it.
//
// It is not configured as an idle timeout anywhere — it falls out of two
// defaults meeting. CometBFT's RPC server sets ReadTimeout: 10s and never sets
// IdleTimeout (cometbft/rpc/jsonrpc/server/http_server.go, DefaultConfig), and
// net/http documents that a zero IdleTimeout means ReadTimeout is used instead
// (net/http/server.go, Server.IdleTimeout). So 10s of silence closes the
// connection, and the client is not told.
const cometbftServerIdleTimeout = 10 * time.Second

// rpcIdleConnTimeout is how long WE keep an idle connection to that server.
//
// It must stay below cometbftServerIdleTimeout, and the reason is a bug this
// cost us. Blocks arrive slower than 10s on every network we run against
// (~10.1s on localnet, ~60s on mainnet), so by the time a block event triggers
// the canonical-hash query, the pooled connection has always been idle past
// the server's limit. The server closed it; the client did not know; the POST
// went into a dead socket and came back EOF.
//
// Go does not retry it, either: net/http retries a request that fails on a
// reused connection only when the request is replayable, and a POST is
// replayable only if it carries an Idempotency-Key header
// (net/http/request.go, isReplayable). The CometBFT JSON-RPC client sends
// plain POSTs. So the error surfaced, handleBlockEvent returned early, and the
// block event was dropped for every subscriber — including the miner's cache
// orchestrator. Then the failure emptied the pool, the next block opened a
// fresh connection and succeeded, and that one went stale in turn: exactly
// every other block lost, measured at 15 of 15 even-numbered heights in a
// five-minute run on 2026-08-06.
//
// Retiring the connection first makes the next request dial a new one. That
// costs one TCP handshake per block, against one dropped block event in two.
//
// Derived from the server's limit rather than written as a number, so the two
// cannot drift apart silently. Half leaves room for scheduling delay and clock
// skew between the two processes.
const rpcIdleConnTimeout = cometbftServerIdleTimeout / 2

// newRPCTransport builds the transport used for CometBFT RPC calls.
// A nil tlsConfig leaves the default (plaintext) behaviour.
func newRPCTransport(idleConnTimeout time.Duration, tlsConfig *tls.Config) *stdhttp.Transport {
	transport := stdhttp.DefaultTransport.(*stdhttp.Transport).Clone()
	transport.IdleConnTimeout = idleConnTimeout
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}

	return transport
}

// BlockSubscriberConfig contains configuration for the block subscriber.
type BlockSubscriberConfig struct {
	// RPCEndpoint is the CometBFT RPC endpoint (e.g., "http://localhost:26657")
	RPCEndpoint string

	// UseTLS enables TLS for the RPC connection.
	UseTLS bool

	// QueryTimeout is the timeout for RPC queries (Block, ABCIInfo, Status).
	// Default: 5 seconds
	QueryTimeout time.Duration
}

// DefaultBlockSubscriberConfig returns sensible defaults.
func DefaultBlockSubscriberConfig() BlockSubscriberConfig {
	return BlockSubscriberConfig{
		QueryTimeout: defaultQueryTimeout,
	}
}

// BlockSubscriber is a WebSocket-based BlockClient that subscribes to block events.
// It implements `client.BlockClient` interface using CometBFT WebSocket subscriptions
// instead of polling, providing immediate block notifications with automatic reconnection.
type BlockSubscriber struct {
	logger      logging.Logger
	config      BlockSubscriberConfig
	cometClient *http.HTTP

	// Current block state
	lastBlock atomic.Pointer[SimpleBlock]

	// Fan-out pub/sub for multiple consumers
	// Each subscriber gets an independent channel to avoid race conditions
	subscribers   map[uint64]*subscriberInfo
	subscribersMu sync.RWMutex
	nextSubID     atomic.Uint64

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
	mu       sync.Mutex
}

// Ensure BlockSubscriber implements BlockClient interface from poktroll.
//
// Note: Some methods (CommittedBlocksSequence, GetChainVersion) are required by the
// interface but not used in production. They exist solely for interface compliance.
// See individual method documentation for details.
//
// Interface: github.com/pokt-network/poktroll/pkg/client.BlockClient
var _ client.BlockClient = (*BlockSubscriber)(nil)

// NewBlockSubscriber creates a new block subscriber with WebSocket subscriptions.
func NewBlockSubscriber(
	logger logging.Logger,
	config BlockSubscriberConfig,
) (*BlockSubscriber, error) {
	if config.RPCEndpoint == "" {
		return nil, fmt.Errorf("RPC endpoint is required")
	}

	// Normalize a trailing slash: the CometBFT client appends the "/websocket"
	// endpoint to the remote, so "https://host/" would produce a "//websocket"
	// path. A reverse proxy in front of the node (e.g. Sauron on beta/mainnet)
	// rejects the double-slash WS upgrade with "bad handshake". Trimming it makes
	// "https://host/" and "https://host" behave identically.
	config.RPCEndpoint = strings.TrimRight(config.RPCEndpoint, "/")

	// Default query timeout if not set
	if config.QueryTimeout == 0 {
		config.QueryTimeout = defaultQueryTimeout
	}

	// Create the CometBFT HTTP client. Both branches go through NewWithClient
	// so the RPC calls use our transport; the WebSocket subscription is built
	// separately inside the CometBFT client and is unaffected by it.
	var tlsConfig *tls.Config
	if config.UseTLS {
		// Required for nodes behind TLS.
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	httpClient := &stdhttp.Client{Transport: newRPCTransport(rpcIdleConnTimeout, tlsConfig)}

	cometClient, err := http.NewWithClient(config.RPCEndpoint, "/websocket", httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create CometBFT client: %w", err)
	}

	return &BlockSubscriber{
		logger:      logging.ForComponent(logger, logging.ComponentBlockSubscriber),
		config:      config,
		cometClient: cometClient,
		subscribers: make(map[uint64]*subscriberInfo),
	}, nil
}

// Start begins the WebSocket subscription for new blocks.
func (bs *BlockSubscriber) Start(ctx context.Context) error {
	bs.mu.Lock()
	if bs.closed {
		bs.mu.Unlock()
		return fmt.Errorf("block subscriber is closed")
	}
	bs.ctx, bs.cancelFn = context.WithCancel(ctx)
	bs.mu.Unlock()

	// Get the initial block via RPC
	if err := bs.fetchLatestBlock(ctx); err != nil {
		bs.logger.Warn().Err(err).Msg("failed to fetch initial block, will retry")
	}

	// Start the CometBFT HTTP client to enable WebSocket subscriptions
	// This is required before calling Subscribe() according to CometBFT documentation
	if err := bs.cometClient.Start(); err != nil {
		return fmt.Errorf("failed to start CometBFT client: %w", err)
	}

	// Start a subscription goroutine with reconnection handling
	bs.wg.Add(1)
	go bs.subscriptionLoop(bs.ctx)

	bs.logger.Info().
		Str("rpc_endpoint", bs.config.RPCEndpoint).
		Str("query", newBlockHeaderQuery).
		Msg("block subscriber started with WebSocket subscription")

	return nil
}

// subscriptionLoop manages the WebSocket subscription with automatic reconnection.
func (bs *BlockSubscriber) subscriptionLoop(ctx context.Context) {
	defer bs.wg.Done()

	reconnectDelay := reconnectBaseDelay

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Subscribe to NewBlockHeader events
		eventsCh, err := bs.cometClient.Subscribe(ctx, subscriptionClientID, newBlockHeaderQuery)
		if err != nil {
			bs.logger.Warn().
				Err(err).
				Dur("retry_in", reconnectDelay).
				Msg("failed to subscribe to block events, will retry")

			// Exponential backoff
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
				reconnectDelay = bs.increaseBackoff(reconnectDelay)
				continue
			}
		}

		// Reset backoff on a successful subscription
		reconnectDelay = reconnectBaseDelay
		bs.logger.Info().Msg("WebSocket subscription established")

		// Process events until channel closes or context cancelled
		bs.processEvents(ctx, eventsCh)

		// Channel closed - check if we're shutting down or if it's a real disconnection
		select {
		case <-ctx.Done():
			// Context canceled, graceful shutdown - don't log reconnection
			bs.logger.Debug().Msg("WebSocket subscription closed (shutting down)")
			return
		default:
			// Real disconnection - will reconnect
			bs.logger.Warn().Msg("WebSocket disconnected, reconnecting...")
		}
	}
}

// processEvents processes incoming block events from the WebSocket subscription.
func (bs *BlockSubscriber) processEvents(ctx context.Context, eventsCh <-chan coretypes.ResultEvent) {
	for {
		select {
		case <-ctx.Done():
			return

		case resultEvent, ok := <-eventsCh:
			if !ok {
				// Channel closed - subscription lost
				return
			}

			// Parse the event
			if err := bs.handleBlockEvent(&resultEvent); err != nil {
				bs.logger.Error().
					Err(err).
					Str("event_type", fmt.Sprintf("%T", resultEvent.Data)).
					Msg("failed to handle block event")
			}
		}
	}
}

// handleBlockEvent parses a block header event and updates the last block.
func (bs *BlockSubscriber) handleBlockEvent(resultEvent *coretypes.ResultEvent) error {
	// Type assertion to EventDataNewBlockHeader
	blockHeader, ok := resultEvent.Data.(types.EventDataNewBlockHeader)
	if !ok {
		return fmt.Errorf("expected EventDataNewBlockHeader, got %T", resultEvent.Data)
	}

	height := blockHeader.Header.Height
	timestamp := blockHeader.Header.Time

	// CRITICAL: We must query the full block to get BlockID.Hash (canonical block ID)
	// EventDataNewBlockHeader only has Header.Hash() which is computed, not canonical
	// The canonical BlockID.Hash must match what validator stores in ctx.HeaderHash()
	// See: poktroll/x/session/keeper/keeper.go:82
	ctx, cancel := context.WithTimeout(context.Background(), bs.config.QueryTimeout)
	defer cancel()

	result, err := bs.cometClient.Block(ctx, &height)
	if err != nil {
		return fmt.Errorf("failed to query block %d for canonical hash: %w", height, err)
	}

	// Create a new block using canonical BlockID.Hash
	block := &SimpleBlock{
		height:    height,
		hash:      result.BlockID.Hash,
		timestamp: timestamp,
	}

	// Update the last block atomically
	oldBlock := bs.lastBlock.Load()
	bs.lastBlock.Store(block)

	// Publish block event to all subscribers (fan-out)
	// Only publish if height actually changed to avoid duplicate events
	if oldBlock == nil || block.height > oldBlock.height {
		bs.publishToSubscribers(block)

		// Log if height changed (sampled: every 10th block to reduce verbosity)
		if block.height%10 == 0 {
			bs.logger.Debug().
				Int64("height", block.height).
				Time("block_time", block.timestamp).
				Msg("new block received via WebSocket")
		}
	}

	return nil
}

// increaseBackoff increases the reconnection delay with exponential backoff.
func (bs *BlockSubscriber) increaseBackoff(current time.Duration) time.Duration {
	next := current * reconnectBackoffFactor
	if next > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return next
}

// Subscribe creates an independent channel for a consumer to receive block events.
// Each subscriber gets its own buffered channel, preventing race conditions that
// occur when multiple goroutines read from a shared channel.
//
// The subscriber channel will be automatically closed when:
// - The provided context is canceled
// - The BlockSubscriber is closed
//
// Buffer size controls how many events can be queued before dropping.
// Recommended values:
// - 50-100 for monitoring/health checks
// - 100-200 for session lifecycle management
// - 100+ for publishing to Redis
func (bs *BlockSubscriber) Subscribe(ctx context.Context, bufferSize int) <-chan *SimpleBlock {
	if bufferSize <= 0 {
		bufferSize = defaultSubscriberBufferSize
	}

	// Capture caller information for debugging
	pc, file, line, _ := runtime.Caller(1)
	callerFunc := "unknown"
	if fn := runtime.FuncForPC(pc); fn != nil {
		callerFunc = fn.Name()
	}

	bs.subscribersMu.Lock()
	defer bs.subscribersMu.Unlock()

	// Generate unique subscriber ID
	subID := bs.nextSubID.Add(1)

	// Create a buffered channel for this subscriber
	ch := make(chan *SimpleBlock, bufferSize)

	// Store subscriber info with caller metadata
	info := &subscriberInfo{
		id:         subID,
		ch:         ch,
		callerFunc: callerFunc,
		callerFile: file,
		callerLine: line,
	}
	bs.subscribers[subID] = info

	// Auto-cleanup on context cancellation
	go func() {
		<-ctx.Done()
		bs.unsubscribe(subID)
	}()

	bs.logger.Debug().
		Uint64("subscriber_id", subID).
		Int("buffer_size", bufferSize).
		Str("caller_func", callerFunc).
		Str("caller_file", file).
		Int("caller_line", line).
		Msg("new block subscriber registered")

	return ch
}

// unsubscribe removes a subscriber and closes its channel.
// This prevents goroutine leaks by ensuring consumers exit their range loops.
func (bs *BlockSubscriber) unsubscribe(subID uint64) {
	bs.subscribersMu.Lock()
	defer bs.subscribersMu.Unlock()

	if info, exists := bs.subscribers[subID]; exists {
		close(info.ch) // MUST close to prevent goroutine leak
		delete(bs.subscribers, subID)
		bs.logger.Debug().
			Uint64("subscriber_id", subID).
			Msg("block subscriber unregistered")
	}
}

// publishToSubscribers sends a block event to all registered subscribers.
// Uses non-blocking send to prevent slow consumers from blocking the publisher.
// If a subscriber's buffer is full, the event is dropped for that subscriber only.
func (bs *BlockSubscriber) publishToSubscribers(block *SimpleBlock) {
	bs.subscribersMu.RLock()
	defer bs.subscribersMu.RUnlock()

	// Send it to all subscribers (non-blocking)
	for _, info := range bs.subscribers {
		select {
		case info.ch <- block:
			// Event delivered successfully
		default:
			// Buffer full - drop event for this subscriber
			// Don't block other subscribers or the WebSocket event loop
			// Set as error since this should not happen AND if happens we need to check why the consumer is slow
			bs.logger.Error().
				Uint64("subscriber_id", info.id).
				Int64("height", block.height).
				Str("caller_func", info.callerFunc).
				Str("caller_file", info.callerFile).
				Int("caller_line", info.callerLine).
				Msg("subscriber buffer full, dropping block event")
		}
	}
}

// fetchLatestBlock fetches and stores the latest block via an RPC query.
// Used for the initial block and fallback when the subscription is not available.
func (bs *BlockSubscriber) fetchLatestBlock(ctx context.Context) error {
	queryCtx, cancel := context.WithTimeout(ctx, bs.config.QueryTimeout)
	defer cancel()

	result, err := bs.cometClient.Block(queryCtx, nil)
	if err != nil {
		return fmt.Errorf("failed to query block: %w", err)
	}

	// CRITICAL: Use BlockID.Hash (canonical block ID) instead of Block.Hash() (computed hash)
	// This must match what the validator stores in ctx.HeaderHash() via StoreBlockHash()
	// See: poktroll/x/session/keeper/keeper.go:82
	block := &SimpleBlock{
		height:    result.Block.Height,
		hash:      result.BlockID.Hash,
		timestamp: result.Block.Time,
	}

	bs.lastBlock.Store(block)

	bs.logger.Info().
		Int64("height", block.height).
		Time("block_time", block.timestamp).
		Msg("fetched initial block via RPC")

	return nil
}

// LastBlock returns the last known block.
func (bs *BlockSubscriber) LastBlock(ctx context.Context) client.Block {
	block := bs.lastBlock.Load()
	if block == nil {
		// If no block yet, try to fetch one
		_ = bs.fetchLatestBlock(ctx)
		block = bs.lastBlock.Load()
		if block == nil {
			// Return a zero block if still nil
			return &SimpleBlock{height: 0, hash: nil}
		}
	}
	return block
}

// GetBlockAtHeight queries the blockchain for a specific block by height.
// This is CRITICAL for proof generation - the validator uses the exact block at a specific
// height, so we must query that exact block, not just wait for it and return LastBlock().
//
// IMPORTANT: Uses BlockID.Hash (canonical block identifier) instead of Block.Hash() (computed hash).
// This must match what the validator stores via ctx.HeaderHash() in StoreBlockHash().
func (bs *BlockSubscriber) GetBlockAtHeight(ctx context.Context, height int64) (client.Block, error) {
	result, err := bs.cometClient.Block(ctx, &height)
	if err != nil {
		return nil, fmt.Errorf("failed to query block at height %d: %w", height, err)
	}

	// CRITICAL: Use BlockID.Hash (canonical block ID) instead of Block.Hash() (computed hash)
	// This must match what the validator stores in ctx.HeaderHash() via StoreBlockHash()
	// See: poktroll/x/session/keeper/keeper.go:82
	block := &SimpleBlock{
		height:    result.Block.Height,
		hash:      result.BlockID.Hash, // ✅ Canonical hash
		timestamp: result.Block.Time,
	}

	return block, nil
}

// CommittedBlocksSequence returns nil - not used in production.
//
// This method exists solely for poktroll client.BlockClient interface compliance.
// The interface expects an observable-based block replay pattern, but BlockSubscriber
// uses a subscription-based fan-out pattern via Subscribe() instead.
//
// Interface: github.com/pokt-network/poktroll/pkg/client.BlockClient
// Used by: Tests only (never called in production code)
func (bs *BlockSubscriber) CommittedBlocksSequence(_ context.Context) client.BlockReplayObservable {
	return nil
}

// GetChainVersion returns nil - not used in production.
//
// This method exists solely for poktroll client.BlockClient interface compliance.
// The chain version was previously fetched via ABCIInfo RPC, but analysis showed
// no production code actually uses this value - only test mocks access it.
//
// Returning nil saves one unnecessary RPC call (ABCIInfo) at BlockSubscriber startup.
//
// Interface: github.com/pokt-network/poktroll/pkg/client.BlockClient
// Used by: Tests only (never called in production code)
func (bs *BlockSubscriber) GetChainVersion() *version.Version {
	return nil
}

// GetRPCClient returns the underlying CometBFT RPC client.
// This allows reusing the same RPC connection for additional queries (like BlockResults).
func (bs *BlockSubscriber) GetRPCClient() *http.HTTP {
	return bs.cometClient
}

// GetChainID fetches the chain ID from the node.
func (bs *BlockSubscriber) GetChainID(ctx context.Context) (string, error) {
	// Apply configured query timeout
	queryCtx, cancel := context.WithTimeout(ctx, bs.config.QueryTimeout)
	defer cancel()

	status, err := bs.cometClient.Status(queryCtx)
	if err != nil {
		return "", fmt.Errorf("failed to get node status: %w", err)
	}
	return status.NodeInfo.Network, nil
}

// Close stops the block subscriber and unsubscribes from events.
func (bs *BlockSubscriber) Close() {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if bs.closed {
		return
	}
	bs.closed = true

	// Unsubscribe from events with timeout to prevent hanging on shutdown
	if bs.cometClient != nil {
		// Use a fresh context with timeout for shutdown operation
		unsubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := bs.cometClient.UnsubscribeAll(unsubCtx, subscriptionClientID); err != nil {
			bs.logger.Warn().Err(err).Msg("failed to unsubscribe from block events")
		}
	}

	if bs.cancelFn != nil {
		bs.cancelFn()
	}

	bs.wg.Wait()

	// Close all subscriber channels after all goroutines have stopped
	bs.subscribersMu.Lock()
	subscriberCount := len(bs.subscribers)
	for _, info := range bs.subscribers {
		close(info.ch)
		bs.logger.Debug().
			Uint64("subscriber_id", info.id).
			Str("caller_func", info.callerFunc).
			Str("caller_file", info.callerFile).
			Int("caller_line", info.callerLine).
			Msg("closed subscriber channel on shutdown")
	}
	// Clear the subscribers map
	bs.subscribers = make(map[uint64]*subscriberInfo)
	bs.subscribersMu.Unlock()

	bs.logger.Info().
		Str("rpc_endpoint", bs.config.RPCEndpoint).
		Int("subscribers_closed", subscriberCount).
		Msg("block subscriber closed")
}
