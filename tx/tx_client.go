package tx

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
)

const (
	// DefaultGasPrice is the default gas price in upokt.
	DefaultGasPrice = "0.000001upokt"

	// DefaultGasAdjustment is the default multiplier for simulated gas.
	// Applied when GasLimit=0 (auto) to add safety margin: actual_gas = simulated_gas * adjustment
	DefaultGasAdjustment = 1.7

	// DefaultTimeoutHeight is the number of blocks after which a transaction times out.
	DefaultTimeoutHeight = 100

	// DefaultChainID for the pocket network.
	DefaultChainID = "pocket"
)

// TxClientConfig contains configuration for the transaction client.
type TxClientConfig struct {
	// GRPCEndpoint is the gRPC endpoint for the full node.
	// Only used if GRPCConn is nil.
	GRPCEndpoint string

	// GRPCConn is an existing gRPC connection to reuse.
	// If provided, GRPCEndpoint and UseTLS are ignored.
	// The caller is responsible for closing this connection.
	GRPCConn *grpc.ClientConn

	// ChainID is the chain ID of the network.
	ChainID string

	// GasLimit is the gas limit for transactions.
	// Set to 0 for automatic gas estimation (simulation).
	// Set to a positive value for a fixed gas limit.
	// No default - must be explicitly configured (0 for auto, or explicit value)
	GasLimit uint64

	// GasPrice is the gas price for transactions.
	GasPrice cosmostypes.DecCoin

	// GasAdjustment is the multiplier applied to simulated gas to add safety margin.
	// Only used when GasLimit=0 (automatic simulation).
	// Actual gas = simulated_gas * GasAdjustment
	// Default: 1.7 (adds 70% safety margin)
	GasAdjustment float64

	// TimeoutBlocks is the number of blocks after which a transaction times out.
	TimeoutBlocks uint64

	// UseTLS enables TLS for the gRPC connection.
	// Set to true when connecting to endpoints on port 443 or with TLS enabled.
	// Only used if GRPCConn is nil.
	// Default: false (insecure connection)
	UseTLS bool
}

// TxClient provides transaction submission capabilities for the HA system.
// It supports multi-supplier signing using private keys from the KeyManager.
type TxClient struct {
	logger     logging.Logger
	config     TxClientConfig
	keyManager keys.KeyManager
	grpcConn   *grpc.ClientConn
	ownsConn   bool // true if we created the connection and should close it

	// Codec for encoding/decoding transactions
	codec       codec.Codec
	txConfig    client.TxConfig
	authQuerier authtypes.QueryClient
	txClient    txtypes.ServiceClient

	// Per-supplier account info cache
	accountCache   map[string]*authtypes.BaseAccount
	accountCacheMu sync.RWMutex

	// Mutex to prevent concurrent transactions
	txMu sync.Mutex

	// Lifecycle
	closed bool
	mu     sync.RWMutex
}

// NewTxClient creates a new transaction client.
func NewTxClient(
	logger logging.Logger,
	keyManager keys.KeyManager,
	config TxClientConfig,
) (*TxClient, error) {
	// Validate: either GRPCConn or GRPCEndpoint must be provided
	if config.GRPCConn == nil && config.GRPCEndpoint == "" {
		return nil, fmt.Errorf("either GRPCConn or GRPCEndpoint is required")
	}
	if config.ChainID == "" {
		config.ChainID = DefaultChainID
	}
	// GasLimit: No default applied - 0 means automatic (simulation), non-zero means explicit limit
	// Check Denom instead of IsZero() since zero-value DecCoin has nil internal state
	if config.GasPrice.Denom == "" {
		gasPrice, err := cosmostypes.ParseDecCoin(DefaultGasPrice)
		if err != nil {
			return nil, fmt.Errorf("failed to parse default gas price: %w", err)
		}
		config.GasPrice = gasPrice
	}
	if config.TimeoutBlocks == 0 {
		config.TimeoutBlocks = DefaultTimeoutHeight
	}
	if config.GasAdjustment == 0 {
		config.GasAdjustment = DefaultGasAdjustment
	}

	var grpcConn *grpc.ClientConn
	var ownsConn bool

	if config.GRPCConn != nil {
		// Use the provided connection (caller owns it)
		grpcConn = config.GRPCConn
		ownsConn = false
	} else {
		// Create our own connection
		var transportCreds credentials.TransportCredentials
		if config.UseTLS {
			transportCreds = credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS12,
			})
		} else {
			transportCreds = insecure.NewCredentials()
		}

		var err error
		grpcConn, err = grpc.NewClient(
			config.GRPCEndpoint,
			grpc.WithTransportCredentials(transportCreds),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
		}
		ownsConn = true
	}

	// Create codec and tx config
	cdc, txConfig := createCodecAndTxConfig()

	tc := &TxClient{
		logger:       logging.ForComponent(logger, logging.ComponentTxClient),
		config:       config,
		keyManager:   keyManager,
		grpcConn:     grpcConn,
		ownsConn:     ownsConn,
		codec:        cdc,
		txConfig:     txConfig,
		authQuerier:  authtypes.NewQueryClient(grpcConn),
		txClient:     txtypes.NewServiceClient(grpcConn),
		accountCache: make(map[string]*authtypes.BaseAccount),
	}

	tc.logger.Info().
		Str("endpoint", config.GRPCEndpoint).
		Str("chain_id", config.ChainID).
		Bool("shared_conn", !ownsConn).
		Msg("transaction client initialized")

	return tc, nil
}

