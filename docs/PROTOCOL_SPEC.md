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
| `Rpc-Type`        | No       | Backend routing: `3` (JSON_RPC), `4` (REST), `5` (COMET_BFT). Absent: the service's `default_backend`, else JSON-RPC |
| `Accept-Encoding` | No       | `gzip` to accept a compressed response (only honoured with `response_compression.enabled`) |

**Body**: Protobuf-encoded `RelayRequest` (always, regardless of Content-Type header)
- `Meta`: Session header, supplier address, signature
- `Payload`: Serialized `POKTHTTPRequest` (method, URL, headers, body)

### Relayer → Gateway (Outgoing)

| Header             | Condition    | Description                                            |
|--------------------|--------------|--------------------------------------------------------|
| `Content-Type`     | Always       | Echoes client's `Accept` (default: `application/json`) |
| `Content-Encoding` | If enabled   | `gzip` when `response_compression.enabled` is true (default false), the client sent `Accept-Encoding: gzip` and the response is at least `min_size_bytes` |

**Body**: Protobuf-encoded `RelayResponse` (always, regardless of Content-Type header)
- `Meta`: Session header, supplier signature
- `Payload`: Serialized `POKTHTTPResponse` (status, headers, **uncompressed** body)

### Relayer → Backend (Outgoing)

| Header               | Sent | Description                                       |
|----------------------|------|---------------------------------------------------|
| `Content-Type`       | Yes  | From `POKTHTTPRequest` (e.g., `application/json`) |
| `Accept-Encoding`    | Yes  | `identity` (asks the backend for an uncompressed response) |
| `Pocket-Supplier`    | Yes  | Supplier operator address                         |
| `Pocket-Service`     | Yes  | Service ID                                        |
| `Pocket-Application` | Yes  | Application address                               |

**Body**: Raw request body from `POKTHTTPRequest.BodyBz`

### Backend → Relayer (Incoming)

**Body**: Raw backend response (JSON-RPC, REST, etc.)

**Note**: The relayer asks for an uncompressed response (`Accept-Encoding:
identity`) and does not decompress: a backend that compresses anyway has its
bytes signed and passed through as they came.

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
| `Target-Service-Id`        | Yes      | Service ID for routing (`Pocket-Service-Id` is read as a legacy fallback) |
| `Pocket-Supplier-Address`  | No       | Preferred supplier                              |
| `Rpc-Type`                 | No       | `2` (WEBSOCKET); the upgrade request itself selects the WebSocket path |

### Gateway → Relayer (Messages)

**Message Type**: Binary

**Body**: Protobuf-encoded `RelayRequest` where `Payload` contains raw WebSocket message (e.g., JSON-RPC)

### Relayer → Gateway (Messages)

**Message Type**: Binary

**Body**: Protobuf-encoded `RelayResponse` where `Payload` contains raw backend response

**Compression**: none. `permessage-deflate` is disabled on both sides (the
relayer's upgrader and its backend dialer).

### Relayer → Backend (Connection)

| Header                     | Description                                |
|----------------------------|--------------------------------------------|
| `Pocket-Supplier`          | Supplier operator address                  |
| `Pocket-Service`           | Service ID                                 |

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
| HTTP      | RFC 7231  | `Accept-Encoding: gzip`                        | `Content-Encoding: gzip`, only with `response_compression.enabled` (default false) |
| gRPC      | gRPC spec | `grpc-encoding: gzip`                          | `grpc-encoding: gzip`    |
| WebSocket | —         | not supported                                  | none                     |

---

## Pocket Context Headers

These headers are forwarded to backends for observability:

| Header               | Value                                           |
|----------------------|-------------------------------------------------|
| `Pocket-Supplier`    | Supplier operator address (e.g., `pokt1abc...`) |
| `Pocket-Service`     | Service ID (e.g., `eth-mainnet`)                |
| `Pocket-Application` | Application address from session                |

For gRPC, these are sent as lowercase metadata keys.
