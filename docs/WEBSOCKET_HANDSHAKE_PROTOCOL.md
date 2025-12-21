# WebSocket Handshake Validation Protocol

## Overview

This document specifies the WebSocket handshake validation protocol between PATH (gateway) and RelayMiner (supplier). The handshake carries the same validation information as `RelayRequest.Meta`, enabling the relayer to validate the connection upfront (eager validation), after which subsequent WebSocket messages can be validated either eagerly or optimistically based on relayer configuration.

## Current State (v1.0)

Currently, PATH sends minimal headers during WebSocket handshake:
- `Target-Service-Id`: Service identifier
- `App-Address`: Application address
- `Rpc-Type`: RPC type (2 = WebSocket)

This is insufficient for the relayer to validate the connection properly.

## Proposed Protocol (v2.0)

### Headers Required from PATH

PATH must send the following headers during WebSocket upgrade handshake, mirroring `RelayRequestMetadata` and `SessionHeader`:

| Header | Description | Example |
|--------|-------------|---------|
| `Pocket-Session-Id` | Session identifier | `abc123...` |
| `Pocket-Session-Start-Height` | Session start block height | `1000` |
| `Pocket-Session-End-Height` | Session end block height | `1010` |
| `Pocket-App-Address` | Application address | `pokt1abc...` |
| `Pocket-Service-Id` | Service identifier | `develop-websocket` |
| `Pocket-Supplier-Address` | Target supplier operator address | `pokt1xyz...` |
| `Pocket-Signature` | Ring signature (base64 encoded) | `base64...` |

### Signature Generation

The signature is a ring signature over the handshake parameters, generated the same way as for HTTP relays:

```
signableBytes = hash(sessionId + appAddress + serviceId + supplierAddress + sessionEndHeight)
signature = ring.Sign(signableBytes, gatewayOrAppPrivateKey)
```

### Relayer Validation Flow

```
┌─────────────────────────────────────────────────────────────────┐
│                    WebSocket Upgrade Request                     │
│                                                                  │
│  Headers:                                                        │
│    Pocket-Session-Id: abc123                                    │
│    Pocket-Session-End-Height: 1010                              │
│    Pocket-App-Address: pokt1app...                              │
│    Pocket-Service-Id: develop-websocket                         │
│    Pocket-Supplier-Address: pokt1supplier...                    │
│    Pocket-Signature: <ring-signature>                           │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Relayer Validation (Eager)                    │
│                                                                  │
│  1. Validate Supplier Address                                    │
│     - Pocket-Supplier-Address == relayer.supplierAddress        │
│     - Reject if mismatch (wrong supplier)                        │
│                                                                  │
│  2. Validate Service                                             │
│     - Pocket-Service-Id is configured in relayer                │
│     - Reject if service not supported                            │
│                                                                  │
│  3. Validate Session                                             │
│     - Session exists for app/service combination                 │
│     - Session is active (current height within bounds)           │
│     - Reject if session invalid or expired                       │
│                                                                  │
│  4. Verify Ring Signature                                        │
│     - Reconstruct signable bytes from headers                    │
│     - Verify signature using app's ring                          │
│     - Reject if signature invalid                                │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼ (All validations pass)
┌─────────────────────────────────────────────────────────────────┐
│                    WebSocket Connection Established              │
│                                                                  │
│  - Connection upgraded to WebSocket                              │
│  - Bridge established to backend                                 │
│  - Session context stored for relay processing                   │
│  - Subsequent messages validated per relayer config              │
│    (eager or optimistic)                                         │
└─────────────────────────────────────────────────────────────────┘
```

### Error Responses

| HTTP Status | Reason |
|-------------|--------|
| 400 | Missing required headers |
| 403 | Invalid signature |
| 403 | Wrong supplier (not intended recipient) |
| 404 | Service not configured |
| 404 | Session not found |
| 410 | Session expired |

### Subsequent Message Handling

Once the WebSocket connection is established:

1. **Eager Mode**: Each WebSocket message (RelayRequest) is fully validated before forwarding
2. **Optimistic Mode**: Messages are forwarded immediately, validation happens async

The validation mode is configured per-service in the relayer config.

## Implementation Tasks

### PATH Changes
1. Update `getRelayMinerConnectionHeaders()` to include all required headers
2. Generate ring signature for handshake parameters
3. Include session context in headers

### RelayMiner Changes
1. Update `WebSocketHandler()` to extract and validate all headers
2. Implement signature verification for handshake
3. Validate supplier address matches
4. Cache session context for subsequent message processing
5. Return appropriate error codes for validation failures

## Security Considerations

1. **Replay Protection**: Session end height prevents replaying old handshakes
2. **Supplier Targeting**: Supplier address validation prevents misdirected connections
3. **Ring Signature**: Proves the gateway/app is authorized for the session
4. **Session Validation**: Ensures connection is for a valid, active session

## Backwards Compatibility

During transition:
- RelayMiner should accept connections with old headers (warn in logs)
- New validation is opt-in via config flag
- Once PATH is updated, enforce new validation