// createCodecAndTxConfig creates the codec and transaction config for signing.
func createCodecAndTxConfig() (codec.Codec, client.TxConfig) {
	registry := codectypes.NewInterfaceRegistry()

	// Register necessary interfaces
	authtypes.RegisterInterfaces(registry)
	cryptocodec.RegisterInterfaces(registry)
	prooftypes.RegisterInterfaces(registry)
	sessiontypes.RegisterInterfaces(registry)

	cdc := codec.NewProtoCodec(registry)
	txConfig := authtx.NewTxConfig(cdc, authtx.DefaultSignModes)

	return cdc, txConfig
}

// CreateClaims creates and submits claim transactions for a supplier.
// Returns the TX hash for deduplication tracking.
func (tc *TxClient) CreateClaims(
	ctx context.Context,
	supplierOperatorAddr string,
	timeoutHeight int64,
	claims []*prooftypes.MsgCreateClaim,
) (string, error) {
	tc.mu.RLock()
	if tc.closed {
		tc.mu.RUnlock()
		return "", fmt.Errorf("tx client is closed")
	}
	tc.mu.RUnlock()

	if len(claims) == 0 {
		return "", nil
	}

	// Convert claims to Msg interface
	msgs := make([]cosmostypes.Msg, len(claims))
	for i, claim := range claims {
		msgs[i] = claim
	}

	txHash, err := tc.signAndBroadcast(ctx, supplierOperatorAddr, uint64(timeoutHeight), "claim", msgs...)
	if err != nil {
		txClaimErrors.WithLabelValues(supplierOperatorAddr, "broadcast").Inc()
		return "", fmt.Errorf("failed to broadcast claims: %w", err)
	}

	tc.logger.Info().
		Str("supplier", supplierOperatorAddr).
		Int("num_claims", len(claims)).
		Str("tx_hash", txHash).
		Msg("claims submitted")

	txClaimsSubmitted.WithLabelValues(supplierOperatorAddr).Add(float64(len(claims)))
	return txHash, nil
}

// SubmitProofs submits proof transactions for a supplier.
// Returns the TX hash for deduplication tracking.
func (tc *TxClient) SubmitProofs(
	ctx context.Context,
	supplierOperatorAddr string,
	timeoutHeight int64,
	proofs []*prooftypes.MsgSubmitProof,
) (string, error) {
	tc.mu.RLock()
	if tc.closed {
		tc.mu.RUnlock()
		return "", fmt.Errorf("tx client is closed")
	}
	tc.mu.RUnlock()

	if len(proofs) == 0 {
		return "", nil
	}

	// Convert proofs to Msg interface
	msgs := make([]cosmostypes.Msg, len(proofs))
	for i, proof := range proofs {
		msgs[i] = proof
	}

	txHash, err := tc.signAndBroadcast(ctx, supplierOperatorAddr, uint64(timeoutHeight), "proof", msgs...)
	if err != nil {
		txProofErrors.WithLabelValues(supplierOperatorAddr, "broadcast").Inc()
		return "", fmt.Errorf("failed to broadcast proofs: %w", err)
	}

	tc.logger.Info().
		Str("supplier", supplierOperatorAddr).
		Int("num_proofs", len(proofs)).
		Str("tx_hash", txHash).
		Msg("proofs submitted")

	txProofsSubmitted.WithLabelValues(supplierOperatorAddr).Add(float64(len(proofs)))
	return txHash, nil
}

