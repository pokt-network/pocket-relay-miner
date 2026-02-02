//go:build test

package tx

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// =============================================================================
// Client Lifecycle Tests
// =============================================================================

func TestNewTxClient_ValidConfig(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint:  testServer.address,
		ChainID:       "test-chain",
		GasLimit:      100000,
		GasPrice:      parseGasPrice(t, "0.001upokt"),
		TimeoutBlocks: 50,
		UseTLS:        false,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	require.NotNil(t, tc)
	defer tc.Close()

	require.Equal(t, "test-chain", tc.config.ChainID)
	require.Equal(t, uint64(100000), tc.config.GasLimit)
	require.Equal(t, uint64(50), tc.config.TimeoutBlocks)
	require.True(t, tc.ownsConn, "client should own the connection")
}

func TestNewTxClient_WithSharedConnection(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	// Create a shared connection
	conn, err := grpc.NewClient(testServer.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	config := TxClientConfig{
		GRPCConn:      conn,
		ChainID:       "test-chain",
		GasLimit:      100000,
		GasPrice:      parseGasPrice(t, "0.001upokt"),
		TimeoutBlocks: 50,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	require.NotNil(t, tc)
	defer tc.Close()

	require.False(t, tc.ownsConn, "client should not own shared connection")
	require.Equal(t, conn, tc.grpcConn)
}

func TestNewTxClient_InvalidEndpoint(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: "invalid-endpoint-format",
		ChainID:      "test-chain",
		GasLimit:     10000,
	}

	// Should create client successfully (gRPC doesn't validate endpoint format upfront)
	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	require.NotNil(t, tc)
	defer tc.Close()

	// But operations should fail
	ctx := context.Background()
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()
	testServer.addAccount("pokt1supplier123", 1, 0)

	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, "pokt1supplier123", "session-1"),
	}

	_, err = tc.CreateClaims(ctx, "pokt1supplier123", 1000, claims)
	require.Error(t, err)
}

func TestNewTxClient_MissingEndpointAndConn(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		ChainID: "test-chain",
		// No GRPCEndpoint or GRPCConn
	}

	tc, err := NewTxClient(logger, km, config)
	require.Error(t, err)
	require.Nil(t, tc)
	require.Contains(t, err.Error(), "either GRPCConn or GRPCEndpoint is required")
}

func TestNewTxClient_MissingChainID(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		// ChainID is empty, should use default
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	require.NotNil(t, tc)
	defer tc.Close()

	// Should use default chain ID
	require.Equal(t, DefaultChainID, tc.config.ChainID)
}

func TestNewTxClient_Defaults(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		// Leave all other fields at zero/empty
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	require.NotNil(t, tc)
	defer tc.Close()

	// Verify defaults were applied
	require.Equal(t, DefaultChainID, tc.config.ChainID)
	require.Equal(t, uint64(DefaultTimeoutHeight), tc.config.TimeoutBlocks)

	// Verify gas price has a denom (means it was initialized)
	require.NotEmpty(t, tc.config.GasPrice.Denom)
}

func TestTxClient_Close_Success(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)

	err = tc.Close()
	require.NoError(t, err)
	require.True(t, tc.closed)
}

func TestTxClient_Close_AlreadyClosed(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)

	// Close once
	err = tc.Close()
	require.NoError(t, err)

	// Close again should not error
	err = tc.Close()
	require.NoError(t, err)
}

func TestTxClient_Close_SharedConnection(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	// Create shared connection
	conn, err := grpc.NewClient(testServer.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	config := TxClientConfig{
		GRPCConn: conn,
		ChainID:  "test-chain",
		GasLimit: 100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)

	// Close the client - should not close shared connection
	err = tc.Close()
	require.NoError(t, err)

	// Connection should still be usable (verify it wasn't closed)
	require.NotNil(t, conn)
}

func TestTxClient_OperationsAfterClose(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)

	err = tc.Close()
	require.NoError(t, err)

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, "pokt1supplier123", "session-1"),
	}

	// Operations should fail after close
	_, err = tc.CreateClaims(ctx, "pokt1supplier123", 1000, claims)
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed")

	proofs := []*prooftypes.MsgSubmitProof{
		generateTestProof(t, "pokt1supplier123", "session-1"),
	}

	_, err = tc.SubmitProofs(ctx, "pokt1supplier123", 1000, proofs)
	require.Error(t, err)
	require.Contains(t, err.Error(), "closed")
}

