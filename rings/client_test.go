//go:build test

package rings

import (
	"context"
	"errors"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	ringtypes "github.com/pokt-network/go-dleq/types"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// mockApplicationQuerier is a mock implementation of ApplicationQueryClient for testing.
type mockApplicationQuerier struct {
	app *apptypes.Application
	err error
}

func (m *mockApplicationQuerier) GetApplication(ctx context.Context, address string) (apptypes.Application, error) {
	if m.err != nil {
		return apptypes.Application{}, m.err
	}
	if m.app != nil {
		return *m.app, nil
	}
	return apptypes.Application{}, errors.New("application not found")
}

func (m *mockApplicationQuerier) GetAllApplications(ctx context.Context) ([]apptypes.Application, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.app != nil {
		return []apptypes.Application{*m.app}, nil
	}
	return nil, nil
}

func (m *mockApplicationQuerier) GetParams(ctx context.Context) (*apptypes.Params, error) {
	return &apptypes.Params{}, nil
}

// mockAccountQuerier is a mock implementation of AccountQueryClient for testing.
type mockAccountQuerier struct {
	pubKeys map[string]cryptotypes.PubKey
	err     error
}

func (m *mockAccountQuerier) GetAccount(ctx context.Context, address string) (cosmostypes.AccountI, error) {
	return nil, errors.New("not implemented in mock")
}

func (m *mockAccountQuerier) GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	if m.err != nil {
		return nil, m.err
	}
	if pubKey, ok := m.pubKeys[address]; ok {
		return pubKey, nil
	}
	return nil, errors.New("pubkey not found")
}

// mockSharedQuerier is a mock implementation of SharedQueryClient for testing.
type mockSharedQuerier struct {
	params *sharedtypes.Params
	err    error
}

func (m *mockSharedQuerier) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.params != nil {
		return m.params, nil
	}
	// Return default params
	return &sharedtypes.Params{
		NumBlocksPerSession:                4,
		GracePeriodEndOffsetBlocks:         1,
		ClaimWindowOpenOffsetBlocks:        1,
		ClaimWindowCloseOffsetBlocks:       4,
		ProofWindowOpenOffsetBlocks:        0,
		ProofWindowCloseOffsetBlocks:       4,
		SupplierUnbondingPeriodSessions:    4,
		ApplicationUnbondingPeriodSessions: 4,
		ComputeUnitsToTokensMultiplier:     42,
	}, nil
}

func (m *mockSharedQuerier) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return 0, errors.New("not implemented in mock")
}

func (m *mockSharedQuerier) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return 0, errors.New("not implemented in mock")
}

func (m *mockSharedQuerier) GetEarliestSupplierClaimCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return 0, errors.New("not implemented in mock")
}

func (m *mockSharedQuerier) GetProofWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return 0, errors.New("not implemented in mock")
}

func (m *mockSharedQuerier) GetEarliestSupplierProofCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return 0, errors.New("not implemented in mock")
}

// createTestAddress creates a test bech32 address from a public key.
func createTestAddress(pubKey cryptotypes.PubKey) string {
	// Convert to Cosmos SDK address format (bech32 with "pokt" prefix)
	addr := cosmostypes.AccAddress(pubKey.Address())
	return addr.String()
}

// newNopLogger creates a no-op logger for testing.
func newNopLogger() zerolog.Logger {
	return zerolog.Nop()
}

// TestNewRingClient tests ring client creation.
func TestNewRingClient(t *testing.T) {
	logger := newNopLogger()
	appQuerier := &mockApplicationQuerier{}
	accQuerier := &mockAccountQuerier{}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	require.NotNil(t, client)
}

// TestRingClient_GetRingForAddressAtHeight tests ring retrieval for an address.
func TestRingClient_GetRingForAddressAtHeight(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	// Generate test keys
	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	gateway1PrivKey := secp256k1.GenPrivKey()
	gateway1PubKey := gateway1PrivKey.PubKey()
	gateway1Address := createTestAddress(gateway1PubKey)

	// Setup mock application with one gateway delegation
	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{gateway1Address},
	}

	// Setup mock account querier with public keys
	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:      appPubKey,
		gateway1Address: gateway1PubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	// Test getting ring at a specific height
	ring, err := client.GetRingForAddressAtHeight(ctx, appAddress, 100)

	require.NoError(t, err)
	require.NotNil(t, ring)
	// Ring should contain app + gateway = 2 keys
	require.Equal(t, 2, ring.Size())
}