// signAndBroadcast signs and broadcasts a transaction.
// txType should be "claim" or "proof" for proper metrics labeling.
func (tc *TxClient) signAndBroadcast(
	ctx context.Context,
	signerAddr string,
	timeoutHeight uint64,
	txType string,
	msgs ...cosmostypes.Msg,
) (string, error) {
	tc.txMu.Lock()
	defer tc.txMu.Unlock()

	startTime := time.Now()
	defer func() {
		txBroadcastLatency.WithLabelValues(signerAddr).Observe(time.Since(startTime).Seconds())
	}()

	// Get signing key
	privKey, err := tc.keyManager.GetSigner(signerAddr)
	if err != nil {
		return "", fmt.Errorf("failed to get signing key: %w", err)
	}

	// Get account info
	account, err := tc.getAccount(ctx, signerAddr)
	if err != nil {
		return "", fmt.Errorf("failed to get account: %w", err)
	}

	// Build the transaction
	txBuilder := tc.txConfig.NewTxBuilder()
	if setMsgsErr := txBuilder.SetMsgs(msgs...); setMsgsErr != nil {
		return "", fmt.Errorf("failed to set messages: %w", setMsgsErr)
	}

	// Determine gas limit and fees
	var gasLimit uint64
	var feeAmount cosmostypes.Coins

	if tc.config.GasLimit == 0 {
		// Automatic gas estimation: simulate transaction to estimate gas
		simGas, simErr := tc.simulateTx(ctx, txBuilder, privKey, account)
		if simErr != nil {
			// Simulation failed and no fallback gas limit configured
			return "", fmt.Errorf("gas simulation failed (gas_limit=0 requires successful simulation): %w", simErr)
		}

		// Apply gas adjustment for safety margin
		gasLimit = uint64(float64(simGas) * tc.config.GasAdjustment)
		tc.logger.Debug().
			Str("supplier", signerAddr).
			Uint64("simulated_gas", simGas).
			Float64("gas_adjustment", tc.config.GasAdjustment).
			Uint64("final_gas_limit", gasLimit).
			Msg("gas simulation succeeded")

		feeAmount = tc.calculateFeeForGas(gasLimit)
	} else {
		// Use explicit gas limit
		gasLimit = tc.config.GasLimit
		feeAmount = tc.calculateFee()
	}

	// Set gas limit and fees
	txBuilder.SetGasLimit(gasLimit)
	txBuilder.SetFeeAmount(feeAmount)

	// Set memo (optional)
	txBuilder.SetMemo("HA RelayMiner")

	// Set unordered=true to eliminate account sequence issues
	// With unordered, TXs don't check sequence numbers and can be included in any order
	txBuilder.SetUnordered(true)

	// Set timeout timestamp (required for unordered TXs)
	// Cosmos SDK requires time.Time for unordered TXs
	// Use 2 minute timeout - sufficient for TX inclusion
	// The timeoutHeight parameter is ignored for unordered TXs (only used for logging)
	timeoutDuration := 2 * time.Minute
	timeoutTimestamp := time.Now().Add(timeoutDuration)
	txBuilder.SetTimeoutTimestamp(timeoutTimestamp)

	// Sign the transaction (unordered=true means sequence=0)
	err = tc.signTx(ctx, txBuilder, privKey, account, true)
	if err != nil {
		return "", fmt.Errorf("failed to sign transaction: %w", err)
	}

	// Encode the transaction
	txBytes, err := tc.txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return "", fmt.Errorf("failed to encode transaction: %w", err)
	}

	// Broadcast in SYNC mode (returns after CheckTx, fast)
	// Using unordered eliminates sequence mismatch issues
	// Duplicate protection handled by caller via Redis tracking
	res, err := tc.txClient.BroadcastTx(ctx, &txtypes.BroadcastTxRequest{
		TxBytes: txBytes,
		Mode:    txtypes.BroadcastMode_BROADCAST_MODE_SYNC,
	})
	if err != nil {
		return "", fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	txHash := res.TxResponse.TxHash

	// Check result (SYNC mode returns CheckTx result only)
	if res.TxResponse.Code != 0 {
		// CheckTx failed
		if isInsufficientBalanceError(res.TxResponse.RawLog) {
			txInsufficientBalanceErrors.WithLabelValues(signerAddr).Inc()
		}

		if isSequenceMismatchError(res.TxResponse.RawLog) {
			// Should NOT happen with unordered=true, but handle anyway
			tc.logger.Warn().
				Str("supplier", signerAddr).
				Str("tx_type", txType).
				Str("error", res.TxResponse.RawLog).
				Msg("sequence mismatch with unordered TX (unexpected)")
			tc.InvalidateAccount(signerAddr)
		}

		tc.logger.Warn().
			Str("supplier", signerAddr).
			Str("tx_type", txType).
			Str("tx_hash", txHash).
			Uint32("code", res.TxResponse.Code).
			Str("error", res.TxResponse.RawLog).
			Msg("transaction CheckTx failed")

		return txHash, fmt.Errorf("CheckTx failed (code %d): %s", res.TxResponse.Code, res.TxResponse.RawLog)
	}

	// CheckTx passed! TX accepted to mempool
	tc.logger.Info().
		Str("supplier", signerAddr).
		Str("tx_type", txType).
		Str("tx_hash", txHash).
		Time("timeout_timestamp", timeoutTimestamp).
		Msg("transaction accepted to mempool (unordered)")

	// NOTE: We don't increment sequence for unordered TXs (they don't use sequence numbers)

	txBroadcastsTotal.WithLabelValues(signerAddr, "success").Inc()
	return txHash, nil
}