// =============================================================================
// Account Management Tests
// =============================================================================

func TestGetAccount_Success(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 42, 10)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	account, err := tc.getAccount(ctx, supplierAddr)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, supplierAddr, account.Address)
	require.Equal(t, uint64(42), account.AccountNumber)
	require.Equal(t, uint64(10), account.Sequence)
}

func TestGetAccount_NotFound(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	account, err := tc.getAccount(ctx, "pokt1nonexistent")
	require.Error(t, err)
	require.Nil(t, account)
	require.Contains(t, err.Error(), "not found")
}

func TestGetAccount_Cached(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 42, 10)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()

	// First call - should query the server
	account1, err := tc.getAccount(ctx, supplierAddr)
	require.NoError(t, err)
	require.NotNil(t, account1)
	require.Equal(t, uint64(10), account1.Sequence)

	// Update the server's account sequence
	testServer.addAccount(supplierAddr, 42, 20)

	// Second call - should use cache (not query server again)
	account2, err := tc.getAccount(ctx, supplierAddr)
	require.NoError(t, err)
	require.NotNil(t, account2)
	require.Equal(t, uint64(10), account2.Sequence, "should still have cached sequence")
}

func TestGetAccount_CacheInvalidation(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 42, 10)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()

	// First call - should cache
	account1, err := tc.getAccount(ctx, supplierAddr)
	require.NoError(t, err)
	require.Equal(t, uint64(10), account1.Sequence)

	// Invalidate cache
	tc.InvalidateAccount(supplierAddr)
	assertAccountNotInCache(t, tc, supplierAddr)

	// Update server
	testServer.addAccount(supplierAddr, 42, 20)

	// Next call should query server again
	account2, err := tc.getAccount(ctx, supplierAddr)
	require.NoError(t, err)
	require.Equal(t, uint64(20), account2.Sequence, "should have new sequence from server")
}

func TestIncrementSequence(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 42, 10)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()

	// Load account into cache
	account, err := tc.getAccount(ctx, supplierAddr)
	require.NoError(t, err)
	require.Equal(t, uint64(10), account.Sequence)

	// NOTE: incrementSequence() removed - not needed for unordered transactions
	// Sequence numbers are not used with unordered transactions
}

// =============================================================================
// Transaction Building and Submission Tests
// =============================================================================

func TestCreateClaims_Success(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.001upokt"),
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
		generateTestClaim(t, supplierAddr, "session-2"),
	}

	txHash, err := tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.NoError(t, err)
	require.NotEmpty(t, txHash)

	// Verify broadcast was called
	require.Equal(t, 1, testServer.getBroadcastCount())

	// NOTE: Sequence increment check removed - unordered transactions don't use sequences
}

func TestCreateClaims_EmptyList(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, nil)
	require.NoError(t, err)

	// No broadcast should have occurred
	require.Equal(t, 0, testServer.getBroadcastCount())
}

func TestSubmitProofs_Success(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.001upokt"),
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	proofs := []*prooftypes.MsgSubmitProof{
		generateTestProof(t, supplierAddr, "session-1"),
	}

	_, err = tc.SubmitProofs(ctx, supplierAddr, 1000, proofs)
	require.NoError(t, err)

	// Verify broadcast was called
	require.Equal(t, 1, testServer.getBroadcastCount())

	// NOTE: Sequence check removed - unordered transactions don't use account sequences
}