// TestRingClient_GetRingForAddressAtHeight_NoDelegations tests ring with no delegations.
func TestRingClient_GetRingForAddressAtHeight_NoDelegations(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	// Generate test keys
	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	// Setup mock application with NO gateway delegations
	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{}, // Empty
	}

	// Setup mock account querier with public keys (including placeholder)
	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:             appPubKey,
		PlaceholderRingAddress: PlaceholderRingPubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	// Test getting ring - should use placeholder
	ring, err := client.GetRingForAddressAtHeight(ctx, appAddress, 100)

	require.NoError(t, err)
	require.NotNil(t, ring)
	// Ring should contain app + placeholder = 2 keys
	require.Equal(t, 2, ring.Size())
}

// TestRingClient_GetRingForAddressAtHeight_ApplicationNotFound tests error handling.
func TestRingClient_GetRingForAddressAtHeight_ApplicationNotFound(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	appQuerier := &mockApplicationQuerier{err: errors.New("application not found")}
	accQuerier := &mockAccountQuerier{}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	ring, err := client.GetRingForAddressAtHeight(ctx, "invalid_address", 100)

	require.Error(t, err)
	require.Nil(t, ring)
}

// TestRingClient_GetRingForAddressAtHeight_MultipleDelegations tests ring with multiple delegations.
func TestRingClient_GetRingForAddressAtHeight_MultipleDelegations(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	// Generate test keys
	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	// Generate 3 gateway keys
	var gatewayAddresses []string
	pubKeys := map[string]cryptotypes.PubKey{
		appAddress: appPubKey,
	}

	for i := 0; i < 3; i++ {
		gwPrivKey := secp256k1.GenPrivKey()
		gwPubKey := gwPrivKey.PubKey()
		gwAddress := createTestAddress(gwPubKey)
		gatewayAddresses = append(gatewayAddresses, gwAddress)
		pubKeys[gwAddress] = gwPubKey
	}

	// Setup mock application with multiple gateway delegations
	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: gatewayAddresses,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	// Test getting ring
	ring, err := client.GetRingForAddressAtHeight(ctx, appAddress, 100)

	require.NoError(t, err)
	require.NotNil(t, ring)
	// Ring should contain app + 3 gateways = 4 keys
	require.Equal(t, 4, ring.Size())
}

// TestRingPointsContain tests the ringPointsContain helper function.
func TestRingPointsContain(t *testing.T) {
	// Generate test keys for the ring (3 public keys)
	privKeys := make([]*secp256k1.PrivKey, 3)
	pubKeys := make([]cryptotypes.PubKey, 3)
	for i := 0; i < 3; i++ {
		privKeys[i] = secp256k1.GenPrivKey()
		pubKeys[i] = privKeys[i].PubKey()
	}

	// Create ring from public keys
	testRing, err := GetRingFromPubKeys(pubKeys)
	require.NoError(t, err)

	// Convert keys to points and create map (expected ring points)
	points, err := pointsFromPublicKeys(pubKeys...)
	require.NoError(t, err)

	ringPointsMap := make(map[string]ringtypes.Point)
	for _, point := range points {
		ringPointsMap[string(point.Encode())] = point
	}

	// Sign with one of the actual private keys in the ring (index 0)
	curve := ring_secp256k1.NewCurve()
	signableBz := [32]byte{}
	copy(signableBz[:], []byte("test message"))

	scalar, err := curve.DecodeToScalar(privKeys[0].Key)
	require.NoError(t, err)

	ringSig, err := testRing.Sign(signableBz, scalar)
	require.NoError(t, err)

	// Test that ringPointsContain returns true for matching points
	contains := ringPointsContain(ringPointsMap, ringSig)
	require.True(t, contains)
}