// signTx signs a transaction with the given private key.
func (tc *TxClient) signTx(
	ctx context.Context,
	txBuilder client.TxBuilder,
	privKey cryptotypes.PrivKey,
	account *authtypes.BaseAccount,
	unordered bool,
) error {
	pubKey := privKey.PubKey()
	signMode := signing.SignMode_SIGN_MODE_DIRECT

	// For unordered transactions, sequence MUST be 0
	sequence := account.Sequence
	if unordered {
		sequence = 0
	}

	// Set signature info placeholder
	sigV2 := signing.SignatureV2{
		PubKey: pubKey,
		Data: &signing.SingleSignatureData{
			SignMode:  signMode,
			Signature: nil,
		},
		Sequence: sequence,
	}

	if err := txBuilder.SetSignatures(sigV2); err != nil {
		return fmt.Errorf("failed to set signature placeholder: %w", err)
	}

	// Build sign data
	signerData := authsigning.SignerData{
		ChainID:       tc.config.ChainID,
		AccountNumber: account.AccountNumber,
		Sequence:      sequence,
		PubKey:        pubKey,
		Address:       account.Address,
	}

	// Get bytes to sign using the sign mode handler
	bytesToSign, err := authsigning.GetSignBytesAdapter(
		ctx,
		tc.txConfig.SignModeHandler(),
		signMode,
		signerData,
		txBuilder.GetTx(),
	)
	if err != nil {
		return fmt.Errorf("failed to get sign bytes: %w", err)
	}

	// Sign
	signature, err := privKey.Sign(bytesToSign)
	if err != nil {
		return fmt.Errorf("failed to sign: %w", err)
	}

	// Set the actual signature
	sigV2.Data = &signing.SingleSignatureData{
		SignMode:  signMode,
		Signature: signature,
	}

	if err := txBuilder.SetSignatures(sigV2); err != nil {
		return fmt.Errorf("failed to set signature: %w", err)
	}

	return nil
}

