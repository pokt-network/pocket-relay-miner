//go:build test

package tx

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"cosmossdk.io/math"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

// mockAuthQueryServer implements authtypes.QueryServer for testing
type mockAuthQueryServer struct {
	authtypes.UnimplementedQueryServer
	accounts map[string]*authtypes.BaseAccount
	t        *testing.T

	// Params is what the connection probe calls, so the test server has to
	// answer it: embedding UnimplementedQueryServer alone returns
	// codes.Unimplemented, which the probe classifies as a misconfigured
	// connection -- correctly, since that is what an endpoint that does not
	// serve the auth module looks like.
	paramsMu      sync.Mutex
	paramsCalls   int
	paramsErr     error
	paramsBlockCh chan struct{}
	paramsSeen    chan struct{}

	accountCalls   int
	accountBlockCh chan struct{}
	accountSeen    chan struct{}
}

// BlockAccount parks every later Account call until the returned channel is
// closed. It is how a test reaches the FIRST network call of the signing path,
// which is the one a deadline applied further down would not cover.
func (m *mockAuthQueryServer) BlockAccount() chan struct{} {
	ch := make(chan struct{})
	m.paramsMu.Lock()
	defer m.paramsMu.Unlock()
	m.accountBlockCh = ch
	return ch
}

// NotifyAccount returns a channel that receives once per Account call.
func (m *mockAuthQueryServer) NotifyAccount(buf int) chan struct{} {
	ch := make(chan struct{}, buf)
	m.paramsMu.Lock()
	defer m.paramsMu.Unlock()
	m.accountSeen = ch
	return ch
}

// AccountCalls reports how many account lookups reached the server.
func (m *mockAuthQueryServer) AccountCalls() int {
	m.paramsMu.Lock()
	defer m.paramsMu.Unlock()
	return m.accountCalls
}