// TestRingPointsContain_Mismatch tests ringPointsContain with non-matching points.
func TestRingPointsContain_Mismatch(t *testing.T) {
	// Generate two different sets of keys with private keys
	privKeys1 := make([]*secp256k1.PrivKey, 3)
	pubKeys1 := make([]cryptotypes.PubKey, 3)
	for i := 0; i < 3; i++ {
		privKeys1[i] = secp256k1.GenPrivKey()
		pubKeys1[i] = privKeys1[i].PubKey()
	}

	privKeys2 := make([]*secp256k1.PrivKey, 3)
	pubKeys2 := make([]cryptotypes.PubKey, 3)
	for i := 0; i < 3; i++ {
		privKeys2[i] = secp256k1.GenPrivKey()
		pubKeys2[i] = privKeys2[i].PubKey()
	}

	// Create ring from keys2
	ring2, err := GetRingFromPubKeys(pubKeys2)
	require.NoError(t, err)

	// Create points map from keys1 (different keys)
	points1, err := pointsFromPublicKeys(pubKeys1...)
	require.NoError(t, err)

	ringPointsMap := make(map[string]ringtypes.Point)
	for _, point := range points1 {
		ringPointsMap[string(point.Encode())] = point
	}

	// Sign with ring2 using one of its private keys
	curve := ring_secp256k1.NewCurve()
	signableBz := [32]byte{}
	copy(signableBz[:], []byte("test message"))

	scalar, err := curve.DecodeToScalar(privKeys2[0].Key)
	require.NoError(t, err)

	ringSig, err := ring2.Sign(signableBz, scalar)
	require.NoError(t, err)

	// Test that ringPointsContain returns false for non-matching points
	contains := ringPointsContain(ringPointsMap, ringSig)
	require.False(t, contains)
}

// TestGetRingAddressesAtSessionEndHeight tests ring address calculation at session end height.
func TestGetRingAddressesAtSessionEndHeight(t *testing.T) {
	gateway1 := "gateway1"
	gateway2 := "gateway2"

	tests := []struct {
		name                       string
		app                        *apptypes.Application
		targetSessionEndHeight     uint64
		expectedDelegateeAddresses []string
	}{
		{
			name: "no pending undelegations",
			app: &apptypes.Application{
				DelegateeGatewayAddresses: []string{gateway1, gateway2},
				PendingUndelegations:      map[uint64]apptypes.UndelegatingGatewayList{},
			},
			targetSessionEndHeight:     100,
			expectedDelegateeAddresses: []string{gateway1, gateway2},
		},
		{
			name: "undelegation before target height (should be excluded)",
			app: &apptypes.Application{
				DelegateeGatewayAddresses: []string{gateway1},
				PendingUndelegations: map[uint64]apptypes.UndelegatingGatewayList{
					50: {GatewayAddresses: []string{gateway2}}, // Undelegated before target
				},
			},
			targetSessionEndHeight:     100,
			expectedDelegateeAddresses: []string{gateway1}, // gateway2 not included
		},
		{
			name: "undelegation after target height (should be included)",
			app: &apptypes.Application{
				DelegateeGatewayAddresses: []string{gateway1},
				PendingUndelegations: map[uint64]apptypes.UndelegatingGatewayList{
					200: {GatewayAddresses: []string{gateway2}}, // Undelegated after target
				},
			},
			targetSessionEndHeight:     100,
			expectedDelegateeAddresses: []string{gateway1, gateway2}, // gateway2 included
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addresses := GetRingAddressesAtSessionEndHeight(tt.app, tt.targetSessionEndHeight)

			require.ElementsMatch(t, tt.expectedDelegateeAddresses, addresses)
		})
	}
}

// TestGetRingAddressesAtBlock tests ring address calculation with shared params.
func TestGetRingAddressesAtBlock(t *testing.T) {
	sharedParams := &sharedtypes.Params{
		NumBlocksPerSession: 4,
	}

	app := &apptypes.Application{
		DelegateeGatewayAddresses: []string{"gateway1"},
		PendingUndelegations:      map[uint64]apptypes.UndelegatingGatewayList{},
	}

	// Test at block 100
	addresses := GetRingAddressesAtBlock(sharedParams, app, 100)

	require.NotEmpty(t, addresses)
	require.Contains(t, addresses, "gateway1")
}

// TestPlaceholderRingPubKey tests that the placeholder key is valid.
func TestPlaceholderRingPubKey(t *testing.T) {
	require.NotNil(t, PlaceholderRingPubKey)
	require.NotEmpty(t, PlaceholderRingAddress)

	// Verify placeholder can be used in a ring
	keys := []cryptotypes.PubKey{PlaceholderRingPubKey, generateTestKeys(t, 1)[0]}
	ring, err := GetRingFromPubKeys(keys)

	require.NoError(t, err)
	require.NotNil(t, ring)
	require.Equal(t, 2, ring.Size())
}