// getAccount retrieves account info from chain or cache.
func (tc *TxClient) getAccount(ctx context.Context, addr string) (*authtypes.BaseAccount, error) {
	// Check cache first
	tc.accountCacheMu.RLock()
	if account, ok := tc.accountCache[addr]; ok {
		tc.accountCacheMu.RUnlock()
		return account, nil
	}
	tc.accountCacheMu.RUnlock()

	// Query chain
	tc.accountCacheMu.Lock()
	defer tc.accountCacheMu.Unlock()

	// Double-check after acquiring lock
	if account, ok := tc.accountCache[addr]; ok {
		return account, nil
	}

	res, err := tc.authQuerier.Account(ctx, &authtypes.QueryAccountRequest{
		Address: addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query account: %w", err)
	}

	var account authtypes.BaseAccount
	if err := tc.codec.UnpackAny(res.Account, &account); err != nil {
		// Try unpacking as BaseAccount directly
		if err := account.Unmarshal(res.Account.Value); err != nil {
			return nil, fmt.Errorf("failed to unpack account: %w", err)
		}
	}

	tc.accountCache[addr] = &account
	return &account, nil
}

// NOTE: incrementSequence() removed - not needed for unordered transactions in SYNC mode

// InvalidateAccount removes an account from the cache.
func (tc *TxClient) InvalidateAccount(addr string) {
	tc.accountCacheMu.Lock()
	defer tc.accountCacheMu.Unlock()
	delete(tc.accountCache, addr)
}

// NOTE: waitForTxCommit() removed - not needed in SYNC mode (doesn't wait for commit)

// simulateTx simulates a transaction to estimate gas usage.
func (tc *TxClient) simulateTx(
	ctx context.Context,
	txBuilder client.TxBuilder,
	privKey cryptotypes.PrivKey,
	account *authtypes.BaseAccount,
) (uint64, error) {
	// Sign with unordered=true to match actual broadcast (for accurate gas estimation)
	if err := tc.signTx(ctx, txBuilder, privKey, account, true); err != nil {
		return 0, fmt.Errorf("failed to sign transaction for simulation: %w", err)
	}

	// Encode the transaction
	txBytes, err := tc.txConfig.TxEncoder()(txBuilder.GetTx())
	if err != nil {
		return 0, fmt.Errorf("failed to encode transaction for simulation: %w", err)
	}

	// Simulate the transaction
	simRes, err := tc.txClient.Simulate(ctx, &txtypes.SimulateRequest{
		TxBytes: txBytes,
	})
	if err != nil {
		return 0, fmt.Errorf("simulation failed: %w", err)
	}

	if simRes.GasInfo == nil {
		return 0, fmt.Errorf("simulation returned nil gas info")
	}

	return simRes.GasInfo.GasUsed, nil
}

// calculateFee calculates the transaction fee based on configured gas limit.
// This is the MAXIMUM fee we're willing to pay (set before broadcast).
func (tc *TxClient) calculateFee() cosmostypes.Coins {
	return tc.calculateFeeForGas(tc.config.GasLimit)
}

// calculateFeeForGas calculates the transaction fee for a given gas limit.
func (tc *TxClient) calculateFeeForGas(gasLimit uint64) cosmostypes.Coins {
	gasLimitDec := math.LegacyNewDec(int64(gasLimit))
	feeAmount := tc.config.GasPrice.Amount.Mul(gasLimitDec)

	// Truncate and add 1 if there's a remainder to ensure we don't underpay
	feeInt := feeAmount.TruncateInt()
	if feeAmount.Sub(math.LegacyNewDecFromInt(feeInt)).IsPositive() {
		feeInt = feeInt.Add(math.OneInt())
	}

	return cosmostypes.NewCoins(cosmostypes.NewCoin(tc.config.GasPrice.Denom, feeInt))
}

// NOTE: calculateActualFee() removed - not available in SYNC mode (only CheckTx, no execution result)

// isInsufficientBalanceError checks if the error message indicates insufficient balance.
func isInsufficientBalanceError(errorMsg string) bool {
	// Common error patterns from Cosmos SDK
	insufficientFundsPatterns := []string{
		"insufficient funds",
		"insufficient account balance",
		"spendable balance",
	}

	errorLower := strings.ToLower(errorMsg)
	for _, pattern := range insufficientFundsPatterns {
		if strings.Contains(errorLower, pattern) {
			return true
		}
	}
	return false
}

// isSequenceMismatchError checks if the error message indicates account sequence mismatch.
func isSequenceMismatchError(errorMsg string) bool {
	// Common error patterns from Cosmos SDK
	sequenceMismatchPatterns := []string{
		"account sequence mismatch",
		"incorrect account sequence",
		"sequence mismatch",
	}

	errorLower := strings.ToLower(errorMsg)
	for _, pattern := range sequenceMismatchPatterns {
		if strings.Contains(errorLower, pattern) {
			return true
		}
	}
	return false
}

// Close closes the transaction client.
// If the client was created with a shared gRPC connection, it will not be closed.
func (tc *TxClient) Close() error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.closed {
		return nil
	}
	tc.closed = true

	// Only close the connection if we created it ourselves
	if tc.ownsConn && tc.grpcConn != nil {
		if err := tc.grpcConn.Close(); err != nil {
			return fmt.Errorf("failed to close gRPC connection: %w", err)
		}
	}

	tc.logger.Info().Msg("transaction client closed")
	return nil
}

