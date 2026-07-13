//go:build test

package relayer

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// serveRelayerHTTPServer starts newRelayerHTTPServer on an ephemeral port with
// the given handler and returns its base URL. The server is shut down on test
// cleanup.
func serveRelayerHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := newRelayerHTTPServer(listener.Addr().String(), handler, 30*time.Second)

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.Serve(listener)
	}()

	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		// Serve always returns a non-nil error; after Close it must be ErrServerClosed.
		require.ErrorIs(t, <-serveErrCh, http.ErrServerClosed)
	})

	return "http://" + listener.Addr().String()
}

// h2cClient returns an HTTP client that speaks HTTP/2 cleartext with prior
// knowledge -- the same way a native gRPC client opens a non-TLS connection.
func h2cClient() *http.Client {
	transport := &http.Transport{}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetUnencryptedHTTP2(true)

	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// http1Client returns an HTTP client restricted to HTTP/1.1.
func http1Client() *http.Client {
	transport := &http.Transport{}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)

	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// TestRelayerHTTPServer_ServesH2CAndHTTP1 pins the relayer listener's protocol
// support: native gRPC clients require HTTP/2 cleartext (h2c) on the same port
// that JSON-RPC/REST clients hit over HTTP/1.1. Losing either breaks a
// transport in production.
func TestRelayerHTTPServer_ServesH2CAndHTTP1(t *testing.T) {
	tests := []struct {
		name              string
		client            *http.Client
		expectedProtoText string
		expectedProtoMaj  int
	}{
		{
			name:              "h2c prior knowledge (native gRPC transport)",
			client:            h2cClient(),
			expectedProtoText: "HTTP/2.0",
			expectedProtoMaj:  2,
		},
		{
			name:              "HTTP/1.1 (JSON-RPC, REST, WebSocket upgrade)",
			client:            http1Client(),
			expectedProtoText: "HTTP/1.1",
			expectedProtoMaj:  1,
		},
	}

	// The handler echoes the protocol the server negotiated for the request, so
	// the assertions prove the server-side view, not just the client's.
	baseURL := serveRelayerHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.Proto)
	}))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer tt.client.CloseIdleConnections()

			resp, err := tt.client.Get(baseURL + "/")
			require.NoError(t, err)
			defer func() {
				require.NoError(t, resp.Body.Close())
			}()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, tt.expectedProtoMaj, resp.ProtoMajor, "client-observed protocol")
			require.Equal(t, tt.expectedProtoText, string(body), "server-observed protocol")
		})
	}
}

// TestNewRelayerHTTPServer_Config verifies the server settings the relayer
// depends on: h2c enabled alongside HTTP/1.1, the HTTP/2 stream cap, and the
// timeout policy (WriteTimeout disabled in favour of per-request deadlines).
func TestNewRelayerHTTPServer_Config(t *testing.T) {
	const maxServiceTimeout = 600 * time.Second

	srv := newRelayerHTTPServer("127.0.0.1:8545", http.NotFoundHandler(), maxServiceTimeout)

	require.NotNil(t, srv.Protocols)
	require.True(t, srv.Protocols.HTTP1(), "HTTP/1.1 must stay enabled for JSON-RPC and REST")
	require.True(t, srv.Protocols.UnencryptedHTTP2(), "h2c must be enabled for native gRPC")

	require.NotNil(t, srv.HTTP2)
	require.Equal(t, MaxConcurrentStreams, srv.HTTP2.MaxConcurrentStreams)

	require.Equal(t, "127.0.0.1:8545", srv.Addr)
	require.Equal(t, maxServiceTimeout+ReadTimeoutBuffer, srv.ReadTimeout)
	require.Zero(t, srv.WriteTimeout, "write deadlines are per-request via ResponseController")
	require.Equal(t, DefaultIdleTimeout, srv.IdleTimeout)
}
