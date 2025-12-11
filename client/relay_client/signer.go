package relay_client

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"

	"github.com/pokt-network/poktroll/pkg/crypto"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// Signer handles signing of relay requests using ring signatures.
// It manages an secp256k1 private key and derives the corresponding Pocket Network address.
// The signer is used to create cryptographic signatures for relay requests that prove
// the request is authorized by the application.
type Signer struct {
	privKey cryptotypes.PrivKey
	address string
}

// NewSignerFromHex creates a new signer from a hex-encoded secp256k1 private key.
//
// The private key must be exactly 32 bytes (64 hex characters) and will be used to:
//   - Generate an secp256k1 public key
//   - Derive a Bech32-encoded Pocket Network address (pokt prefix)
//   - Sign relay requests with ring signatures
//
// Parameters:
//   - privKeyHex: Hex-encoded secp256k1 private key (32 bytes = 64 hex chars)
//
// Returns:
//   - *Signer: Initialized signer with private key and derived address
//   - error: If the key is empty, invalid hex, or wrong length
//
// Example:
//
//	signer, err := NewSignerFromHex("2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a")
//	if err != nil {
//	    return fmt.Errorf("failed to create signer: %w", err)
//	}
//	address := signer.GetAddress() // pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4
func NewSignerFromHex(privKeyHex string) (*Signer, error) {
	if privKeyHex == "" {
		return nil, fmt.Errorf("private key hex is empty")
	}

	// Decode hex to get private key bytes
	privKeyBytes, err := hex.DecodeString(privKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid private key hex: %w", err)
	}

	// Expected key length is 32 bytes for secp256k1
	if len(privKeyBytes) != 32 {
		return nil, fmt.Errorf("invalid private key length: expected 32 bytes, got %d", len(privKeyBytes))
	}

	// Create secp256k1 private key from raw bytes
	// NOTE: Do NOT use GenPrivKeyFromSecret - it hashes the input, but our keys are already final
	privKey := &secp256k1.PrivKey{Key: privKeyBytes}

	// Derive Bech32 address from public key
	address, err := deriveAddressFromPubKey(privKey.PubKey())
	if err != nil {
		return nil, fmt.Errorf("failed to derive address: %w", err)
	}

	return &Signer{
		privKey: privKey,
		address: address,
	}, nil
}

// GetAddress returns the Bech32-encoded Pocket Network address derived from the private key.
//
// The address is computed once during NewSignerFromHex and cached. It uses the
// "pokt" prefix and is derived from the secp256k1 public key.
//
// Returns:
//   - string: Bech32 address (e.g., "pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4")
func (s *Signer) GetAddress() string {
	return s.address
}

// SignRelayRequest signs a relay request with the application's ring signature.
//
// This implements the Pocket Network relay authentication protocol using ring signatures,
// which allows an application to sign relay requests while maintaining privacy about
// which specific key (application or delegated gateway) is signing.
//
// Signing flow:
//  1. Get the application ring from the RingClient (includes app + delegate gateways)
//  2. Get the signable bytes hash from the relay request
//  3. Convert the private key to a scalar for ring signature operations
//  4. Sign the hash using the ring signature with the private key
//  5. Serialize and set the signature in the request metadata
//
// Parameters:
//   - ctx: Context for cancellation and deadlines
//   - relayRequest: The relay request to sign (must have valid session header)
//   - app: The application from the blockchain (used for ring construction)
//   - ringClient: Client to fetch the application ring at session end height
//
// Returns:
//   - error: If relay request is nil, missing session header, ring fetch fails, or signing fails
//
// The signature is set directly in relayRequest.Meta.Signature.
func (s *Signer) SignRelayRequest(
	ctx context.Context,
	relayRequest *servicetypes.RelayRequest,
	app *apptypes.Application,
	ringClient crypto.RingClient,
) error {
	if relayRequest == nil {
		return fmt.Errorf("relay request is nil")
	}
	if relayRequest.Meta.SessionHeader == nil {
		return fmt.Errorf("relay request missing session header")
	}

	// Get session end height for ring construction
	sessionEndHeight := relayRequest.Meta.SessionHeader.SessionEndBlockHeight

	// Get the application ring (includes app + delegate gateways)
	appRing, err := ringClient.GetRingForAddressAtHeight(ctx, s.address, sessionEndHeight)
	if err != nil {
		return fmt.Errorf("failed to get application ring: %w", err)
	}

	// Get signable bytes hash from relay request
	relayReqSignableBz, err := relayRequest.GetSignableBytesHash()
	if err != nil {
		return fmt.Errorf("failed to get signable bytes hash: %w", err)
	}

	// Convert private key to scalar for ring signature
	curve := ring_secp256k1.NewCurve()
	signingKey, err := curve.DecodeToScalar(s.privKey.Bytes())
	if err != nil {
		return fmt.Errorf("failed to convert private key to scalar: %w", err)
	}

	// Sign the relay request with the application's private key using ring signature
	signature, err := appRing.Sign(relayReqSignableBz, signingKey)
	if err != nil {
		return fmt.Errorf("failed to sign relay request: %w", err)
	}

	// Serialize the ring signature
	signatureBz, err := signature.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize signature: %w", err)
	}

	// Set the signature in the relay request metadata
	relayRequest.Meta.Signature = signatureBz

	return nil
}

// deriveAddressFromPubKey derives a Bech32-encoded Pocket address from a public key.
func deriveAddressFromPubKey(pubKey cryptotypes.PubKey) (string, error) {
	// Get the address bytes from the public key
	addrBytes := pubKey.Address().Bytes()

	// Encode as Bech32 with "pokt" prefix
	address, err := bech32.ConvertAndEncode("pokt", addrBytes)
	if err != nil {
		return "", fmt.Errorf("failed to encode address as Bech32: %w", err)
	}

	return address, nil
}

// GetPrivKey returns the underlying private key.
//
// WARNING: This exposes the raw private key and should be used with extreme caution.
// Prefer using SignRelayRequest for signing operations to avoid exposing the key.
//
// Returns:
//   - cryptotypes.PrivKey: The secp256k1 private key (32 bytes)
func (s *Signer) GetPrivKey() cryptotypes.PrivKey {
	return s.privKey
}