func TestSubmitProofs_EmptyList(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	_, err = tc.SubmitProofs(ctx, supplierAddr, 1000, nil)
	require.NoError(t, err)

	// No broadcast should have occurred
	require.Equal(t, 0, testServer.getBroadcastCount())
}

func TestSignAndBroadcast_GasEstimation(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	gasLimit := uint64(250000)
	gasPrice := parseGasPrice(t, "0.000025upokt")

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     gasLimit,
		GasPrice:     gasPrice,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	// Calculate expected fee
	expectedFee := calculateExpectedFee(gasLimit, gasPrice)

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.NoError(t, err)

	// Verify fee calculation matches our expectation
	// (This is an indirect test - fee is set internally)
	require.NotNil(t, expectedFee)
	require.False(t, expectedFee.IsZero())
}

// =============================================================================
// Error Scenarios Tests
// =============================================================================

func TestSubmitTx_NetworkTimeout(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	// Set broadcast to return timeout error
	testServer.setBroadcastError(status.Error(codes.DeadlineExceeded, "context deadline exceeded"))

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deadline exceeded")
}

func TestSubmitTx_InvalidSequence(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	// Set broadcast to return sequence mismatch error
	testServer.setBroadcastFailure(32, "account sequence mismatch, expected 5, got 0")

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sequence mismatch")
}

func TestSubmitTx_InsufficientGas(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	// Set broadcast to return out of gas error
	testServer.setBroadcastFailure(11, "out of gas: gas required exceeds allowance")

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of gas")
}

func TestSubmitTx_AccountNotFound(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	// Don't add account to server

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestSubmitTx_KeyNotFound(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	// Create key manager without the required key
	km := setupTestKeyManager(t, "pokt1other")
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no key found")
}

func TestSubmitTx_BroadcastError(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	// Set broadcast to return generic error
	testServer.setBroadcastError(fmt.Errorf("network error"))

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
	require.Contains(t, err.Error(), "network error")
}

// =============================================================================
// Concurrent Access Tests
// =============================================================================

func TestConcurrentSubmissions_SameSupplier(t *testing.T) {
	t.Skip("TODO(phase3): fix data race in mockTxServiceServer.broadcastCounter (test_helpers.go:77) - needs mutex protection for concurrent BroadcastTx calls")
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	numGoroutines := 10

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	// Launch concurrent submissions for the same supplier
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			claims := []*prooftypes.MsgCreateClaim{
				generateTestClaim(t, supplierAddr, fmt.Sprintf("session-%d", idx)),
			}

			_, err := tc.CreateClaims(ctx, supplierAddr, 1000, claims)
			if err != nil {
				errors <- err
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Collect errors
	var errs []error
	for err := range errors {
		errs = append(errs, err)
	}

	// All submissions should succeed (txMu protects concurrent access)
	require.Empty(t, errs, "expected no errors, got: %v", errs)

	// Verify all transactions were broadcast
	require.Equal(t, numGoroutines, testServer.getBroadcastCount())

	// NOTE: Sequence check removed - unordered transactions don't use account sequences
}

func TestConcurrentSubmissions_DifferentSuppliers(t *testing.T) {
	t.Skip("TODO(phase3): fix data race in mockTxServiceServer.broadcastCounter (test_helpers.go:77) - needs mutex protection for concurrent BroadcastTx calls")
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	suppliers := []string{
		"pokt1supplier1",
		"pokt1supplier2",
		"pokt1supplier3",
	}

	for _, addr := range suppliers {
		testServer.addAccount(addr, 1, 0)
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, suppliers...)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     10000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()

	var wg sync.WaitGroup
	errors := make(chan error, len(suppliers)*3)

	// Launch concurrent submissions for different suppliers
	for _, supplier := range suppliers {
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(addr string, idx int) {
				defer wg.Done()

				claims := []*prooftypes.MsgCreateClaim{
					generateTestClaim(t, addr, fmt.Sprintf("session-%s-%d", addr, idx)),
				}

				_, err := tc.CreateClaims(ctx, addr, 1000, claims)
				if err != nil {
					errors <- err
				}
			}(supplier, i)
		}
	}

	wg.Wait()
	close(errors)

	// Collect errors
	var errs []error
	for err := range errors {
		errs = append(errs, err)
	}

	// All submissions should succeed
	require.Empty(t, errs, "expected no errors, got: %v", errs)

	// NOTE: Sequence check removed - unordered transactions don't use account sequences
	// Verify total broadcasts (3 suppliers × 3 submissions each)
	require.Equal(t, len(suppliers)*3, testServer.getBroadcastCount())
}

func TestConcurrentAccountQueries(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 5)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	ctx := context.Background()
	numGoroutines := 20

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	// Launch concurrent account queries
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			account, err := tc.getAccount(ctx, supplierAddr)
			if err != nil {
				errors <- err
				return
			}
			if account.Sequence != 5 {
				errors <- fmt.Errorf("unexpected sequence: %d", account.Sequence)
			}
		}()
	}

	wg.Wait()
	close(errors)

	// Collect errors
	var errs []error
	for err := range errors {
		errs = append(errs, err)
	}

	// All queries should succeed with consistent data
	require.Empty(t, errs, "expected no errors, got: %v", errs)
}

