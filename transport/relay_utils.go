package transport

import (
	"crypto/sha256"
	"fmt"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// RelayMetadata contains the extracted metadata from a Relay.
// This is used to avoid unmarshaling the full relay when only metadata is needed.
type RelayMetadata struct {
	SessionID               string
	SessionStartHeight      int64
	SessionEndHeight        int64
	ApplicationAddress      string
	ServiceID               string
	SupplierOperatorAddress string
}

// ExtractRelayMetadata extracts metadata from serialized relay bytes.
// This avoids duplicating metadata in the MinedRelayMessage.
func ExtractRelayMetadata(relayBytes []byte) (*RelayMetadata, error) {
	if len(relayBytes) == 0 {
		return nil, fmt.Errorf("relay bytes is empty")
	}

	var relay servicetypes.Relay
	if err := relay.Unmarshal(relayBytes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal relay: %w", err)
	}

	if relay.Req == nil || relay.Req.Meta.SessionHeader == nil {
		return nil, fmt.Errorf("relay request or session header is nil")
	}

	header := relay.Req.Meta.SessionHeader
	meta := &RelayMetadata{
		SessionID:          header.SessionId,
		SessionStartHeight: header.SessionStartBlockHeight,
		SessionEndHeight:   header.SessionEndBlockHeight,
		ApplicationAddress: header.ApplicationAddress,
		ServiceID:          header.ServiceId,
	}

	// NOTE: SupplierOperatorAddress is NOT in the relay - it comes from:
	// 1. The stream name (publisher routes by supplier)
	// 2. Must be passed separately or extracted from signing context
	// The signature is in relay.Res.Meta.SupplierOperatorSignature but not the address

	return meta, nil
}

// ComputeRelayHash computes the SHA256 hash of relay bytes.
// This can be used instead of storing the hash in the message.
func ComputeRelayHash(relayBytes []byte) []byte {
	hash := sha256.Sum256(relayBytes)
	return hash[:]
}
