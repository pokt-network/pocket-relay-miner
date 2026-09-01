package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	sdktypes "github.com/pokt-network/shannon-sdk/types"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// RunCometBFTMode sends a CometBFT (RPCType 5) relay to the relayer. CometBFT RPC
// is JSON-RPC 2.0 over HTTP, so this mirrors the jsonrpc mode but tags the relay
// with Rpc-Type: 5 so the relayer routes it to a cometbft backend (e.g. a
// validator's :26657 endpoint). Diagnostic (single-relay) only for now; load
// testing goes through the jsonrpc mode.
func RunCometBFTMode(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient) error {
	applyRelayTimeout()

	payloadBz, err := buildCometBFTPayload()
	if err != nil {
		return fmt.Errorf("failed to build payload: %w", err)
	}

	if RelayLoadTest {
		return fmt.Errorf("load test mode is not supported for cometbft relays (use jsonrpc for load testing)")
	}

	result := BuildAndSendRelay(ctx, logger, client, payloadBz, sendCometBFTRelay)
	DisplayDiagnosticResult(client, result)
	if !result.Success {
		return result.Error
	}
	return nil
}

// buildCometBFTPayload creates a serialized POKTHTTPRequest carrying a CometBFT
// JSON-RPC request. It defaults to `status` (returns node/sync info, a good smoke
// test); a caller-supplied --payload (e.g. `health`, `abci_info`) is forwarded
// verbatim so no re-encoding can alter it.
func buildCometBFTPayload() ([]byte, error) {
	var jsonPayload []byte
	if RelayPayloadJSON != "" {
		jsonPayload = []byte(RelayPayloadJSON)
	} else {
		body := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "status",
			"params":  []any{},
		}
		var err error
		jsonPayload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal CometBFT payload: %w", err)
		}
	}

	httpReq, err := http.NewRequest("POST", "/", bytes.NewReader(jsonPayload))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	_, poktHTTPRequestBz, err := sdktypes.SerializeHTTPRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize POKTHTTPRequest: %w", err)
	}
	return poktHTTPRequestBz, nil
}

// sendCometBFTRelay sends a relay tagged as CometBFT (Rpc-Type: 5).
func sendCometBFTRelay(ctx context.Context, relayRequestBz []byte) ([]byte, http.Header, error) {
	return sendRelayOverHTTP(ctx, relayRequestBz, rpcTypeCometBFT)
}