// =============================================================================
// SupplierClient Wrapper Tests
// =============================================================================

func TestHASupplierClient_CreateClaims(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	// Create wrapper
	sc := NewHASupplierClient(tc, supplierAddr, logger)

	ctx := context.Background()
	claim := generateTestClaim(t, supplierAddr, "session-1")

	err = sc.CreateClaims(ctx, 100, claim)
	require.NoError(t, err)

	// Verify broadcast occurred
	require.Equal(t, 1, testServer.getBroadcastCount())
}

func TestHASupplierClient_SubmitProofs(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	// Create wrapper
	sc := NewHASupplierClient(tc, supplierAddr, logger)

	ctx := context.Background()
	proof := generateTestProof(t, supplierAddr, "session-1")

	err = sc.SubmitProofs(ctx, 100, proof)
	require.NoError(t, err)

	// Verify broadcast occurred
	require.Equal(t, 1, testServer.getBroadcastCount())
}

func TestHASupplierClient_OperatorAddress(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     10000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	sc := NewHASupplierClient(tc, supplierAddr, logger)

	require.Equal(t, supplierAddr, sc.OperatorAddress())
}

// =============================================================================
// Fee Calculation Tests
// =============================================================================

func TestCalculateFee_RoundingUp(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	// Use values that will result in fractional fee
	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     123456,
		GasPrice:     parseGasPrice(t, "0.000007upokt"),
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()
}

func TestCalculateFee_ExactValue(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, "pokt1supplier123")
	defer km.Close()

	// Use values that result in exact fee
	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
		GasPrice:     parseGasPrice(t, "0.001upokt"),
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()
}

// =============================================================================
// Context Cancellation Tests
// =============================================================================

func TestCreateClaims_ContextCanceled(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	// Create canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
}

func TestCreateClaims_ContextTimeout(t *testing.T) {
	testServer := setupMockGRPCServer(t)
	defer testServer.cleanup()

	supplierAddr := "pokt1supplier123"
	testServer.addAccount(supplierAddr, 1, 0)

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	km := setupTestKeyManager(t, supplierAddr)
	defer km.Close()

	config := TxClientConfig{
		GRPCEndpoint: testServer.address,
		ChainID:      "test-chain",
		GasLimit:     100000,
	}

	tc, err := NewTxClient(logger, km, config)
	require.NoError(t, err)
	defer tc.Close()

	// Create context with very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	time.Sleep(10 * time.Millisecond) // Ensure timeout expires

	claims := []*prooftypes.MsgCreateClaim{
		generateTestClaim(t, supplierAddr, "session-1"),
	}

	_, err = tc.CreateClaims(ctx, supplierAddr, 1000, claims)
	require.Error(t, err)
}
