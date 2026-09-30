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
	"github.com/pokt-network/pocket-relay-miner/query"
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

// SubscribingBlockClient is the block client this project requires: poktroll's,
// plus the per-block stream every block-driven loop in the miner is built on.
//
// It is a TYPE and not a runtime capability check on purpose. The inclusion
// reconciler used to discover the stream by asserting its block client to an
// anonymous interface, and a client without Subscribe left the reconciler fully
// CONSTRUCTED -- pool running, rebroadcast store writing entries into Redis --
// with no trigger to ever read them, so those entries aged out at their TTL
// while an Error line in the log was the only sign. That is strictly worse than
// having no reconciler at all, where a nil store writes nothing. Requiring the
// capability where the client is wired makes it a build failure instead.
type SubscribingBlockClient interface {
	client.BlockClient
	Subscribe(ctx context.Context, bufferSize int) <-chan *SimpleBlock
}

// SimpleBlock implements client.Block interface with timestamp support.
// It is what RedisBlockClientAdapter hands to its subscribers, and what
// BlockSubscriber emits, to represent a blockchain block.
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

// BlockSubscriberConfig contains configuration for the block subscriber.
type BlockSubscriberConfig struct {
	// RPCEndpoint is the CometBFT RPC endpoint (e.g., "http://localhost:26657")
	RPCEndpoint string

	// UseTLS enables TLS for the RPC connection.
	UseTLS bool

	// QueryTimeout is the timeout for RPC queries (Block, ABCIInfo, Status).
	// Default: 5 seconds
	QueryTimeout time.Duration

	// PollInterval is how often the node's latest height is polled, racing the
	// websocket. Default: defaultPollInterval. Not an operator setting: only
	// tests shorten it.
	PollInterval time.Duration
}

const (
	// defaultPollInterval paces the latest-height poll that races the
	// websocket. One request per second to the node is the cost of never
	// depending on the websocket alone.
	defaultPollInterval = 1 * time.Second

	// wsStaleBlocks is how many heights the poll may publish with the
	// websocket saying nothing before the websocket client is thrown away and
	// a new one opened.
	wsStaleBlocks = 2

	// maxReadFailures is how many passes may fail to read a height, for a
	// reason other than "not yet", before that height is skipped.
	maxReadFailures = 10

	// maxCatchUpBlocks bounds how many missing heights one pass reads. Past
	// it the oldest are skipped: a node that far behind is resyncing, and no
	// window this process is waiting on is still open for them.
	maxCatchUpBlocks = 64
)

// BlockSubscriber is the leader's single reader of blocks from the chain. It
// learns that a height exists from two sources racing each other -- the
// websocket's NewBlockHeader events and a poll of the node's latest height --
// and publishes a height only after reading that block's canonical hash, in
// order, with no height skipped.
//
// Why both sources: CometBFT's websocket is best effort. The node drops events
// for a client it considers slow and can cancel a subscription with an error
// the client may never receive; the client never closes the channel it hands
// out, sends no pings, and gives up after its own reconnect attempts. Any of
// those leaves an open channel with no events. The poll turns that silence into
// latency; the websocket keeps the latency low while it works.
//
// Why publish only after the read: a node announces a height before it can
// serve the data at it, and behind a load balancer the read can land on a node
// one block behind. The read retries that answer ("not yet") on the next pass
// instead of dropping the height.
type BlockSubscriber struct {
	logger logging.Logger
	config BlockSubscriberConfig

	// cometClient serves every HTTP query (Block, Status). It is never started:
	// HTTP calls do not need it, and the websocket gets its own client, which
	// is replaced when it goes silent.
	cometClient *http.HTTP
	reader      *BlockReader

	// nextHeight is the next height to read; a skipped height moves it as a
	// published one does. failingHeight and failures count the passes that
	// could not read nextHeight for a reason other than "not yet". All three
	// belong to the advance loop alone.
	nextHeight    int64
	failingHeight int64
	failures      int

	// Current block state: the last height read and published.
	lastBlock atomic.Pointer[SimpleBlock]

	// seenHeight is the highest height either source reported; wake is
	// signalled when it moves. Only the advance loop reads blocks.
	seenHeight atomic.Int64
	wake       chan struct{}

	// wsHeight is the highest height the websocket reported. publishedPastWS
	// counts the heights published above it since the websocket's last event;
	// at wsStaleBlocks the websocket client is replaced. Counting only heights
	// above wsHeight leaves alone a websocket that lags but keeps reporting,
	// and a catch-up that publishes several heights below the one just
	// announced.
	wsHeight        atomic.Int64
	publishedPastWS atomic.Int64

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

// NewBlockSubscriber creates a new block subscriber.
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
	if config.PollInterval == 0 {
		config.PollInterval = defaultPollInterval
	}

	cometClient, err := newCometClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create CometBFT client: %w", err)
	}

	return &BlockSubscriber{
		logger:      logging.ForComponent(logger, logging.ComponentBlockSubscriber),
		config:      config,
		cometClient: cometClient,
		reader:      &BlockReader{cometClient: cometClient, queryTimeout: config.QueryTimeout},
		wake:        make(chan struct{}, 1),
		subscribers: make(map[uint64]*subscriberInfo),
	}, nil
}