// =============================================================================
// SupplierClient wrapper for compatibility with pkg/client interfaces
// =============================================================================

// HASupplierClient wraps TxClient to implement the client.SupplierClient interface.
type HASupplierClient struct {
	txClient     *TxClient
	operatorAddr string
	logger       logging.Logger

	// lastClaimTxHash stores the TX hash of the last claim submission (for deduplication)
	lastClaimTxHash string
	lastClaimTxMu   sync.RWMutex

	// lastProofTxHash stores the TX hash of the last proof submission (for deduplication)
	lastProofTxHash string
	lastProofTxMu   sync.RWMutex
}

// NewHASupplierClient creates a new supplier client for a specific operator.
func NewHASupplierClient(
	txClient *TxClient,
	operatorAddr string,
	logger logging.Logger,
) *HASupplierClient {
	return &HASupplierClient{
		txClient:     txClient,
		operatorAddr: operatorAddr,
		logger:       logger.With().Str("supplier", operatorAddr).Logger(),
	}
}

// GetEstimatedFeeUpokt returns the estimated transaction fee in upokt.
// This is used for economic validation before submitting claims.
func (c *HASupplierClient) GetEstimatedFeeUpokt() uint64 {
	// Formula: fee = GasLimit × GasPrice
	gasLimitDec := math.LegacyNewDec(int64(c.txClient.config.GasLimit))
	feeAmount := c.txClient.config.GasPrice.Amount.Mul(gasLimitDec)
	feeInt := feeAmount.TruncateInt()
	if feeAmount.Sub(math.LegacyNewDecFromInt(feeInt)).IsPositive() {
		feeInt = feeInt.Add(math.OneInt())
	}
	return feeInt.Uint64()
}

// CreateClaims implements client.SupplierClient.
func (c *HASupplierClient) CreateClaims(
	ctx context.Context,
	timeoutHeight int64,
	claimMsgs ...pocktclient.MsgCreateClaim,
) error {
	claims := make([]*prooftypes.MsgCreateClaim, len(claimMsgs))
	for i, msg := range claimMsgs {
		claim, ok := msg.(*prooftypes.MsgCreateClaim)
		if !ok {
			return fmt.Errorf("invalid claim message type: %T", msg)
		}
		claims[i] = claim
	}

	// Call TxClient and capture TX hash for deduplication
	txHash, err := c.txClient.CreateClaims(ctx, c.operatorAddr, timeoutHeight, claims)
	if err != nil {
		return err
	}

	// Store TX hash for retrieval by caller (1 line after broadcast)
	c.lastClaimTxMu.Lock()
	c.lastClaimTxHash = txHash
	c.lastClaimTxMu.Unlock()

	return nil
}

// SubmitProofs implements client.SupplierClient.
func (c *HASupplierClient) SubmitProofs(
	ctx context.Context,
	timeoutHeight int64,
	proofMsgs ...pocktclient.MsgSubmitProof,
) error {
	proofs := make([]*prooftypes.MsgSubmitProof, len(proofMsgs))
	for i, msg := range proofMsgs {
		proof, ok := msg.(*prooftypes.MsgSubmitProof)
		if !ok {
			return fmt.Errorf("invalid proof message type: %T", msg)
		}
		proofs[i] = proof
	}

	// Call TxClient and capture TX hash for deduplication
	txHash, err := c.txClient.SubmitProofs(ctx, c.operatorAddr, timeoutHeight, proofs)
	if err != nil {
		return err
	}

	// Store TX hash for retrieval by caller (1 line after broadcast)
	c.lastProofTxMu.Lock()
	c.lastProofTxHash = txHash
	c.lastProofTxMu.Unlock()

	return nil
}

// OperatorAddress implements client.SupplierClient.
func (c *HASupplierClient) OperatorAddress() string {
	return c.operatorAddr
}

// GetLastClaimTxHash returns the TX hash of the last claim submission.
// This is used for deduplication tracking in Redis.
func (c *HASupplierClient) GetLastClaimTxHash() string {
	c.lastClaimTxMu.RLock()
	defer c.lastClaimTxMu.RUnlock()
	return c.lastClaimTxHash
}

// GetLastProofTxHash returns the TX hash of the last proof submission.
// This is used for deduplication tracking in Redis.
func (c *HASupplierClient) GetLastProofTxHash() string {
	c.lastProofTxMu.RLock()
	defer c.lastProofTxMu.RUnlock()
	return c.lastProofTxHash
}
