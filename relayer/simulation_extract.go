package relayer

import (
	"net/http"

	"google.golang.org/grpc/metadata"
)

// Header/metadata names carrying the simulation directive. Each transport reads
// the same logical signal through its own native channel (HTTP header, gRPC
// metadata, WS handshake header) — the same pattern the existing Rpc-Type
// selector already uses. gRPC metadata keys are lowercase by HTTP/2 convention.
const (
	HeaderSimulationKeyID = "Pocket-Simulation-Key-Id"
	MetaSimulationKeyID   = "pocket-simulation-key-id"
)

// SimDirectiveFromHTTP extracts the simulation directive from HTTP request
// headers (jsonrpc, cometbft, rest/stream). Absent header => empty KeyID =>
// normal relay.
func SimDirectiveFromHTTP(h http.Header) SimulationDirective {
	return SimulationDirective{KeyID: h.Get(HeaderSimulationKeyID)}
}

// SimDirectiveFromGRPC extracts the simulation directive from gRPC metadata.
func SimDirectiveFromGRPC(md metadata.MD) SimulationDirective {
	vals := md.Get(MetaSimulationKeyID)
	if len(vals) == 0 {
		return SimulationDirective{}
	}
	return SimulationDirective{KeyID: vals[0]}
}

// SimDirectiveFromWS extracts the simulation directive from a WebSocket
// handshake's HTTP headers. Read once at handshake; the resulting bit lives on
// the bridge for the connection's lifetime.
func SimDirectiveFromWS(h http.Header) SimulationDirective {
	return SimulationDirective{KeyID: h.Get(HeaderSimulationKeyID)}
}
