//go:build test

package client

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	rpcserver "github.com/cometbft/cometbft/rpc/jsonrpc/server"
	"github.com/stretchr/testify/require"
)

// This file is about one defect, observed live on 2026-08-06: the block
// subscriber lost block events because its pooled HTTP connection was dead and
// it did not know.
//
// Three things have to line up, and the tests below reproduce all three:
//
//  1. The CometBFT RPC server closes a connection left idle for 10s. Nobody
//     configured that — its DefaultConfig sets ReadTimeout: 10s and no
//     IdleTimeout, and net/http then uses ReadTimeout as the idle deadline.
//  2. Blocks arrive slower than that (~10.1s on localnet, ~60s on mainnet), so
//     the pooled connection is ALWAYS past the deadline when the next block
//     event fires.
//  3. Something between client and server withholds the FIN. Go normally
//     notices a server-closed connection on its background read loop and
//     quietly dials a new one — that is why this does not reproduce against a
//     plain local server. Through a forwarder it does: verified by hand
//     against Tilt's port-forward, where a stock transport failed with
//     `Post "http://localhost:26657": EOF`, and in the load test itself, where
//     15 of 15 even-numbered block heights were lost.
//
// The proxy below is (3): a forwarder that does not pass the close along.

// postThroughTransport issues one POST the way the CometBFT JSON-RPC client
// does: same method, same body shape, and no Idempotency-Key — which is what
// makes net/http treat the request as non-replayable, so a stale connection
// surfaces as an error instead of being retried.
func postThroughTransport(client *http.Client, url string) error {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"block"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// Draining is load-bearing, not politeness: net/http only returns a
	// connection to the idle pool once its body has been read to EOF. Closing
	// an unread body throws the connection away, and then every request dials
	// fresh — which silently turns these tests into no-ops, because reuse is
	// the whole thing under test.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// newIdleClosingServer mirrors the CometBFT RPC server's shape: ReadTimeout
// set, IdleTimeout unset, so net/http closes connections idle beyond it.
func newIdleClosingServer(t *testing.T, idleLimit time.Duration) *httptest.Server {
	t.Helper()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	srv.Config.ReadTimeout = idleLimit
	srv.Start()
	t.Cleanup(srv.Close)

	return srv
}

// newSilentCloseProxy forwards TCP to upstream, but when upstream closes it
// leaves the client side open instead of propagating the close. The client
// therefore still believes the connection is usable; it only finds out when it
// writes the next request and gets EOF back.
//
// This is the behaviour of the forwarder in front of the node during the live
// run. Without it Go's transport detects the close on its read loop and
// silently redials, and the defect is invisible.
func newSilentCloseProxy(t *testing.T, upstream string) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var wg sync.WaitGroup
	done := make(chan struct{})

	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			downstream, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = downstream.Close() }()

				up, dialErr := net.Dial("tcp", upstream)
				if dialErr != nil {
					return
				}
				defer func() { _ = up.Close() }()

				upstreamClosed := make(chan struct{})

				// upstream -> client, until upstream closes.
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = io.Copy(downstream, up)
					close(upstreamClosed)
					// Deliberately NOT closing downstream here: that omission
					// is the whole point of this proxy.
				}()

				// client -> upstream. Once upstream is gone, the next thing the
				// client writes ends the connection with no response, which is
				// exactly the EOF seen in production.
				buf := make([]byte, 4096)
				for {
					n, readErr := downstream.Read(buf)
					if n > 0 {
						select {
						case <-upstreamClosed:
							// Let the client finish writing and start waiting
							// for a response before the connection dies. That
							// ordering matters: net/http retries a request it
							// knows it never managed to write, but a request
							// that was written and then got nothing back is a
							// non-replayable POST failure, which is the error
							// seen in production.
							time.Sleep(50 * time.Millisecond)
							return
						default:
						}
						if _, writeErr := up.Write(buf[:n]); writeErr != nil {
							return
						}
					}
					if readErr != nil {
						return
					}
					select {
					case <-done:
						return
					default:
					}
				}
			}()
		}
	}()

	return "http://" + listener.Addr().String()
}

// TestDefaultTransport_LosesRequestsWhenTheCloseIsNotPropagated is the NEGATIVE
// CONTROL: it reproduces the production defect with a stock transport, so the
// test after it is known to be measuring something real.
func TestDefaultTransport_LosesRequestsWhenTheCloseIsNotPropagated(t *testing.T) {
	const (
		serverIdleLimit = 200 * time.Millisecond
		gap             = 500 * time.Millisecond
	)

	srv := newIdleClosingServer(t, serverIdleLimit)
	proxyURL := newSilentCloseProxy(t, srv.Listener.Addr().String())

	client := &http.Client{
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
		Timeout:   5 * time.Second,
	}

	require.NoError(t, postThroughTransport(client, proxyURL), "the first request opens a fresh connection")

	time.Sleep(gap) //nolint:staticcheck // the defect IS the idle interval; there is nothing to poll for

	err := postThroughTransport(client, proxyURL)
	require.Error(t, err,
		"a stock transport must reuse the connection the server already closed — "+
			"if this stops failing, the control is dead and the test below proves nothing")
}