// TestRingClient_GetRingForAddressAtHeight_ErrorRetrievingPubKey tests error handling.
func TestRingClient_GetRingForAddressAtHeight_ErrorRetrievingPubKey(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	// Generate test keys
	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	// Setup mock application
	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{"invalid_gateway"},
	}

	// Setup mock account querier that will return error for invalid_gateway
	pubKeys := map[string]cryptotypes.PubKey{
		appAddress: appPubKey,
		// invalid_gateway is missing, will cause error
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys} // Will error on invalid_gateway
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	// This should fail when trying to get pubkey for invalid_gateway
	ring, err := client.GetRingForAddressAtHeight(ctx, appAddress, 100)

	require.Error(t, err)
	require.Nil(t, ring)
	require.Contains(t, err.Error(), "pubkey not found")
}

// TestGetRingAddressesAtSessionEndHeight_WithDuplicates tests deduplication within pending undelegations.
func TestGetRingAddressesAtSessionEndHeight_WithDuplicates(t *testing.T) {
	gateway1 := "gateway1"
	gateway2 := "gateway2"
	gateway3 := "gateway3"

	// App with no active delegations, but multiple pending undelegations
	// at different heights that may reference the same gateway
	app := &apptypes.Application{
		DelegateeGatewayAddresses: []string{},
		PendingUndelegations: map[uint64]apptypes.UndelegatingGatewayList{
			200: {GatewayAddresses: []string{gateway1, gateway2}},
			300: {GatewayAddresses: []string{gateway2, gateway3}}, // gateway2 appears again
		},
	}

	addresses := GetRingAddressesAtSessionEndHeight(app, 100)

	// The deduplication logic should prevent gateway2 from being added twice
	// when processing different pending undelegation heights
	uniqueAddresses := make(map[string]bool)
	for _, addr := range addresses {
		require.False(t, uniqueAddresses[addr], "duplicate address found: %s", addr)
		uniqueAddresses[addr] = true
	}

	// Should contain all three gateways
	require.Contains(t, addresses, gateway1)
	require.Contains(t, addresses, gateway2)
	require.Contains(t, addresses, gateway3)
	require.Equal(t, 3, len(addresses))
}

// TestRingClient_GetRingAddressesAtBlock_QueryError tests error handling.
func TestRingClient_GetRingAddressesAtBlock_QueryError(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	app := &apptypes.Application{
		Address:                   "app1",
		DelegateeGatewayAddresses: []string{"gateway1"},
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{}
	sharedQuerier := &mockSharedQuerier{err: errors.New("query failed")}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)
	rc := client.(*ringClient)

	// This should fail when trying to get params
	addresses, err := rc.GetRingAddressesAtBlock(ctx, app, 100)

	require.Error(t, err)
	require.Nil(t, addresses)
	require.Contains(t, err.Error(), "query failed")
}

// TestRingClient_VerifyRelayRequestSignature_ValidSignature tests valid signature verification.
func TestRingClient_VerifyRelayRequestSignature_ValidSignature(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	// Generate test keys (3 for ring)
	privKeys := make([]*secp256k1.PrivKey, 3)
	pubKeys := make([]cryptotypes.PubKey, 3)
	addresses := make([]string, 3)

	for i := 0; i < 3; i++ {
		privKeys[i] = secp256k1.GenPrivKey()
		pubKeys[i] = privKeys[i].PubKey()
		addresses[i] = createTestAddress(pubKeys[i])
	}

	// Setup: addresses[0] is app, addresses[1] and addresses[2] are gateways
	appAddress := addresses[0]
	gatewayAddresses := []string{addresses[1], addresses[2]}

	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: gatewayAddresses,
	}

	pubKeyMap := make(map[string]cryptotypes.PubKey)
	for i, addr := range addresses {
		pubKeyMap[addr] = pubKeys[i]
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeyMap}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	// Create a valid ring from the keys
	ring, err := GetRingFromPubKeys(pubKeys)
	require.NoError(t, err)

	// Create a relay request
	sessionHeader := &sessiontypes.SessionHeader{
		ApplicationAddress:      appAddress,
		ServiceId:               "test_service",
		SessionId:               "session_123",
		SessionStartBlockHeight: 1,
		SessionEndBlockHeight:   100,
	}

	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: sessionHeader,
		},
		Payload: []byte(`{"jsonrpc":"2.0","method":"test","params":[],"id":1}`),
	}

	// Get signable bytes from relay request
	signableBz, err := relayRequest.GetSignableBytesHash()
	require.NoError(t, err)

	// Sign with the app's private key (index 0)
	curve := ring_secp256k1.NewCurve()
	scalar, err := curve.DecodeToScalar(privKeys[0].Key)
	require.NoError(t, err)

	ringSig, err := ring.Sign(signableBz, scalar)
	require.NoError(t, err)

	// Serialize and add signature to relay request
	serializedSig, err := ringSig.Serialize()
	require.NoError(t, err)
	relayRequest.Meta.Signature = serializedSig

	// Verify the signature
	err = client.VerifyRelayRequestSignature(ctx, relayRequest)
	require.NoError(t, err)
}

