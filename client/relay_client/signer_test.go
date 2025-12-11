package relay_client

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	"github.com/stretchr/testify/require"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

const (
	// Valid test private key (32 bytes hex)
	testPrivKeyHex = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"
	// Expected address derived from testPrivKeyHex
	testExpectedAddress = "pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4"
)

// TestNewSignerFromHex_ValidKey tests signer creation with a valid private key.
func TestNewSignerFromHex_ValidKey(t *testing.T) {
	signer, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err, "NewSignerFromHex should succeed with valid key")
	require.NotNil(t, signer, "Signer should not be nil")
	require.NotNil(t, signer.privKey, "Private key should be set")
	require.NotEmpty(t, signer.address, "Address should be derived")

	// Verify address format (should be bech32 with "pokt" prefix)
	require.Equal(t, testExpectedAddress, signer.address, "Address should match expected value")
}

// TestNewSignerFromHex_EmptyKey tests that empty key is rejected.
func TestNewSignerFromHex_EmptyKey(t *testing.T) {
	signer, err := NewSignerFromHex("")
	require.Error(t, err, "NewSignerFromHex should fail with empty key")
	require.Nil(t, signer, "Signer should be nil on error")
	require.Contains(t, err.Error(), "private key hex is empty")
}

// TestNewSignerFromHex_InvalidHex tests that invalid hex is rejected.
func TestNewSignerFromHex_InvalidHex(t *testing.T) {
	signer, err := NewSignerFromHex("not-valid-hex")
	require.Error(t, err, "NewSignerFromHex should fail with invalid hex")
	require.Nil(t, signer, "Signer should be nil on error")
	require.Contains(t, err.Error(), "invalid private key hex")
}

// TestNewSignerFromHex_WrongLength tests that wrong key length is rejected.
func TestNewSignerFromHex_WrongLength(t *testing.T) {
	// 16 bytes instead of 32
	shortKey := "2d00ef074d9b51e46886dc9a1df11e7b"
	signer, err := NewSignerFromHex(shortKey)
	require.Error(t, err, "NewSignerFromHex should fail with wrong length key")
	require.Nil(t, signer, "Signer should be nil on error")
	require.Contains(t, err.Error(), "invalid private key length")
}

// TestGetAddress tests address retrieval.
func TestGetAddress(t *testing.T) {
	signer, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err)

	address := signer.GetAddress()
	require.NotEmpty(t, address, "GetAddress should return non-empty address")
	require.Equal(t, testExpectedAddress, address, "Address should match expected value")
}

// TestGetPrivKey tests private key retrieval.
func TestGetPrivKey(t *testing.T) {
	signer, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err)

	privKey := signer.GetPrivKey()
	require.NotNil(t, privKey, "GetPrivKey should return non-nil key")
	require.Len(t, privKey.Bytes(), 32, "Private key should be 32 bytes")

	// Verify same key produces same address (consistency check)
	pubKey := privKey.PubKey()
	address, err := deriveAddressFromPubKey(pubKey)
	require.NoError(t, err)
	require.Equal(t, testExpectedAddress, address, "Key should derive to expected address")
}

// TestDeriveAddressFromPubKey tests Bech32 address derivation.
func TestDeriveAddressFromPubKey(t *testing.T) {
	// Create a test private key (use direct PrivKey construction, NOT GenPrivKeyFromSecret)
	keyBytes, err := hex.DecodeString(testPrivKeyHex)
	require.NoError(t, err)

	// Use direct construction like NewSignerFromHex does
	privKey := &secp256k1.PrivKey{Key: keyBytes}
	pubKey := privKey.PubKey()

	address, err := deriveAddressFromPubKey(pubKey)
	require.NoError(t, err, "deriveAddressFromPubKey should succeed")
	require.NotEmpty(t, address, "Address should not be empty")
	require.Equal(t, "pokt", address[:4], "Address should have 'pokt' prefix")
	require.Equal(t, testExpectedAddress, address, "Address should match expected value")
}

// TestSignRelayRequest_NilRequest tests that nil request is rejected.
func TestSignRelayRequest_NilRequest(t *testing.T) {
	signer, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err)

	err = signer.SignRelayRequest(context.Background(), nil, &apptypes.Application{}, nil)
	require.Error(t, err, "SignRelayRequest should fail with nil request")
	require.Contains(t, err.Error(), "relay request is nil")
}

// TestSignRelayRequest_MissingSessionHeader tests that missing session header is rejected.
func TestSignRelayRequest_MissingSessionHeader(t *testing.T) {
	signer, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err)

	relayRequest := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: nil, // Missing
		},
	}

	err = signer.SignRelayRequest(context.Background(), relayRequest, &apptypes.Application{}, nil)
	require.Error(t, err, "SignRelayRequest should fail with missing session header")
	require.Contains(t, err.Error(), "missing session header")
}

// TestScalarConversion tests private key to scalar conversion.
func TestScalarConversion(t *testing.T) {
	// Create test private key
	keyBytes, err := hex.DecodeString(testPrivKeyHex)
	require.NoError(t, err)

	privKey := secp256k1.GenPrivKeyFromSecret(keyBytes)

	// Convert to scalar (same logic as in SignRelayRequest)
	curve := ring_secp256k1.NewCurve()
	scalar, err := curve.DecodeToScalar(privKey.Bytes())
	require.NoError(t, err, "DecodeToScalar should succeed")
	require.NotNil(t, scalar, "Scalar should not be nil")
}

// TestKeyConsistency tests that same hex always produces same key and address.
func TestKeyConsistency(t *testing.T) {
	// Create signer twice with same key
	signer1, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err)

	signer2, err := NewSignerFromHex(testPrivKeyHex)
	require.NoError(t, err)

	// Addresses should match
	require.Equal(t, signer1.GetAddress(), signer2.GetAddress(), "Same key should produce same address")

	// Private key bytes should match
	require.Equal(t, signer1.GetPrivKey().Bytes(), signer2.GetPrivKey().Bytes(), "Same key should produce same private key")
}