// TestRPCTransport_SurvivesAConnectionClosedWhileIdle is the fix: our transport
// retires an idle connection before the server does, so the next request dials
// a fresh one and never writes into a dead socket.
func TestRPCTransport_SurvivesAConnectionClosedWhileIdle(t *testing.T) {
	const (
		serverIdleLimit = 200 * time.Millisecond
		clientIdle      = 50 * time.Millisecond
		gap             = 500 * time.Millisecond
	)

	srv := newIdleClosingServer(t, serverIdleLimit)
	proxyURL := newSilentCloseProxy(t, srv.Listener.Addr().String())

	client := &http.Client{
		Transport: newRPCTransport(clientIdle, nil),
		Timeout:   5 * time.Second,
	}

	for i := 0; i < 3; i++ {
		require.NoErrorf(t, postThroughTransport(client, proxyURL), "request %d", i)
		time.Sleep(gap) //nolint:staticcheck // the behaviour under test only exists after an idle interval
	}
}

// TestCometbftServerIdleTimeout_MatchesUpstream is the guard on a copied
// constant. cometbftServerIdleTimeout restates a value that lives in CometBFT,
// and a restated value drifts silently — so read the real one and compare.
//
// The node never overrides ReadTimeout (cometbft/node/node.go builds the RPC
// server config from DefaultConfig and replaces only the body/header/batch
// limits, max connections and, conditionally, WriteTimeout), and config.toml
// exposes no read_timeout field, so this default is what every node of this
// version runs with.
//
// What this does NOT prove: the node we talk to may be built from a different
// CometBFT version than the one we compile against. This pins our assumption
// against our dependency, which is the only side we can check.
func TestCometbftServerIdleTimeout_MatchesUpstream(t *testing.T) {
	upstream := rpcserver.DefaultConfig()

	require.Equal(t, upstream.ReadTimeout, cometbftServerIdleTimeout,
		"CometBFT's RPC ReadTimeout changed; it is the idle deadline our connection reuse is sized against")
	require.False(t, reflect.ValueOf(*upstream).FieldByName("IdleTimeout").IsValid(),
		"CometBFT's RPC server config gained an IdleTimeout field; the ReadTimeout inference no longer holds")
}

// TestRPCIdleConnTimeout_StaysBelowTheServerLimit pins the relationship that
// makes the fix work. The margin is not cosmetic: the client must give up on a
// connection strictly before the server does, or the race is back.
func TestRPCIdleConnTimeout_StaysBelowTheServerLimit(t *testing.T) {
	require.Positive(t, rpcIdleConnTimeout, "zero would mean Go's default of 90s, far above the server's limit")
	require.Less(t, rpcIdleConnTimeout, cometbftServerIdleTimeout,
		"the client must retire idle connections before the CometBFT RPC server closes them")
	require.LessOrEqual(t, rpcIdleConnTimeout, cometbftServerIdleTimeout/2,
		"leave at least half the window as margin for scheduling and clock skew")
}

// TestNewRPCTransport_KeepsTLSConfig proves the TLS path did not lose its
// settings when the transport gained an idle timeout.
func TestNewRPCTransport_KeepsTLSConfig(t *testing.T) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	tr := newRPCTransport(rpcIdleConnTimeout, cfg)

	require.NotNil(t, tr.TLSClientConfig)
	require.Equal(t, uint16(tls.VersionTLS12), tr.TLSClientConfig.MinVersion)
	require.Equal(t, rpcIdleConnTimeout, tr.IdleConnTimeout)
}

// TestNewRPCTransport_NilTLSDoesNotForceAMinVersion covers the plaintext path.
// Cloning net/http's default transport carries a TLSClientConfig with ALPN
// protocols set, so the assertion is about not imposing our own TLS policy,
// not about the field being nil.
func TestNewRPCTransport_NilTLSDoesNotForceAMinVersion(t *testing.T) {
	tr := newRPCTransport(rpcIdleConnTimeout, nil)

	require.Equal(t, rpcIdleConnTimeout, tr.IdleConnTimeout)
	if tr.TLSClientConfig != nil {
		require.Zero(t, tr.TLSClientConfig.MinVersion,
			"the plaintext path must not pin a TLS minimum version of its own")
	}
}
