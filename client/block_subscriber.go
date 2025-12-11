package client

import (
	"context"
	"crypto/tls"
	"fmt"
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
	reconnectMaxDelay = 30 * time.Second

	// reconnectBackoffFactor is the multiplier for exponential backoff.
	reconnectBackoffFactor = 2
)

// BlockSubscriberConfig contains configuration for the block subscriber.
type BlockSubscriberConfig struct {
	// RPCEndpoint is the CometBFT RPC endpoint (e.g., "http://localhost:26657")
	RPCEndpoint string

	// UseTLS enables TLS for the RPC connection.
	UseTLS bool
}

// DefaultBlockSubscriberConfig returns sensible defaults.
func DefaultBlockSubscriberConfig() BlockSubscriberConfig {
	return BlockSubscriberConfig{}
}

// BlockSubscriber is a WebSocket-based BlockClient that subscribes to block events.
// It implements client.BlockClient interface using CometBFT WebSocket subscriptions
// instead of polling, providing immediate block notifications with automatic reconnection.
type BlockSubscriber struct {
	logger      logging.Logger
	config      BlockSubscriberConfig
	cometClient *http.HTTP

	// Current block state
	lastBlock atomic.Pointer[simpleBlock]

	// Chain version
	chainVersion   *version.Version
	chainVersionMu sync.RWMutex

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
	mu       sync.Mutex
}

// NewBlockSubscriber creates a new block subscriber with WebSocket subscriptions.
func NewBlockSubscriber(
	logger logging.Logger,
	config BlockSubscriberConfig,
) (*BlockSubscriber, error) {
	if config.RPCEndpoint == "" {
		return nil, fmt.Errorf("RPC endpoint is required")
	}

	// Create CometBFT HTTP client with WebSocket support
	var cometClient *http.HTTP
	var err error

	if config.UseTLS {
		// For TLS, we need to create a custom HTTP client
		httpClient := &tls.Config{MinVersion: tls.VersionTLS12}
		_ = httpClient // TODO: Use custom transport if needed
		cometClient, err = http.New(config.RPCEndpoint, "/websocket")
	} else {
		cometClient, err = http.New(config.RPCEndpoint, "/websocket")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create CometBFT client: %w", err)
	}

	return &BlockSubscriber{
		logger:      logging.ForComponent(logger, logging.ComponentBlockSubscriber),
		config:      config,
		cometClient: cometClient,
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

	// Get initial block via RPC
	if err := bs.fetchLatestBlock(ctx); err != nil {
		bs.logger.Warn().Err(err).Msg("failed to fetch initial block, will retry")
	}

	// Initialize chain version
	if err := bs.initializeChainVersion(ctx); err != nil {
		bs.logger.Warn().Err(err).Msg("failed to initialize chain version")
	}

	// Start the CometBFT HTTP client to enable WebSocket subscriptions
	// This is required before calling Subscribe() according to CometBFT documentation
	if err := bs.cometClient.Start(); err != nil {
		return fmt.Errorf("failed to start CometBFT client: %w", err)
	}

	// Start subscription goroutine with reconnection handling
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

		// Reset backoff on successful subscription
		reconnectDelay = reconnectBaseDelay
		bs.logger.Info().Msg("WebSocket subscription established")

		// Process events until channel closes or context cancelled
		bs.processEvents(ctx, eventsCh)

		// Channel closed - check if we're shutting down or if it's a real disconnection
		select {
		case <-ctx.Done():
			// Context cancelled, graceful shutdown - don't log reconnection
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
				bs.logger.Debug().
					Err(err).
					Str("event_type", fmt.Sprintf("%T", resultEvent.Data)).
					Msg("failed to handle block event")
			}
		}
	}
}

// handleBlockEvent parses a block header event and updates the last block.
func (bs *BlockSubscriber) handleBlockEvent(resultEvent *coretypes.ResultEvent) error {
	// Type assert to EventDataNewBlockHeader
	blockHeader, ok := resultEvent.Data.(types.EventDataNewBlockHeader)
	if !ok {
		return fmt.Errorf("expected EventDataNewBlockHeader, got %T", resultEvent.Data)
	}

	// Extract height and hash
	height := blockHeader.Header.Height
	hash := blockHeader.Header.Hash()

	// Create new block
	block := &simpleBlock{
		height: height,
		hash:   hash,
	}

	// Update last block atomically
	oldBlock := bs.lastBlock.Load()
	bs.lastBlock.Store(block)

	// Log if height changed (sampled: every 100th block to reduce verbosity)
	if oldBlock == nil || block.height > oldBlock.height {
		if block.height%100 == 0 {
			bs.logger.Debug().
				Int64("height", block.height).
				Msg("new block received via WebSocket")
		}
	}

	return nil
}

// increaseBackoff increases the reconnect delay with exponential backoff.
func (bs *BlockSubscriber) increaseBackoff(current time.Duration) time.Duration {
	next := current * reconnectBackoffFactor
	if next > reconnectMaxDelay {
		return reconnectMaxDelay
	}
	return next
}

// fetchLatestBlock fetches and stores the latest block via RPC query.
// Used for initial block and fallback when subscription not available.
func (bs *BlockSubscriber) fetchLatestBlock(ctx context.Context) error {
	result, err := bs.cometClient.Block(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to query block: %w", err)
	}

	block := &simpleBlock{
		height: result.Block.Height,
		hash:   result.Block.Hash(),
	}

	bs.lastBlock.Store(block)

	bs.logger.Debug().
		Int64("height", block.height).
		Msg("fetched initial block via RPC")

	return nil
}

// initializeChainVersion fetches and stores the chain version.
func (bs *BlockSubscriber) initializeChainVersion(ctx context.Context) error {
	abciInfo, err := bs.cometClient.ABCIInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to get ABCI info: %w", err)
	}

	chainVersion, err := version.NewVersion(abciInfo.Response.Version)
	if err != nil {
		return fmt.Errorf("failed to parse chain version: %w", err)
	}

	bs.chainVersionMu.Lock()
	bs.chainVersion = chainVersion
	bs.chainVersionMu.Unlock()

	bs.logger.Info().
		Str("version", chainVersion.String()).
		Msg("chain version initialized")

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
			return &simpleBlock{height: 0, hash: nil}
		}
	}
	return block
}

// CommittedBlocksSequence returns nil since we don't provide observable interface.
// The subscription-based updates happen internally via WebSocket.
func (bs *BlockSubscriber) CommittedBlocksSequence(ctx context.Context) client.BlockReplayObservable {
	// Not implemented - internal WebSocket subscription is used instead
	return nil
}

// GetChainVersion returns the cached chain version.
func (bs *BlockSubscriber) GetChainVersion() *version.Version {
	bs.chainVersionMu.RLock()
	defer bs.chainVersionMu.RUnlock()
	return bs.chainVersion
}

// GetChainID fetches the chain ID from the node.
func (bs *BlockSubscriber) GetChainID(ctx context.Context) (string, error) {
	status, err := bs.cometClient.Status(ctx)
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

	// Unsubscribe from events
	if bs.cometClient != nil {
		// UnsubscribeAll closes the subscription
		_ = bs.cometClient.UnsubscribeAll(context.Background(), subscriptionClientID)
	}

	if bs.cancelFn != nil {
		bs.cancelFn()
	}

	bs.wg.Wait()

	bs.logger.Info().Msg("block subscriber closed")
}

// Ensure BlockSubscriber implements BlockClient interface
var _ client.BlockClient = (*BlockSubscriber)(nil)