// newCometClient builds a CometBFT client for the configured endpoint.
func newCometClient(config BlockSubscriberConfig) (*http.HTTP, error) {
	if config.UseTLS {
		// Create a custom HTTP client with TLS configuration for secure connections
		// This is required for connecting to instances behind TLS
		httpClient := &stdhttp.Client{
			Transport: &stdhttp.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		}
		return http.NewWithClient(config.RPCEndpoint, "/websocket", httpClient)
	}
	return http.New(config.RPCEndpoint, "/websocket")
}

// BlockReader reads one block at a height from a node and keeps its canonical
// hash. The leader's BlockSubscriber reads every block through one; a miner
// worker holds one only to read a block hash the leader's record lacks. A block
// hash at a height is immutable once the block exists, so any node that has it
// gives the same answer: reading it is safe from any process, unlike the
// current height, which only the leader reads.
type BlockReader struct {
	cometClient  *http.HTTP
	queryTimeout time.Duration
}

// NewBlockReader builds a reader for the node at rpcEndpoint.
func NewBlockReader(rpcEndpoint string, useTLS bool) (*BlockReader, error) {
	config := BlockSubscriberConfig{RPCEndpoint: strings.TrimRight(rpcEndpoint, "/"), UseTLS: useTLS}
	cometClient, err := newCometClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create CometBFT client: %w", err)
	}
	return &BlockReader{cometClient: cometClient, queryTimeout: defaultQueryTimeout}, nil
}

// readBlock reads the block at height once.
func (r *BlockReader) readBlock(ctx context.Context, height int64) (*SimpleBlock, error) {
	queryCtx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	result, err := r.cometClient.Block(queryCtx, &height)
	if err != nil {
		return nil, fmt.Errorf("failed to query block at height %d: %w", height, err)
	}
	// CometBFT answers a missing block or block meta with no error and a nil
	// block; a proxy can answer another height. Neither is the block asked for.
	if result == nil || result.Block == nil || result.Block.Height != height || len(result.BlockID.Hash) == 0 {
		return nil, fmt.Errorf("node returned no usable block for height %d", height)
	}

	// CRITICAL: Use BlockID.Hash (canonical block ID) instead of Block.Hash() (computed hash)
	// This must match what the validator stores in ctx.HeaderHash() via StoreBlockHash()
	// See: poktroll/x/session/keeper/keeper.go:82
	return &SimpleBlock{
		height:    result.Block.Height,
		hash:      result.BlockID.Hash,
		timestamp: result.Block.Time,
	}, nil
}

// BlockAtHeight reads the block at height, retrying while the node answers
// that it does not have that height yet, until ctx ends.
func (r *BlockReader) BlockAtHeight(ctx context.Context, height int64) (client.Block, error) {
	return query.RetryWhileHeightNotYet(ctx, func() (client.Block, error) {
		return r.readBlock(ctx, height)
	})
}

