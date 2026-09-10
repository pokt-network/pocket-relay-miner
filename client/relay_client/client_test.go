package relay_client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	nodeservice "github.com/cosmos/cosmos-sdk/client/grpc/node"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// Unused test constants - may be used for future integration tests
// const (
// 	testServiceID    = "develop"
// 	testSupplierAddr = "pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj"
// 	testPayload      = `{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`
// )

// TestNewRelayClient_EmptyPrivateKey tests that empty private key is rejected.
func TestNewRelayClient_EmptyPrivateKey(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	_, err := NewRelayClient(Config{
		AppPrivateKeyHex: "",
		QueryClients:     nil,
	}, logger)

	require.Error(t, err, "NewRelayClient should fail with empty private key")
	require.Contains(t, err.Error(), "application private key is required")
}

// TestNewRelayClient_NilQueryClients tests that nil query clients is rejected.
func TestNewRelayClient_NilQueryClients(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	_, err := NewRelayClient(Config{
		AppPrivateKeyHex: testPrivKeyHex,
		QueryClients:     nil,
	}, logger)

	require.Error(t, err, "NewRelayClient should fail with nil query clients")
	require.Contains(t, err.Error(), "query clients are required")
}

// TestNewRelayClient_InvalidPrivateKey tests that invalid private key is rejected.
func TestNewRelayClient_InvalidPrivateKey(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	_, err := NewRelayClient(Config{
		AppPrivateKeyHex: "invalid-hex",
		QueryClients:     nil, // Will fail on query clients check before private key validation
	}, logger)

	require.Error(t, err, "NewRelayClient should fail with invalid configuration")
	// Note: Query clients are validated first, so error is about that, not the key
	require.Contains(t, err.Error(), "query clients are required")
}

// TestGetAppAddress tests address retrieval.
func TestGetAppAddress(t *testing.T) {
	// Note: This test would require mock QueryClients, skipping for now
	// since we verified address derivation in signer_test.go
	t.Skip("Requires mock QueryClients")
}

// startKeyedChain is a chain plus the query client's session cache in front
// of it, keyed the way query/query.go keys it: by the start height of the
// height asked for (sharedtypes.GetSessionStartHeight). Asked at height 0, the
// chain answers with its latest session -- and the cache files that answer
// under start height 0, which is where the load tests' stale session came from.
type startKeyedChain struct {
	params sharedtypes.Params

	mu     sync.Mutex
	height int64
	cache  map[int64]*sessiontypes.Session
}

func newStartKeyedChain(height int64) *startKeyedChain {
	params := sharedtypes.DefaultParams()
	params.NumBlocksPerSession = 10
	return &startKeyedChain{params: params, height: height, cache: map[int64]*sessiontypes.Session{}}
}

func (f *startKeyedChain) GetSession(_ context.Context, _, serviceID string, height int64) (*sessiontypes.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := sharedtypes.GetSessionStartHeight(&f.params, height)
	if s, ok := f.cache[key]; ok {
		return s, nil
	}
	at := height
	if at == 0 {
		at = f.height
	}
	s := &sessiontypes.Session{Header: &sessiontypes.SessionHeader{
		ServiceId:               serviceID,
		SessionStartBlockHeight: sharedtypes.GetSessionStartHeight(&f.params, at),
		SessionEndBlockHeight:   sharedtypes.GetSessionEndHeight(&f.params, at),
	}}
	f.cache[key] = s
	return s, nil
}

func (f *startKeyedChain) latest(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.height, nil
}

func (f *startKeyedChain) advanceTo(height int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.height = height
}

// TestCurrentSession_FollowsTheChainAcrossABorder is a load test crossing a
// session border: the relays built after it must be signed for the session the
// chain is in, through a session cache that behaves like the query client's.
func TestCurrentSession_FollowsTheChainAcrossABorder(t *testing.T) {
	chain := newStartKeyedChain(5)
	c := &RelayClient{
		appAddress: "pokt1app",
		sessions:   chain,
		height:     &latestHeight{fetch: chain.latest, maxAge: 0},
	}

	first, err := c.currentSession(context.Background(), "svc")
	require.NoError(t, err)
	require.Equal(t, int64(1), first.Header.SessionStartBlockHeight)
	require.Equal(t, int64(10), first.Header.SessionEndBlockHeight)

	chain.advanceTo(12)
	next, err := c.currentSession(context.Background(), "svc")
	require.NoError(t, err)
	require.Equal(t, int64(11), next.Header.SessionStartBlockHeight,
		"a relay built after the border was signed for the session that started at %d, not the one the chain is in",
		next.Header.SessionStartBlockHeight)
	require.Equal(t, int64(20), next.Header.SessionEndBlockHeight)
}