// TestRingClient_VerifyRelayRequestSignature_MissingSignature tests missing signature error.
func TestRingClient_VerifyRelayRequestSignature_MissingSignature(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{},
	}

	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:             appPubKey,
		PlaceholderRingAddress: PlaceholderRingPubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	// Create relay request without signature
	sessionHeader := &sessiontypes.SessionHeader{
		ApplicationAddress:      appAddress,
		ServiceId:               "test_service",
		SessionId:               "session_123",
		SessionStartBlockHeight: 1,
		SessionEndBlockHeight:   100,
	}

	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: sessionHeader,
			Signature:     nil, // Missing signature
		},
		Payload: []byte(`{"test":"data"}`),
	}

	err := client.VerifyRelayRequestSignature(ctx, relayRequest)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequest)
	require.Contains(t, err.Error(), "missing signature")
}

// TestRingClient_VerifyRelayRequestSignature_InvalidSignature tests invalid signature error.
func TestRingClient_VerifyRelayRequestSignature_InvalidSignature(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{},
	}

	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:             appPubKey,
		PlaceholderRingAddress: PlaceholderRingPubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	sessionHeader := &sessiontypes.SessionHeader{
		ApplicationAddress:      appAddress,
		ServiceId:               "test_service",
		SessionId:               "session_123",
		SessionStartBlockHeight: 1,
		SessionEndBlockHeight:   100,
	}

	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: sessionHeader,
			Signature:     []byte("invalid_signature_bytes"),
		},
		Payload: []byte(`{"test":"data"}`),
	}

	err := client.VerifyRelayRequestSignature(ctx, relayRequest)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequestSignature)
}

// TestRingClient_VerifyRelayRequestSignature_InvalidSessionHeader tests invalid session header.
func TestRingClient_VerifyRelayRequestSignature_InvalidSessionHeader(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	appQuerier := &mockApplicationQuerier{}
	accQuerier := &mockAccountQuerier{}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	// Create relay request with invalid session header (missing required fields)
	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				// Missing required fields
			},
			Signature: []byte("some_signature"),
		},
		Payload: []byte(`{"test":"data"}`),
	}

	err := client.VerifyRelayRequestSignature(ctx, relayRequest)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrRingClientInvalidRelayRequest)
	require.Contains(t, err.Error(), "invalid session header")
}

// TestRingClient_GetRingPointsForAddressAtHeight tests the getRingPointsForAddressAtHeight helper.
func TestRingClient_GetRingPointsForAddressAtHeight(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	// Generate test keys
	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	gateway1PrivKey := secp256k1.GenPrivKey()
	gateway1PubKey := gateway1PrivKey.PubKey()
	gateway1Address := createTestAddress(gateway1PubKey)

	// Setup mock application
	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{gateway1Address},
	}

	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:      appPubKey,
		gateway1Address: gateway1PubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)
	rc := client.(*ringClient)

	// Test getRingPointsForAddressAtHeight
	ringPoints, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)

	require.NoError(t, err)
	require.NotNil(t, ringPoints)
	require.Equal(t, 2, len(ringPoints), "should have 2 points (app + 1 gateway)")

	// Verify points can be encoded
	for key, point := range ringPoints {
		require.NotEmpty(t, key)
		require.NotNil(t, point)
		encoded := point.Encode()
		require.NotEmpty(t, encoded)
	}
}

