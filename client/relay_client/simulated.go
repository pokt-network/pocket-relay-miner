package relay_client

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"

	"github.com/pokt-network/pocket-relay-miner/relayer"
	"github.com/pokt-network/pocket-relay-miner/rings"
)

// simNonceBytes is the number of crypto/rand bytes used for the synthetic
// session id's nonce component (hex-encoded: 8 bytes -> 16 hex characters).
const simNonceBytes = 8

// simSessionStartHeight / simSessionEndHeight are the placeholder session
// bounds a simulated relay carries. The relayer's SimulationVerifier never
// checks these against a real on-chain session — it only requires
// SessionHeader.ValidateBasic to pass (start >= 1, end > start) — but they
// must still satisfy that check, since both the relayer and RelayRequest's
// own ValidateBasic call it.
const (
	simSessionStartHeight = 1
	simSessionEndHeight   = 2
)

// BuildSimulatedRelayRequest builds a real, ring-signed RelayRequest for the
// relayer's simulated-relay path: a synthetic session carrying a fresh
// timestamp+nonce (relayer.FormatSimSessionID), ring-signed over
// [appPubKey, gatewayPubKeys...] — the SAME pubkeys an operator pins in the
// relayer's simulation config (relayer.SimIdentity.AppPubKeyHex /
// GatewayPubKeysHex) — and served but never charged.
//
// This performs NO chain queries: the ring is built locally from the given
// pubkeys via rings.GetRingFromPubKeys, exactly mirroring how the relayer's
// SimulationVerifier independently reconstructs the same ring from its
// config-pinned pubkeys (relayer/simulation.go buildSimIdentities) instead of
// an on-chain application/delegation lookup.
//
// Signing: this method signs with THIS client's signer (c.signer), which
// NewRelayClient sets to the gateway signer when GatewayPrivateKeyHex is
// provided (gateway mode) and to the app signer otherwise. The relayer's
// pinned ring expects a gateway-key signature, so callers building a
// simulated relay MUST construct the RelayClient in gateway mode
// (GatewayPrivateKeyHex set) — app-only mode would sign with the app key
// instead, which still produces a cryptographically valid ring signature but
// does not match the "gateway signs on behalf of the app" contract the
// simulated-relay feature documents.
//
// now is injected (rather than reading time.Now() internally) so callers can
// pin freshness deterministically in tests; the CLI passes time.Now() at call
// time. Each call draws a fresh random nonce, so repeated calls with the same
// now still produce distinct session ids/signatures — the relayer dedups
// signatures for the freshness window, so a fixed nonce would make the
// second identical call look like a replay of the first.
func (c *RelayClient) BuildSimulatedRelayRequest(
	appPubKeyHex string,
	gatewayPubKeysHex []string,
	serviceID string,
	supplierAddr string,
	payload []byte,
	now time.Time,
) (*servicetypes.RelayRequest, []byte, error) {
	appPub, err := rings.PubKeyFromHex(appPubKeyHex)
	if err != nil {
		return nil, nil, fmt.Errorf("app pubkey: %w", err)
	}
	if len(gatewayPubKeysHex) == 0 {
		return nil, nil, fmt.Errorf("at least one gateway pubkey is required")
	}

	ringPubKeys := make([]cryptotypes.PubKey, 0, 1+len(gatewayPubKeysHex))
	ringPubKeys = append(ringPubKeys, appPub)
	for i, gwHex := range gatewayPubKeysHex {
		gwPub, err := rings.PubKeyFromHex(gwHex)
		if err != nil {
			return nil, nil, fmt.Errorf("gateway pubkey[%d]: %w", i, err)
		}
		ringPubKeys = append(ringPubKeys, gwPub)
	}

	appRing, err := rings.GetRingFromPubKeys(ringPubKeys)
	if err != nil {
		return nil, nil, fmt.Errorf("build ring: %w", err)
	}

	nonceHex, err := randomSimNonceHex()
	if err != nil {
		return nil, nil, fmt.Errorf("generate simulation nonce: %w", err)
	}

	relayRequest := &servicetypes.RelayRequest{
		Payload: payload,
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      cosmostypes.AccAddress(appPub.Address()).String(),
				ServiceId:               serviceID,
				SessionId:               relayer.FormatSimSessionID(now, nonceHex),
				SessionStartBlockHeight: simSessionStartHeight,
				SessionEndBlockHeight:   simSessionEndHeight,
			},
			SupplierOperatorAddress: supplierAddr,
			Signature:               nil, // filled by SignRelayRequestWithRing below
		},
	}

	if err := c.signer.SignRelayRequestWithRing(relayRequest, appRing); err != nil {
		return nil, nil, fmt.Errorf("sign simulated relay request: %w", err)
	}

	relayRequestBz, err := relayRequest.Marshal()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal simulated relay request: %w", err)
	}

	return relayRequest, relayRequestBz, nil
}

// randomSimNonceHex returns simNonceBytes of crypto/rand entropy, hex-encoded,
// for the synthetic session id's nonce component.
func randomSimNonceHex() (string, error) {
	b := make([]byte, simNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