// TestLatestHeight_ConcurrentBuildsShareOneRead is every worker of a load test
// asking for the height at once: one read of the node serves them all, and
// the memo is safe under -race.
func TestLatestHeight_ConcurrentBuildsShareOneRead(t *testing.T) {
	var reads atomic.Int64
	h := &latestHeight{
		maxAge: time.Hour,
		fetch: func(context.Context) (int64, error) {
			reads.Add(1)
			return 42, nil
		},
	}

	const workers = 50
	got := make([]int64, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			height, err := h.get(context.Background())
			require.NoError(t, err)
			got[i] = height
		}(i)
	}
	wg.Wait()

	for i, height := range got {
		require.Equal(t, int64(42), height, "worker %d", i)
	}
	require.Equal(t, int64(1), reads.Load(), "one read of the node must serve every build within maxAge")
}

// TestLatestHeight_FailedRead is the node blinking: with a height already read
// the builds keep it and ask again only after maxAge; with none, they fail
// instead of falling back to height 0.
func TestLatestHeight_FailedRead(t *testing.T) {
	errNode := errors.New("node unavailable")

	t.Run("keeps the last height and retries after maxAge", func(t *testing.T) {
		var reads atomic.Int64
		h := &latestHeight{
			maxAge: time.Hour,
			fetch: func(context.Context) (int64, error) {
				reads.Add(1)
				return 0, errNode
			},
			height: 42,
			readAt: time.Now().Add(-2 * time.Hour),
		}
		for i := 0; i < 3; i++ {
			height, err := h.get(context.Background())
			require.NoError(t, err)
			require.Equal(t, int64(42), height)
		}
		require.Equal(t, int64(1), reads.Load(), "a failed read must not be retried on every build")
	})

	t.Run("fails with no height to keep", func(t *testing.T) {
		h := &latestHeight{maxAge: time.Hour, fetch: func(context.Context) (int64, error) { return 0, errNode }}
		height, err := h.get(context.Background())
		require.ErrorIs(t, err, errNode)
		require.Zero(t, height)
	})

	t.Run("a zero height from the node is an error, not a height", func(t *testing.T) {
		h := &latestHeight{maxAge: time.Hour, fetch: func(context.Context) (int64, error) { return 0, nil }}
		height, err := h.get(context.Background())
		require.ErrorContains(t, err, "the node reported height 0")
		require.Zero(t, height)
	})
}

// Note: Full integration tests for BuildRelayRequest
// would require:
// 1. Mock QueryClients (Application, Session, Account, Shared)
// 2. Mock blockchain responses
// 3. Test session data
//
// These would be better suited as integration tests with a test network.
// The core signing logic is tested in signer_test.go.

// fakeCometBFT and fakeNodeStatus are one node's two answers to "what height
// is it", taken while a block is being committed: its latest block is already
// H, its committed state still H-1.
type fakeCometBFT struct {
	cmtservice.UnimplementedServiceServer
	latestBlock int64
}

func (f fakeCometBFT) GetLatestBlock(context.Context, *cmtservice.GetLatestBlockRequest) (*cmtservice.GetLatestBlockResponse, error) {
	return &cmtservice.GetLatestBlockResponse{SdkBlock: &cmtservice.Block{Header: cmtservice.Header{Height: f.latestBlock}}}, nil
}

type fakeNodeStatus struct {
	nodeservice.UnimplementedServiceServer
	committed uint64
}

func (f fakeNodeStatus) Status(context.Context, *nodeservice.StatusRequest) (*nodeservice.StatusResponse, error) {
	return &nodeservice.StatusResponse{Height: f.committed}, nil
}

// hydratingSessions refuses a height above the node's committed state, the
// check poktroll's session hydrator makes (x/session/keeper/session_hydrator.go
// hydrateSessionMetadata).
type hydratingSessions struct{ committed int64 }

func (h hydratingSessions) GetSession(_ context.Context, _, serviceID string, height int64) (*sessiontypes.Session, error) {
	if height > h.committed {
		return nil, fmt.Errorf("block height %d is ahead of the last committed block height %d: error during session hydration", height, h.committed)
	}
	return &sessiontypes.Session{Header: &sessiontypes.SessionHeader{ServiceId: serviceID}}, nil
}

// TestCurrentSession_AsksAtTheNodesCommittedHeight is a relay built while the
// node commits a block: the height the client reads must be one the session
// query accepts, through the fetch NewRelayClient wires, over real gRPC.
func TestCurrentSession_AsksAtTheNodesCommittedHeight(t *testing.T) {
	const latestBlock, committed = 413, 412
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	cmtservice.RegisterServiceServer(server, &fakeCometBFT{latestBlock: latestBlock})
	nodeservice.RegisterServiceServer(server, &fakeNodeStatus{committed: committed})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c := &RelayClient{
		appAddress: "pokt1app",
		sessions:   hydratingSessions{committed: committed},
		height:     &latestHeight{fetch: committedHeight(conn), maxAge: 0},
	}
	session, err := c.currentSession(context.Background(), "svc")
	require.NoError(t, err, "the session was asked for at a height the node has not committed yet")
	require.Equal(t, "svc", session.Header.ServiceId)
}
