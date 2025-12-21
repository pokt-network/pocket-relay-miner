# Relay Protocol Specification

This document describes the protocol expectations for relay communication between Gateway/Client, Relayer, and Backend services.

## Overview

```
Gateway/Client ←→ Relayer ←→ Backend Service
```

The Relayer acts as a proxy that:
1. Validates relay requests (ring signatures, sessions)
2. Forwards requests to backend services
3. Signs responses with supplier keys
4. Returns signed responses to the gateway

**Important**: All Gateway ↔ Relayer communication uses protobuf-encoded messages (`RelayRequest`/`RelayResponse`), regardless of HTTP headers. Headers like `Content-Type` and `Accept` are for HTTP convention only.

---

## HTTP Protocol

### Gateway → Relayer (Incoming)

| Header            | Required | Description                                                  |
|-------------------|----------|--------------------------------------------------------------|
| `Content-Type`    | No       | Not validated (typically `application/json`)                 |
| `Accept`          | No       | Echoed in response Content-Type (default: `application/json`)|
| `Rpc-Type`        | Yes      | Backend routing: `3` (JSON_RPC), `4` (REST), `5` (COMET_BFT) |
| `Accept-Encoding` | No       | `gzip` to request compressed responses                       |

**Body**: Protobuf-encoded `RelayRequest` (always, regardless of Content-Type header)
- `Meta`: Session header, supplier address, signature
- `Payload`: Serialized `POKTHTTPRequest` (method, URL, headers, body)

### Relayer → Gateway (Outgoing)

| Header             | Condition    | Description                                            |
|--------------------|--------------|--------------------------------------------------------|
| `Content-Type`     | Always       | Echoes client's `Accept` (default: `application/json`) |
| `Content-Encoding` | If requested | `gzip` (when client sent `Accept-Encoding: gzip`)      |

**Body**: Protobuf-encoded `RelayResponse` (always, regardless of Content-Type header)
- `Meta`: Session header, supplier signature
- `Payload`: Serialized `POKTHTTPResponse` (status, headers, **uncompressed** body)

### Relayer → Backend (Outgoing)

| Header               | Sent | Description                                       |
|----------------------|------|---------------------------------------------------|
| `Content-Type`       | Yes  | From `POKTHTTPRequest` (e.g., `application/json`) |
| `Accept-Encoding`    | Yes  | `gzip` (request compressed responses)             |
| `Pocket-Supplier`    | Yes  | Supplier operator address                         |
| `Pocket-Service`     | Yes  | Service ID                                        |
| `Pocket-Application` | Yes  | Application address                               |

**Body**: Raw request body from `POKTHTTPRequest.BodyBz`

### Backend → Relayer (Incoming)

| Header             | Description                               |
|--------------------|-------------------------------------------|
| `Content-Encoding` | `gzip` if backend compressed the response |

**Body**: Raw backend response (JSON-RPC, REST, etc.)

**Note**: Relayer decompresses gzipped responses before signing. The `POKTHTTPResponse.BodyBz` always contains uncompressed data so the gateway can parse it.

---

## gRPC Protocol

### Gateway → Relayer (Incoming)

| Metadata        | Required | Description                    |
|-----------------|----------|--------------------------------|
| `rpc-type`      | Yes      | `1` (GRPC)                     |
| `grpc-encoding` | No       | `gzip` for compressed messages |

**Message**: Protobuf `RelayRequest` (same structure as HTTP)

### Relayer → Gateway (Outgoing)

| Metadata        | Description                       |
|-----------------|-----------------------------------|
| `grpc-encoding` | `gzip` if compression negotiated  |

**Message**: Protobuf `RelayResponse` (same structure as HTTP)

### Relayer → Backend

gRPC passthrough with metadata:

| Metadata             | Description                                 |
|----------------------|---------------------------------------------|
| `pocket-supplier`    | Supplier operator address                   |
| `pocket-service`     | Service ID                                  |
| `pocket-application` | Application address (from incoming request) |

**Compression**: gRPC handles compression via `grpc-encoding` header automatically when both sides have the gzip compressor registered.

---

## WebSocket Protocol

### Gateway → Relayer (Connection)

| Header                     | Required | Description                                     |
|----------------------------|----------|-------------------------------------------------|
| `Pocket-Service-Id`        | Yes      | Service ID for routing                          |
| `Pocket-Supplier-Address`  | No       | Preferred supplier                              |
| `Rpc-Type`                 | Yes      | `2` (WEBSOCKET)                                 |
| `Sec-WebSocket-Extensions` | No       | `permessage-deflate` for compression (RFC 7692) |

### Gateway → Relayer (Messages)

**Message Type**: Binary

**Body**: Protobuf-encoded `RelayRequest` where `Payload` contains raw WebSocket message (e.g., JSON-RPC)

### Relayer → Gateway (Messages)

**Message Type**: Binary

**Body**: Protobuf-encoded `RelayResponse` where `Payload` contains raw backend response

**Compression**: Per-message deflate (RFC 7692) if negotiated during handshake.

### Relayer → Backend (Connection)

| Header                     | Description                                |
|----------------------------|--------------------------------------------|
| `Pocket-Supplier`          | Supplier operator address                  |
| `Pocket-Service`           | Service ID                                 |
| `Sec-WebSocket-Extensions` | `permessage-deflate` (compression enabled) |

### Message Flow

```
Gateway sends: RelayRequest { Payload: raw_json_rpc }
     ↓
Relayer extracts Payload, forwards to Backend
     ↓
Backend responds with raw data
     ↓
Relayer wraps in signed RelayResponse { Payload: raw_response }
     ↓
Gateway receives: RelayResponse (verifies signature, extracts Payload)
```

---

## Rpc-Type Values

Reference: `poktroll/x/shared/types/service.pb.go`

| Value | Type      | Description         |
|-------|-----------|---------------------|
| `1`   | GRPC      | gRPC backend        |
| `2`   | WEBSOCKET | WebSocket backend   |
| `3`   | JSON_RPC  | JSON-RPC over HTTP  |
| `4`   | REST      | REST API over HTTP  |
| `5`   | COMET_BFT | CometBFT RPC (HTTP) |

---

## Compression Summary

| Protocol  | Standard  | Client Request                                 | Server Response          |
|-----------|-----------|------------------------------------------------|--------------------------|
| HTTP      | RFC 7231  | `Accept-Encoding: gzip`                        | `Content-Encoding: gzip` |
| gRPC      | gRPC spec | `grpc-encoding: gzip`                          | `grpc-encoding: gzip`    |
| WebSocket | RFC 7692  | `Sec-WebSocket-Extensions: permessage-deflate` | Negotiated per-message   |

---

## Pocket Context Headers

These headers are forwarded to backends for observability:

| Header               | Value                                           |
|----------------------|-------------------------------------------------|
| `Pocket-Supplier`    | Supplier operator address (e.g., `pokt1abc...`) |
| `Pocket-Service`     | Service ID (e.g., `eth-mainnet`)                |
| `Pocket-Application` | Application address from session                |

For gRPC, these are sent as lowercase metadata keys.
