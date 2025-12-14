//go:build test

package tx

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"testing"

	"cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
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
	mu       *testing.T
}

func (m *mockAuthQueryServer) Account(
	ctx context.Context,
	req *authtypes.QueryAccountRequest,
) (*authtypes.QueryAccountResponse, error) {
	m.mu.Helper()

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
	mu               *testing.T
	broadcastError   error
	broadcastCode    uint32
	broadcastRawLog  string
	broadcastTxHash  string
	broadcastCounter int
}

func (m *mockTxServiceServer) BroadcastTx(
	ctx context.Context,
	req *txtypes.BroadcastTxRequest,
) (*txtypes.BroadcastTxResponse, error) {
	m.mu.Helper()
	m.broadcastCounter++

	if m.broadcastError != nil {
		return nil, m.broadcastError
	}

	txHash := m.broadcastTxHash
	if txHash == "" {
		txHash = fmt.Sprintf("test-hash-%d", m.broadcastCounter)
	}

	return &txtypes.BroadcastTxResponse{
		TxResponse: &cosmostypes.TxResponse{
			Height:    100,
			TxHash:    txHash,
			Code:      m.broadcastCode,
			RawLog:    m.broadcastRawLog,
			Codespace: "sdk",
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
		mu:       t,
	}
	txServer := &mockTxServiceServer{
		mu: t,
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
	s.listener.Close()
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

// setBroadcastTxHash sets a specific tx hash for BroadcastTx response
func (s *testGRPCServer) setBroadcastTxHash(txHash string) {
	s.txServer.broadcastTxHash = txHash
}

// getBroadcastCount returns the number of times BroadcastTx was called
func (s *testGRPCServer) getBroadcastCount() int {
	return s.txServer.broadcastCounter
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

// generateTestAccount creates a test account
func generateTestAccount(t *testing.T, address string, accountNumber, sequence uint64) *authtypes.BaseAccount {
	t.Helper()

	return &authtypes.BaseAccount{
		Address:       address,
		AccountNumber: accountNumber,
		Sequence:      sequence,
	}
}

// parseGasPrice parses a gas price string for testing
func parseGasPrice(t *testing.T, price string) cosmostypes.DecCoin {
	t.Helper()

	gasPrice, err := cosmostypes.ParseDecCoin(price)
	require.NoError(t, err)
	return gasPrice
}

// assertAccountInCache verifies an account is cached with the expected sequence
func assertAccountInCache(t *testing.T, tc *TxClient, addr string, expectedSequence uint64) {
	t.Helper()

	tc.accountCacheMu.RLock()
	defer tc.accountCacheMu.RUnlock()

	account, ok := tc.accountCache[addr]
	require.True(t, ok, "account should be in cache")
	require.Equal(t, expectedSequence, account.Sequence)
}

// assertAccountNotInCache verifies an account is not in cache
func assertAccountNotInCache(t *testing.T, tc *TxClient, addr string) {
	t.Helper()

	tc.accountCacheMu.RLock()
	defer tc.accountCacheMu.RUnlock()

	_, ok := tc.accountCache[addr]
	require.False(t, ok, "account should not be in cache")
}

// createTestCodec creates a codec for testing
func createTestCodec() codec.Codec {
	registry := codectypes.NewInterfaceRegistry()
	authtypes.RegisterInterfaces(registry)
	cryptocodec.RegisterInterfaces(registry)
	prooftypes.RegisterInterfaces(registry)
	return codec.NewProtoCodec(registry)
}

// hexToPrivKey converts a hex string to a private key
func hexToPrivKey(t *testing.T, hexKey string) cryptotypes.PrivKey {
	t.Helper()

	// Remove 0x prefix if present
	if len(hexKey) > 2 && hexKey[:2] == "0x" {
		hexKey = hexKey[2:]
	}

	keyBytes, err := hex.DecodeString(hexKey)
	require.NoError(t, err)
	require.Equal(t, 32, len(keyBytes), "key must be 32 bytes")

	return &secp256k1.PrivKey{Key: keyBytes}
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