// Start begins reading blocks: the websocket, the poll and the loop that
// reads and publishes each height.
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

	bs.wg.Add(3)
	go logging.RecoverGoRoutine(bs.logger, "block_subscriber_advance", bs.advanceLoop)(bs.ctx)
	go logging.RecoverGoRoutine(bs.logger, "block_subscriber_poll", bs.pollLoop)(bs.ctx)
	go logging.RecoverGoRoutine(bs.logger, "block_subscriber_websocket", bs.subscriptionLoop)(bs.ctx)

	bs.logger.Info().
		Str("rpc_endpoint", bs.config.RPCEndpoint).
		Str("query", newBlockHeaderQuery).
		Dur("poll_interval", bs.config.PollInterval).
		Msg("block subscriber started (websocket and latest-height poll)")

	return nil
}

// observe records that a source reported height and wakes the advance loop.
// It wakes it even when height is not new: a height the last pass could not
// read yet is retried on the next wake, and the poll is what guarantees one.
func (bs *BlockSubscriber) observe(height int64) {
	for {
		seen := bs.seenHeight.Load()
		if height <= seen || bs.seenHeight.CompareAndSwap(seen, height) {
			break
		}
	}
	select {
	case bs.wake <- struct{}{}:
	default:
	}
}

// advanceLoop is the only place blocks are read. On every wake it reads each
// height from the last published one up to the highest reported, in order,
// and stops at the first height it cannot read yet; the next wake -- the poll
// guarantees one per interval -- tries that height again.
func (bs *BlockSubscriber) advanceLoop(ctx context.Context) {
	defer bs.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-bs.wake:
		}
		bs.catchUp(ctx)
	}
}

// catchUp reads and publishes every height between the last published one and
// the highest reported one.
func (bs *BlockSubscriber) catchUp(ctx context.Context) {
	target := bs.seenHeight.Load()
	next := bs.nextHeight
	if next == 0 {
		if last := bs.lastBlock.Load(); last != nil {
			next = last.height + 1
		} else {
			next = target
		}
	}
	if target-next >= maxCatchUpBlocks {
		bs.logger.Warn().
			Int64("from_height", next).
			Int64("to_height", target).
			Int("max_catch_up_blocks", maxCatchUpBlocks).
			Msg("node reported a height far ahead of the last published one; skipping the oldest heights")
		next = target - maxCatchUpBlocks + 1
	}
	for height := next; height <= target; height++ {
		block, err := bs.reader.readBlock(ctx, height)
		if err != nil {
			if query.IsHeightNotYetAvailable(err) {
				bs.logger.Debug().Err(err).Int64("height", height).
					Msg("node does not have this height yet; retrying on the next wake")
				return
			}
			if height != bs.failingHeight {
				bs.failingHeight, bs.failures = height, 0
			}
			bs.failures++
			if bs.failures < maxReadFailures {
				bs.logger.Warn().Err(err).Int64("height", height).Int("failures", bs.failures).
					Msg("block read failed; retrying on the next wake")
				return
			}
			// A height that keeps failing for another reason must not stop
			// every height after it. A consumer that needs its hash reads it
			// from its own node.
			bs.logger.Warn().Err(err).Int64("height", height).Int("failures", bs.failures).
				Msg("block read keeps failing; skipping this height")
			bs.nextHeight = height + 1
			continue
		}
		bs.nextHeight = height + 1
		bs.lastBlock.Store(block)
		bs.publishToSubscribers(block)
		bs.notePublished(height)

		// Log if height changed (sampled: every 10th block to reduce verbosity)
		if block.height%10 == 0 {
			bs.logger.Debug().
				Int64("height", block.height).
				Time("block_time", block.timestamp).
				Msg("new block published")
		}
	}
}

// notePublished counts a published height the websocket has not reported.
func (bs *BlockSubscriber) notePublished(height int64) {
	if height > bs.wsHeight.Load() {
		bs.publishedPastWS.Add(1)
	}
}

// pollLoop reports the node's latest height once per interval, whatever the
// websocket is doing.
func (bs *BlockSubscriber) pollLoop(ctx context.Context) {
	defer bs.wg.Done()
	ticker := time.NewTicker(bs.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		queryCtx, cancel := context.WithTimeout(ctx, bs.config.QueryTimeout)
		status, err := bs.cometClient.Status(queryCtx)
		cancel()
		if err != nil {
			bs.logger.Debug().Err(err).Msg("latest-height poll failed")
			continue
		}
		bs.observe(status.SyncInfo.LatestBlockHeight)
	}
}