// TestRingClient_GetRingPointsForAddressAtHeight_ApplicationError tests error handling.
func TestRingClient_GetRingPointsForAddressAtHeight_ApplicationError(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	appQuerier := &mockApplicationQuerier{err: errors.New("application query failed")}
	accQuerier := &mockAccountQuerier{}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)
	rc := client.(*ringClient)

	ringPoints, err := rc.getRingPointsForAddressAtHeight(ctx, "invalid_address", 100)

	require.Error(t, err)
	require.Nil(t, ringPoints)
	require.Contains(t, err.Error(), "application query failed")
}

// TestRingClient_GetRingPointsForAddressAtHeight_NoDelegations tests with placeholder.
func TestRingClient_GetRingPointsForAddressAtHeight_NoDelegations(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	// App with no delegations
	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{},
	}

	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:             appPubKey,
		PlaceholderRingAddress: PlaceholderRingPubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)
	rc := client.(*ringClient)

	ringPoints, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)

	require.NoError(t, err)
	require.NotNil(t, ringPoints)
	require.Equal(t, 2, len(ringPoints), "should have 2 points (app + placeholder)")
}

// TestRingClient_GetRingPointsForAddressAtHeight_MultipleDelegations tests with many delegations.
func TestRingClient_GetRingPointsForAddressAtHeight_MultipleDelegations(t *testing.T) {
	ctx := context.Background()
	logger := newNopLogger()

	// Generate app key
	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	// Generate 5 gateway keys
	numGateways := 5
	gatewayAddresses := make([]string, numGateways)
	pubKeys := map[string]cryptotypes.PubKey{
		appAddress: appPubKey,
	}

	for i := 0; i < numGateways; i++ {
		gwPrivKey := secp256k1.GenPrivKey()
		gwPubKey := gwPrivKey.PubKey()
		gwAddress := createTestAddress(gwPubKey)
		gatewayAddresses[i] = gwAddress
		pubKeys[gwAddress] = gwPubKey
	}

	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: gatewayAddresses,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)
	rc := client.(*ringClient)

	ringPoints, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)

	require.NoError(t, err)
	require.NotNil(t, ringPoints)
	require.Equal(t, numGateways+1, len(ringPoints), "should have app + 5 gateways = 6 points")
}

// BenchmarkGetRingForAddressAtHeight benchmarks ring retrieval.
func BenchmarkGetRingForAddressAtHeight(b *testing.B) {
	ctx := context.Background()
	logger := newNopLogger()

	// Setup test data
	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	gateway1PrivKey := secp256k1.GenPrivKey()
	gateway1PubKey := gateway1PrivKey.PubKey()
	gateway1Address := createTestAddress(gateway1PubKey)

	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{gateway1Address},
	}

	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:      appPubKey,
		gateway1Address: gateway1PubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := client.GetRingForAddressAtHeight(ctx, appAddress, 100)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

// BenchmarkGetRingPointsForAddressAtHeight benchmarks ring points retrieval.
func BenchmarkGetRingPointsForAddressAtHeight(b *testing.B) {
	ctx := context.Background()
	logger := newNopLogger()

	appPrivKey := secp256k1.GenPrivKey()
	appPubKey := appPrivKey.PubKey()
	appAddress := createTestAddress(appPubKey)

	gateway1PrivKey := secp256k1.GenPrivKey()
	gateway1PubKey := gateway1PrivKey.PubKey()
	gateway1Address := createTestAddress(gateway1PubKey)

	app := &apptypes.Application{
		Address:                   appAddress,
		DelegateeGatewayAddresses: []string{gateway1Address},
	}

	pubKeys := map[string]cryptotypes.PubKey{
		appAddress:      appPubKey,
		gateway1Address: gateway1PubKey,
	}

	appQuerier := &mockApplicationQuerier{app: app}
	accQuerier := &mockAccountQuerier{pubKeys: pubKeys}
	sharedQuerier := &mockSharedQuerier{}

	client := NewRingClient(logger, appQuerier, accQuerier, sharedQuerier)
	rc := client.(*ringClient)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := rc.getRingPointsForAddressAtHeight(ctx, appAddress, 100)
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}