func (m *mockAuthQueryServer) Params(
	ctx context.Context,
	_ *authtypes.QueryParamsRequest,
) (*authtypes.QueryParamsResponse, error) {
	m.paramsMu.Lock()
	m.paramsCalls++
	err, block, seen := m.paramsErr, m.paramsBlockCh, m.paramsSeen
	m.paramsMu.Unlock()

	if seen != nil {
		select {
		case seen <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &authtypes.QueryParamsResponse{Params: authtypes.DefaultParams()}, nil
}

// ParamsCalls reports how many probes have reached the server.
func (m *mockAuthQueryServer) ParamsCalls() int {
	m.paramsMu.Lock()
	defer m.paramsMu.Unlock()
	return m.paramsCalls
}

// SetParamsErr makes every later Params call fail with err.
func (m *mockAuthQueryServer) SetParamsErr(err error) {
	m.paramsMu.Lock()
	defer m.paramsMu.Unlock()
	m.paramsErr = err
}

// BlockParams makes every later Params call wait until the returned channel is
// closed, or until the caller's context expires.
func (m *mockAuthQueryServer) BlockParams() chan struct{} {
	ch := make(chan struct{})
	m.paramsMu.Lock()
	defer m.paramsMu.Unlock()
	m.paramsBlockCh = ch
	return ch
}

// NotifyParams returns a channel that receives once per Params call, so a test
// can wait for a probe instead of sleeping for one.
func (m *mockAuthQueryServer) NotifyParams(buf int) chan struct{} {
	ch := make(chan struct{}, buf)
	m.paramsMu.Lock()
	defer m.paramsMu.Unlock()
	m.paramsSeen = ch
	return ch
}

func (m *mockAuthQueryServer) Account(
	ctx context.Context,
	req *authtypes.QueryAccountRequest,
) (*authtypes.QueryAccountResponse, error) {
	m.t.Helper()

	m.paramsMu.Lock()
	block, seen := m.accountBlockCh, m.accountSeen
	m.accountCalls++
	m.paramsMu.Unlock()

	if seen != nil {
		select {
		case seen <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	account, ok := m.accounts[req.Address]
	if !ok {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("account %s not found", req.Address))
	}

	// Pack the account as Any
	anyAccount, err := codectypes.NewAnyWithValue(account)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to pack account: %v", err))
	}

	return &authtypes.QueryAccountResponse{
		Account: anyAccount,
	}, nil
}

// mockTxServiceServer implements txtypes.ServiceServer for testing
type mockTxServiceServer struct {
	txtypes.UnimplementedServiceServer
	t               *testing.T
	rwMu            sync.RWMutex // protects mutable fields below
	broadcastError  error
	broadcastCode   uint32
	broadcastRawLog string
	// broadcastCodespace overrides the codespace of a synthetic CheckTx
	// rejection. It defaults to "sdk" because that is where every code this
	// client classifies is registered -- but the codespace is HALF of what
	// identifies an error, so a test has to be able to send the same number from
	// somewhere else.
	broadcastCodespace string
	broadcastTxHash    string
	broadcastCounter   int
	getTxCounter       int // number of GetTx (post-broadcast inclusion) calls
	// getTxErr, when set, makes GetTx fail with it instead of echoing the
	// broadcast. Until it existed this mock could only ever answer "included and
	// this is its response", so a test wired to it could not exercise a tx that
	// is absent from the index, or a node whose indexer is off -- the two
	// answers the post-inclusion read exists to tell apart. Criteria written
	// against it were green by construction.
	getTxErr error

	// getTxByHash overrides the answer PER HASH. Without it this mock answers
	// the same thing for every hash, so the precedence between an entry's
	// original and resent hashes -- which one wins when they disagree -- could
	// not be exercised at all: any ordering would pass.
	getTxByHash map[string]mockGetTxAnswer
	lastTxBytes []byte // captured TxBytes from most recent BroadcastTx
	// captured TxBytes from the most recent Simulate. Its twin above is not
	// enough on its own: the two differ by design, and only comparing them can
	// show which fields a decision deliberately keeps out of the estimate.
	lastSimulateTxBytes []byte
	broadcastBlockCh    chan struct{}
	broadcastSeen       chan struct{}

	simulateCalls  int
	simulateGas    uint64
	simulateErrMsg string
	simulateMsgIdx int
	simulateHasIdx bool
}

// Simulate answers gas estimation, which is the DEFAULT production path:
// gas_limit is commented out in config.miner.example.yaml, so it is zero, so
// the client simulates. Without this the mock returns Unimplemented and every
// test has to set GasLimit > 0 -- i.e. the mode production does not use.
func (m *mockTxServiceServer) Simulate(
	_ context.Context,
	req *txtypes.SimulateRequest,
) (*txtypes.SimulateResponse, error) {
	m.rwMu.Lock()
	m.simulateCalls++
	// Captured for the same reason BroadcastTx captures its own: the simulated
	// transaction and the broadcast one are NOT the same bytes, and WHICH fields
	// differ is a deliberate decision rather than an accident. Until this
	// existed, only half the pair could be inspected -- a test could assert what
	// we send and had no way to assert what we simulated.
	m.lastSimulateTxBytes = append([]byte(nil), req.TxBytes...)
	errMsg, gas := m.simulateErrMsg, m.simulateGas
	msgIdx, hasIdx := m.simulateMsgIdx, m.simulateHasIdx
	m.rwMu.Unlock()

	if errMsg != "" {
		return nil, simulationFailure(errMsg, msgIdx, hasIdx)
	}
	if gas == 0 {
		gas = 50000
	}
	return &txtypes.SimulateResponse{
		GasInfo: &cosmostypes.GasInfo{GasWanted: gas, GasUsed: gas},
	}, nil
}

// simulationFailure reproduces the WHOLE wrapping chain a real node applies,
// which is the point of this helper existing.
//
// A mock that returned the keeper's bare text would certify a needle production
// never produces: by the time a simulation error reaches us it has been wrapped
// by baseapp (message index), flattened by the tx service (which appends
// "with gas used: 'N'", a number that differs every call) and carried over gRPC.
// A classifier tested against the bare string would look correct and match
// nothing in production.
//
// hasIndex false is the ante-handler shape: those decorators run in simulate
// too and fail BEFORE runMsgs, so their errors carry no message index at all.
func simulationFailure(serverMsg string, msgIndex int, hasIndex bool) error {
	inner := serverMsg
	if hasIndex {
		// baseapp.go:1052 -- errorsmod.Wrapf puts the wrap BEFORE the cause.
		inner = fmt.Sprintf("failed to execute message; message index: %d: %s", msgIndex, serverMsg)
	}
	// x/auth/tx/service.go:100 -- flattens to codes.Unknown and appends the gas.
	return status.Errorf(codes.Unknown, "%v with gas used: '%d'", inner, 42000)
}

// SimulateCalls reports how many simulations reached the server.
func (m *mockTxServiceServer) SimulateCalls() int {
	m.rwMu.RLock()
	defer m.rwMu.RUnlock()
	return m.simulateCalls
}

// FailSimulation makes every later Simulate fail with serverMsg, wrapped the
// way a real node wraps it. hasIndex false reproduces an ante-handler failure.
func (m *mockTxServiceServer) FailSimulation(serverMsg string, msgIndex int, hasIndex bool) {
	m.rwMu.Lock()
	defer m.rwMu.Unlock()
	m.simulateErrMsg = serverMsg
	m.simulateMsgIdx = msgIndex
	m.simulateHasIdx = hasIndex
}

// BlockBroadcast parks every later BroadcastTx until the returned channel is
// closed, so a test can hold a broadcast in flight.
func (m *mockTxServiceServer) BlockBroadcast() chan struct{} {
	ch := make(chan struct{})
	m.rwMu.Lock()
	defer m.rwMu.Unlock()
	m.broadcastBlockCh = ch
	return ch
}

// NotifyBroadcast returns a channel that receives once per BroadcastTx.
func (m *mockTxServiceServer) NotifyBroadcast(buf int) chan struct{} {
	ch := make(chan struct{}, buf)
	m.rwMu.Lock()
	defer m.rwMu.Unlock()
	m.broadcastSeen = ch
	return ch
}

func (m *mockTxServiceServer) BroadcastTx(
	ctx context.Context,
	req *txtypes.BroadcastTxRequest,
) (*txtypes.BroadcastTxResponse, error) {
	m.t.Helper()

	m.rwMu.Lock()
	m.broadcastCounter++
	counter := m.broadcastCounter
	broadcastErr := m.broadcastError
	txHash := m.broadcastTxHash
	code := m.broadcastCode
	rawLog := m.broadcastRawLog
	codespace := m.broadcastCodespace
	if codespace == "" {
		codespace = "sdk"
	}
	// Copy so later test assertions don't race with in-flight reuse of
	// the request buffer by the grpc server.
	m.lastTxBytes = append([]byte(nil), req.TxBytes...)
	block, seen := m.broadcastBlockCh, m.broadcastSeen
	m.rwMu.Unlock()

	if seen != nil {
		select {
		case seen <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if broadcastErr != nil {
		return nil, broadcastErr
	}

	if txHash == "" {
		txHash = fmt.Sprintf("test-hash-%d", counter)
	}

	return &txtypes.BroadcastTxResponse{
		TxResponse: &cosmostypes.TxResponse{
			Height:    100,
			TxHash:    txHash,
			Code:      code,
			RawLog:    rawLog,
			Codespace: codespace,
		},
	}, nil
}

// mockGetTxAnswer is what the mock replies for one hash: an error, or a code.
type mockGetTxAnswer struct {
	err    error
	code   uint32
	rawLog string
}

// SetGetTxForHash makes GetTx answer this hash specifically.
func (m *mockTxServiceServer) SetGetTxForHash(hash string, answer mockGetTxAnswer) {
	m.rwMu.Lock()
	defer m.rwMu.Unlock()
	if m.getTxByHash == nil {
		m.getTxByHash = map[string]mockGetTxAnswer{}
	}
	m.getTxByHash[hash] = answer
}

// SetGetTxErr makes every later GetTx fail with err. A nil err restores the
// echoing behaviour.
//
// The errors worth passing are the two a real node produces and the third that
// neither describes: status.Error(codes.NotFound, ...) for a hash the index does
// not hold, a plain error carrying "transaction indexing is disabled" for a node
// with tx_index=null, and anything else for the case the classifier must refuse
// to interpret.
func (m *mockTxServiceServer) SetGetTxErr(err error) {
	m.rwMu.Lock()
	defer m.rwMu.Unlock()
	m.getTxErr = err
}

// GetTx implements the GetTx method for testing TX commit verification
func (m *mockTxServiceServer) GetTx(
	ctx context.Context,
	req *txtypes.GetTxRequest,
) (*txtypes.GetTxResponse, error) {
	m.t.Helper()

	m.rwMu.Lock()
	m.getTxCounter++
	code := m.broadcastCode
	rawLog := m.broadcastRawLog
	codespace := m.broadcastCodespace
	if codespace == "" {
		codespace = "sdk"
	}
	getTxErr := m.getTxErr
	perHash, hasPerHash := m.getTxByHash[req.Hash]
	m.rwMu.Unlock()

	if hasPerHash {
		if perHash.err != nil {
			return nil, perHash.err
		}
		code = perHash.code
		rawLog = perHash.rawLog
	} else if getTxErr != nil {
		return nil, getTxErr
	}

	// Return the same response as broadcast - simulates successful TX execution
	// In production, this would query the blockchain for the TX by hash
	return &txtypes.GetTxResponse{
		TxResponse: &cosmostypes.TxResponse{
			Height:    100,
			TxHash:    req.Hash,
			Code:      code,
			RawLog:    rawLog,
			Codespace: codespace,
		},
	}, nil
}

// testGRPCServer encapsulates the test gRPC server setup
type testGRPCServer struct {
	server     *grpc.Server
	authServer *mockAuthQueryServer
	txServer   *mockTxServiceServer
	address    string
	listener   net.Listener
}

// setupMockGRPCServer creates a mock gRPC server for testing
func setupMockGRPCServer(t *testing.T) *testGRPCServer {
	t.Helper()

	// Create listener on a random port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	// Create gRPC server
	server := grpc.NewServer()

	// Create mock servers
	authServer := &mockAuthQueryServer{
		accounts: make(map[string]*authtypes.BaseAccount),
		t:        t,
	}
	txServer := &mockTxServiceServer{
		t: t,
	}

	// Register services
	authtypes.RegisterQueryServer(server, authServer)
	txtypes.RegisterServiceServer(server, txServer)

	// Start serving in background
	go func() {
		_ = server.Serve(listener)
	}()

	return &testGRPCServer{
		server:     server,
		authServer: authServer,
		txServer:   txServer,
		address:    listener.Addr().String(),
		listener:   listener,
	}
}

// cleanup stops the test server
func (s *testGRPCServer) cleanup() {
	s.server.Stop()
	_ = s.listener.Close()
}

// addAccount adds a test account to the mock auth server
func (s *testGRPCServer) addAccount(addr string, accountNumber, sequence uint64) {
	s.authServer.accounts[addr] = &authtypes.BaseAccount{
		Address:       addr,
		AccountNumber: accountNumber,
		Sequence:      sequence,
	}
}

// setBroadcastError sets an error to return from BroadcastTx
func (s *testGRPCServer) setBroadcastError(err error) {
	s.txServer.broadcastError = err
}

// setBroadcastFailure sets a non-zero code for BroadcastTx response
func (s *testGRPCServer) setBroadcastFailure(code uint32, rawLog string) {
	s.txServer.broadcastCode = code
	s.txServer.broadcastRawLog = rawLog
}

// setBroadcastFailureFrom is setBroadcastFailure with the codespace named.
// Needed because an ABCI code means nothing on its own: codes are registered
// PER CODESPACE, so the same number from another module is a different error,
// and a classifier that ignored the codespace would swallow it.
func (s *testGRPCServer) setBroadcastFailureFrom(codespace string, code uint32, rawLog string) {
	s.txServer.broadcastCode = code
	s.txServer.broadcastRawLog = rawLog
	s.txServer.broadcastCodespace = codespace
}

// getBroadcastCount returns the number of times BroadcastTx was called
func (s *testGRPCServer) getBroadcastCount() int {
	return s.txServer.broadcastCounter
}

// getGetTxCount returns the number of times GetTx (post-broadcast inclusion
// verification) was called. The SYNC submission path performs no such call,
// so this stays 0 — see TestSubmitProofs_SyncAcceptIsSuccess_NoInclusionCheck.
func (s *testGRPCServer) getGetTxCount() int {
	s.txServer.rwMu.RLock()
	defer s.txServer.rwMu.RUnlock()
	return s.txServer.getTxCounter
}

// getLastSimulateTxBytes returns a copy of the TxBytes from the most recent
// Simulate call, or nil when nothing was simulated.
//
// Nil is a meaningful answer and callers must check it: with an explicit
// GasLimit the client never simulates, so a test that forgot to leave the limit
// at zero would find nothing here and any assertion about the simulated
// transaction would pass by vacuity.
func (s *testGRPCServer) getLastSimulateTxBytes() []byte {
	s.txServer.rwMu.RLock()
	defer s.txServer.rwMu.RUnlock()
	if s.txServer.lastSimulateTxBytes == nil {
		return nil
	}
	return append([]byte(nil), s.txServer.lastSimulateTxBytes...)
}

// getLastTxBytes returns a copy of the most recently broadcast TxBytes.
// Used by tests that need to decode the outgoing tx to verify fields
// like TimeoutTimestamp that the client sets pre-broadcast.
func (s *testGRPCServer) getLastTxBytes() []byte {
	s.txServer.rwMu.RLock()
	defer s.txServer.rwMu.RUnlock()
	return append([]byte(nil), s.txServer.lastTxBytes...)
}

// generateTestKey generates a test private key
func generateTestKey(t *testing.T, operatorAddr string) cryptotypes.PrivKey {
	t.Helper()

	// Use a deterministic key based on operator address
	seed := []byte(operatorAddr)
	if len(seed) < 32 {
		// Pad to 32 bytes
		padded := make([]byte, 32)
		copy(padded, seed)
		seed = padded
	} else if len(seed) > 32 {
		seed = seed[:32]
	}

	return &secp256k1.PrivKey{Key: seed}
}

// setupTestKeyManager creates a test key manager with pre-loaded keys
func setupTestKeyManager(t *testing.T, addresses ...string) keys.KeyManager {
	t.Helper()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	provider := &mockKeyProvider{
		keys: make(map[string]cryptotypes.PrivKey),
	}

	for _, addr := range addresses {
		provider.keys[addr] = generateTestKey(t, addr)
	}

	km := keys.NewMultiProviderKeyManager(
		logger,
		[]keys.KeyProvider{provider},
		keys.KeyManagerConfig{
			HotReloadEnabled: false,
		},
	)

	ctx := context.Background()
	err := km.Start(ctx)
	require.NoError(t, err)

	return km
}

// mockKeyProvider implements KeyProvider for testing
type mockKeyProvider struct {
	keys map[string]cryptotypes.PrivKey
}

func (m *mockKeyProvider) Name() string {
	return "mock"
}

func (m *mockKeyProvider) Kind() string {
	return "mock"
}

func (m *mockKeyProvider) LoadKeys(ctx context.Context) (map[string]cryptotypes.PrivKey, error) {
	return m.keys, nil
}

func (m *mockKeyProvider) SupportsHotReload() bool {
	return false
}

func (m *mockKeyProvider) WatchForChanges(ctx context.Context) <-chan struct{} {
	return nil
}

func (m *mockKeyProvider) Close() error {
	return nil
}

// generateTestClaim creates a test claim message
func generateTestClaim(t *testing.T, supplierAddr, sessionID string) *prooftypes.MsgCreateClaim {
	t.Helper()

	// Create a minimal valid root hash (32 bytes)
	rootHash := make([]byte, 32)
	copy(rootHash, []byte(sessionID))

	return &prooftypes.MsgCreateClaim{
		SupplierOperatorAddress: supplierAddr,
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               sessionID,
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   200,
			ApplicationAddress:      "pokt1app123",
			ServiceId:               "ethereum",
		},
		RootHash: rootHash,
	}
}

// generateTestProof creates a test proof message
func generateTestProof(t *testing.T, supplierAddr, sessionID string) *prooftypes.MsgSubmitProof {
	t.Helper()

	// Create a minimal valid proof (empty but valid protobuf)
	proofBytes := []byte{0x0a, 0x00} // Empty bytes field in protobuf

	return &prooftypes.MsgSubmitProof{
		SupplierOperatorAddress: supplierAddr,
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId:               sessionID,
			SessionStartBlockHeight: 100,
			SessionEndBlockHeight:   200,
			ApplicationAddress:      "pokt1app123",
			ServiceId:               "ethereum",
		},
		Proof: proofBytes,
	}
}

// parseGasPrice parses a gas price string for testing
func parseGasPrice(t *testing.T, price string) cosmostypes.DecCoin {
	t.Helper()

	gasPrice, err := cosmostypes.ParseDecCoin(price)
	require.NoError(t, err)
	return gasPrice
}

// assertAccountNotInCache verifies an account is not in cache
func assertAccountNotInCache(t *testing.T, tc *TxClient, addr string) {
	t.Helper()

	tc.accountCacheMu.RLock()
	defer tc.accountCacheMu.RUnlock()

	_, ok := tc.accountCache[addr]
	require.False(t, ok, "account should not be in cache")
}

// calculateExpectedFee calculates the expected fee for a transaction
func calculateExpectedFee(gasLimit uint64, gasPrice cosmostypes.DecCoin) cosmostypes.Coins {
	gasLimitDec := math.LegacyNewDec(int64(gasLimit))
	feeAmount := gasPrice.Amount.Mul(gasLimitDec)

	// Truncate and add 1 if there's a remainder
	feeInt := feeAmount.TruncateInt()
	if feeAmount.Sub(math.LegacyNewDecFromInt(feeInt)).IsPositive() {
		feeInt = feeInt.Add(math.OneInt())
	}

	return cosmostypes.NewCoins(cosmostypes.NewCoin(gasPrice.Denom, feeInt))
}

// TestSupplierNode is the PRODUCTION HASupplierClient over this package's mock
// gRPC node, exported (behind the test build tag) so another package can drive
// the real client instead of a hand-written double. A double that merely
// satisfies an interface cannot reproduce a defect that lives in how the real
// client's calls interleave, which is exactly what the miner's submission tests
// need to observe.
type TestSupplierNode struct {
	Client *HASupplierClient
	srv    *testGRPCServer
}

// fixedBlockTime anchors every transaction the node signs at one chain time.
type fixedBlockTime struct{ t time.Time }

func (f fixedBlockTime) LatestBlockTime() time.Time { return f.t }

// NewTestSupplierNode starts a mock node, funds operatorAddr on it and returns
// a real HASupplierClient signing as that address. The gas limit is fixed so
// no simulation runs: every failure the caller arms happens at BROADCAST, after
// the transaction was signed, which is the stage that hands its bytes back.
func NewTestSupplierNode(t *testing.T, operatorAddr string) *TestSupplierNode {
	t.Helper()

	srv := setupMockGRPCServer(t)
	t.Cleanup(srv.cleanup)
	srv.addAccount(operatorAddr, 1, 0)

	km := setupTestKeyManager(t, operatorAddr)
	t.Cleanup(func() { _ = km.Close() })

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	tc, err := NewTxClient(logger, km, TxClientConfig{
		BlockTimeProvider: fixedBlockTime{t: time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)},
		GRPCEndpoint:      srv.address,
		ChainID:           "test-chain",
		GasLimit:          100000,
		ConnProbeInterval: time.Hour,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })

	return &TestSupplierNode{
		Client: NewHASupplierClient(tc, operatorAddr, logger),
		srv:    srv,
	}
}

// FailBroadcasts makes every later BroadcastTx fail with err, so each send is
// signed and then gets no answer.
func (n *TestSupplierNode) FailBroadcasts(err error) {
	n.srv.txServer.rwMu.Lock()
	defer n.srv.txServer.rwMu.Unlock()
	n.srv.txServer.broadcastError = err
}

// RefuseInCheckTx makes the node ANSWER every later broadcast with a refusal:
// the transaction was delivered and judged, which is a different outcome from
// FailBroadcasts (delivered or not, nobody said).
func (n *TestSupplierNode) RefuseInCheckTx(codespace string, code uint32, rawLog string) {
	n.srv.txServer.rwMu.Lock()
	defer n.srv.txServer.rwMu.Unlock()
	n.srv.txServer.broadcastCodespace = codespace
	n.srv.txServer.broadcastCode = code
	n.srv.txServer.broadcastRawLog = rawLog
}