// subscriptionLoop keeps a websocket subscription open, replacing the client
// when it fails, when its channel closes, or when the advance loop reports it
// silent.
func (bs *BlockSubscriber) subscriptionLoop(ctx context.Context) {
	defer bs.wg.Done()

	reconnectDelay := reconnectBaseDelay

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := bs.runSubscription(ctx)
		if ctx.Err() != nil {
			bs.logger.Debug().Msg("websocket subscription closed (shutting down)")
			return
		}
		if err != nil {
			bs.logger.Warn().
				Err(err).
				Dur("retry_in", reconnectDelay).
				Msg("websocket subscription failed, will retry")
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
				reconnectDelay = bs.increaseBackoff(reconnectDelay)
			}
			continue
		}
		reconnectDelay = reconnectBaseDelay
	}
}

// runSubscription opens one websocket client, forwards its events until it
// must be replaced, and stops it. A nil error means the client was replaced on
// purpose (silence or a closed channel).
func (bs *BlockSubscriber) runSubscription(ctx context.Context) error {
	wsClient, err := newCometClient(bs.config)
	if err != nil {
		return fmt.Errorf("failed to create websocket client: %w", err)
	}
	if err = wsClient.Start(); err != nil {
		return fmt.Errorf("failed to start websocket client: %w", err)
	}
	defer func() {
		if stopErr := wsClient.Stop(); stopErr != nil {
			bs.logger.Debug().Err(stopErr).Msg("failed to stop websocket client")
		}
	}()

	eventsCh, err := wsClient.Subscribe(ctx, subscriptionClientID, newBlockHeaderQuery)
	if err != nil {
		return fmt.Errorf("failed to subscribe to block events: %w", err)
	}

	// A fresh client starts with the benefit of the doubt: silence is counted
	// from now.
	bs.publishedPastWS.Store(0)
	bs.logger.Info().Msg("websocket subscription established")
	staleCheck := time.NewTicker(bs.config.PollInterval)
	defer staleCheck.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-staleCheck.C:
			if bs.publishedPastWS.Load() < wsStaleBlocks {
				continue
			}
			bs.logger.Warn().
				Int("stale_blocks", wsStaleBlocks).
				Msg("websocket silent while the poll kept finding blocks; replacing the websocket client")
			return nil
		case resultEvent, ok := <-eventsCh:
			if !ok {
				bs.logger.Warn().Msg("websocket event channel closed; replacing the websocket client")
				return nil
			}
			if err := bs.handleBlockEvent(&resultEvent); err != nil {
				bs.logger.Error().
					Err(err).
					Str("event_type", fmt.Sprintf("%T", resultEvent.Data)).
					Msg("failed to handle block event")
			}
		}
	}
}

// handleBlockEvent records the height a NewBlockHeader event announces. It
// reads nothing: the advance loop reads the block, retrying a node that
// announced the height before it can serve it.
func (bs *BlockSubscriber) handleBlockEvent(resultEvent *coretypes.ResultEvent) error {
	blockHeader, ok := resultEvent.Data.(types.EventDataNewBlockHeader)
	if !ok {
		return fmt.Errorf("expected EventDataNewBlockHeader, got %T", resultEvent.Data)
	}
	height := blockHeader.Header.Height
	for {
		seen := bs.wsHeight.Load()
		if height <= seen || bs.wsHeight.CompareAndSwap(seen, height) {
			break
		}
	}
	bs.publishedPastWS.Store(0)
	bs.observe(height)
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
	if result == nil || result.Block == nil {
		return fmt.Errorf("failed to query block: the node answered no block")
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
		_ = bs.fetchLatestBlock(ctx) //nolint:errcheck // redundant: the two lines below re-read lastBlock and answer a zero block if it is still nil, which is the same outcome this error would have predicted
		block = bs.lastBlock.Load()
		if block == nil {
			// Return a zero block if still nil
			return &SimpleBlock{height: 0, hash: nil}
		}
	}
	return block
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

	// Cancelling stops the websocket loop, which stops its own client.
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
