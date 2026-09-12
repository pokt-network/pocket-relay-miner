package relayer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// WebSocket timeout constants for connection keep-alive.
// These are fixed values for ping/pong health checks and are NOT derived from timeout profiles.
// WebSocket connections are long-lived and session-based - they don't follow request/response timeout patterns.
const (
	// wsWriteWait is the time allowed to write a message to the peer.
	wsWriteWait = 10 * time.Second

	// wsPongWait is the time allowed to wait for the next pong message.
	wsPongWait = 30 * time.Second

	// wsPingPeriod is the send pings to peer with this period. Must be less than pongWait.
	wsPingPeriod = (wsPongWait * 9) / 10

	// defaultWSDialTimeout is the default timeout for establishing WebSocket connection.
	defaultWSDialTimeout = 10 * time.Second
)

// wsMaxMessageBytes caps a single inbound WebSocket frame on both the gateway
// and backend connections.
//
// gorilla buffers an entire frame in memory before ReadMessage returns and its
// default limit is unlimited, so one oversized frame OOMs the process. On the
// gateway side this is reachable pre-auth by anyone: CheckOrigin accepts every
// origin, validateAndLogWebSocketHandshake is permissive by design and never
// rejects, and readLoop allocates the frame before handleGatewayMessage checks
// a ring signature -- so the attacker needs no key, no stake, and no session.
// The backend side is bounded too, since a compromised or hostile backend can
// send an abusive frame just as easily.
//
// 15MB matches PATH's maxMessageBytes and go-ethereum's wsMessageSizeLimit,
// which is what the EVM backends behind this relayer enforce on their own
// output. Matching the rest of the stack is deliberate: a tighter cap here
// would reject frames PATH already accepted and forwarded, reintroducing the
// class of cross-component divergence this bridge exists to avoid.
//
// When exceeded, ReadMessage returns websocket.ErrReadLimit and readLoop tears
// the bridge down through its existing close path, reporting 1009. gorilla also
// emits its own 1009 from inside advanceFrame, but only as a documented BEST
// EFFORT (it discards the WriteControl error), so the close code the peer
// actually observes has to come from our own close path -- see readLoop.
//
// var (not const) so tests can shrink it: exercising the limit at 15MB makes the
// test depend on pushing 15MB through loopback inside a deadline, which is a
// timing race, not a behaviour check.
var wsMaxMessageBytes int64 = 15 * 1024 * 1024

// wsFirstFrameWait bounds how long a connection may stay open without having
// sent a frame that passes admission.
//
// TWO MINUTES, set by the owner (Jorge, 2026-09-03): "2m hardcode, no veo que ni
// 1m sea necesario, so 2m es mas que de sobra". Hardcoded on purpose, no config
// knob.
//
// The measurement it was decided against: PATH upgrades its CLIENT and then
// dials the relayminer immediately (path/websockets/bridge.go:97-105), and the
// handshake RelayRequest it builds (protocol/shannon/websocket_context.go:493)
// exists only to produce the Pocket-Signature header -- it is never written to
// the socket. So the first frame arrives when the client speaks, not when the
// gateway connects, and the residual is real and accepted: a client that holds a
// socket open for more than two minutes before its first subscribe is closed
// with 1013 and reconnects.
//
// A var, like wsMaxMessageBytes above, so a test can drive it in milliseconds.
var wsFirstFrameWait = 2 * time.Minute

// wsCloseSettle is how long release waits between writing the close frames and
// closing the sockets, so a peer can actually receive them.
const wsCloseSettle = 100 * time.Millisecond

// RFC 6455 WebSocket Close Codes
// https://datatracker.ietf.org/doc/html/rfc6455#section-7.4.1
const (
	CloseNormalClosure           = 1000 // Normal closure; the connection successfully completed
	CloseGoingAway               = 1001 // Endpoint is going away (e.g., server shutdown, browser navigating away)
	CloseProtocolError           = 1002 // Protocol error
	CloseUnsupportedData         = 1003 // Received data type cannot be accepted
	CloseNoStatusReceived        = 1005 // No status code was provided (reserved, must not be sent)
	CloseAbnormalClosure         = 1006 // Connection closed abnormally (reserved, must not be sent)
	CloseInvalidPayload          = 1007 // Received data was inconsistent with message type
	ClosePolicyViolation         = 1008 // Received message violates policy
	CloseMessageTooBig           = 1009 // Message too big to process
	CloseMandatoryExtension      = 1010 // Expected extension was not negotiated
	CloseInternalError           = 1011 // Server encountered unexpected condition
	CloseServiceRestart          = 1012 // Server is restarting
	CloseTryAgainLater           = 1013 // Server is overloaded, try again later
	CloseBadGateway              = 1014 // Gateway received invalid response from upstream (unofficial)
	CloseTLSHandshakeFailed      = 1015 // TLS handshake failed (reserved, must not be sent)
	CloseSessionExpired          = 4000 // Custom: Pocket session expired
	CloseValidationFailed        = 4001 // Custom: Relay validation failed
	CloseStakeLimitExceeded      = 4002 // Custom: Application stake limit exceeded
	CloseBackendConnectionFailed = 4003 // Custom: Failed to connect to backend
)

// closeCodeName returns a human-readable name for a WebSocket close code.
func closeCodeName(code int) string {
	switch code {
	case CloseNormalClosure:
		return "NormalClosure"
	case CloseGoingAway:
		return "GoingAway"
	case CloseProtocolError:
		return "ProtocolError"
	case CloseUnsupportedData:
		return "UnsupportedData"
	case CloseNoStatusReceived:
		return "NoStatusReceived"
	case CloseAbnormalClosure:
		return "AbnormalClosure"
	case CloseInvalidPayload:
		return "InvalidPayload"
	case ClosePolicyViolation:
		return "PolicyViolation"
	case CloseMessageTooBig:
		return "MessageTooBig"
	case CloseMandatoryExtension:
		return "MandatoryExtension"
	case CloseInternalError:
		return "InternalError"
	case CloseServiceRestart:
		return "ServiceRestart"
	case CloseTryAgainLater:
		return "TryAgainLater"
	case CloseBadGateway:
		return "BadGateway"
	case CloseTLSHandshakeFailed:
		return "TLSHandshakeFailed"
	case CloseSessionExpired:
		return "SessionExpired"
	case CloseValidationFailed:
		return "ValidationFailed"
	case CloseStakeLimitExceeded:
		return "StakeLimitExceeded"
	case CloseBackendConnectionFailed:
		return "BackendConnectionFailed"
	default:
		return "Unknown"
	}
}

// closeInitiatorForSource maps a message source onto one of the three declared
// initiators. The two vocabularies do not line up -- the source side calls the
// upstream peer "gateway" and the initiator side calls it "client" -- so
// converting one string to the other directly produced a FOURTH initiator value
// that no constant declares. Harmless while nothing read it; the moment a metric
// carries it as a label, client-initiated closes split across two series.
func closeInitiatorForSource(source wsMessageSource) wsCloseInitiator {
	switch source {
	case wsMessageSourceGateway:
		return wsCloseInitiatorClient
	case wsMessageSourceBackend:
		return wsCloseInitiatorBackend
	default:
		return wsCloseInitiatorRelayer
	}
}

// wsCloseInitiator identifies who initiated a WebSocket close.
type wsCloseInitiator string

const (
	wsCloseInitiatorClient  wsCloseInitiator = "client"  // PATH gateway (upstream)
	wsCloseInitiatorBackend wsCloseInitiator = "backend" // Backend service (downstream)
	wsCloseInitiatorRelayer wsCloseInitiator = "relayer" // This relayer (bridge)
)

// getWSDialTimeout returns the dial timeout for WebSocket connections.
// Uses the dial_timeout_seconds from the timeout profile if available.
func getWSDialTimeout(profile *TimeoutProfile) time.Duration {
	if profile != nil && profile.DialTimeoutSeconds > 0 {
		return time.Duration(profile.DialTimeoutSeconds) * time.Second
	}
	return defaultWSDialTimeout
}

// wsMessageSource represents the source of a WebSocket message.
type wsMessageSource string

const (
	wsMessageSourceBackend wsMessageSource = "backend"
	wsMessageSourceGateway wsMessageSource = "gateway"
)

// wsMessage represents a message in the WebSocket bridge.
type wsMessage struct {
	data        []byte
	source      wsMessageSource
	messageType int
}

// WebSocketBridge handles bidirectional WebSocket communication between
// a gateway client and a backend service.
type WebSocketBridge struct {
	logger      logging.Logger
	gatewayConn *websocket.Conn

	// backendConn is nil until a frame has passed admission. The backend is a
	// resource the operator pays for, and it is not dialled on the strength of a
	// WebSocket upgrade alone: before this, anyone who could open a socket could
	// push the operator's backend without ever sending a relay.
	//
	// A PLAIN FIELD and not an atomic, and that is a property of the lifecycle
	// rather than an oversight: it is written once, by awaitFirstFrame, on the
	// Run goroutine, before any other goroutine exists; and it is read by
	// release, on that same goroutine. Nothing that signals a close touches it.
	backendConn *websocket.Conn

	// What awaitFirstFrame needs to dial, held from construction because the
	// dial no longer happens there.
	backendURL     string
	backendHeaders http.Header
	dialTimeout    time.Duration

	// onBackendDial reports the outcome of the dial to whoever built this bridge,
	// which owns the circuit breaker for the endpoint. It used to be recorded as
	// an unconditional success right after construction, which was accurate only
	// while the constructor dialled. Optional: nil means nobody is watching.
	onBackendDial func(statusCode int, err error)

	// firstFrameWait is captured from wsFirstFrameWait at construction rather
	// than read later: the constructor runs on the caller's goroutine, so a test
	// that shortens the package var is ordered with this read.
	firstFrameWait time.Duration

	relayProcessor RelayProcessor
	publisher      transport.MinedRelayPublisher
	responseSigner *ResponseSigner
	relayPipeline  *RelayPipeline // Unified relay processing pipeline

	// Message channel for bridge communication
	msgChan chan wsMessage

	// Write mutexes for WebSocket connections.
	// gorilla/websocket supports one concurrent reader and one concurrent writer,
	// but multiple goroutines write to each connection (messageLoop, pingLoop,
	// closeWithReason, handleSessionExpiration), so we serialize writes.
	gatewayWriteMu sync.Mutex
	backendWriteMu sync.Mutex

	// Track latest request/response for pairing
	latestRequest  *servicetypes.RelayRequest
	latestResponse *servicetypes.RelayResponse
	latestMu       sync.RWMutex

	// Service info
	serviceID     string
	arrivalHeight int64

	// owner is the supplier operator address this bridge belongs to, and it is
	// the ONLY supplier this connection may ever mine, meter or sign for.
	//
	// It has two sources and one moment of decision. A v2 handshake carries
	// Pocket-Supplier-Address, so the owner exists before the first frame; v1
	// and sage carry nothing, so it is adopted from the first RelayRequest.
	// From that moment on the bridge HAS an owner: adoptOrVerifyOwner requires
	// every subsequent frame to name the same one, and closes the connection
	// otherwise.
	//
	// It replaced a plain string that was written once at construction and
	// never again, while a comment promised the bridge would "extract from
	// first RelayRequest". It did not: a sage connection metered against an
	// EMPTY supplier segment, which is not a mis-labelled key but a SHARED
	// budget -- measured 2026-09-03, two suppliers on one session produced a
	// single "<session>::consumed" counter reading 2. The per-supplier limit is
	// computed by dividing the app stake by the session's supplier count, so
	// sharing one counter throttles the session to roughly 1/N.
	//
	// atomic.Pointer and not a plain field because it is written from
	// messageLoop and read from the SessionMonitor callback goroutine
	// (handleSessionExpiration -> sendSessionExpirationMessage).
	owner atomic.Pointer[string]

	// Simulation (optional). When simulated is true, every gateway message on
	// this connection goes through simVerifier's Admission zone instead of
	// relayPipeline.ValidateRelay/MeterRelay -- a simulated relay must never
	// consume stake. simKeyID is the pinned identity bound to this connection
	// for its whole lifetime (set once at the handshake). The forward-to-
	// backend, response-signing, and emit steps are the SHARED data path and
	// run unchanged for both simulated and real connections.
	simulated   bool
	simVerifier *SimulationVerifier
	simKeyID    string

	// Relay counting for billing
	relayCount atomic.Uint64

	// Session expiration monitoring (global, shared across all connections)
	sessionMonitor   *SessionMonitor // Global session monitor
	sessionEndHeight int64           // Session end block height

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup

	// closeReason holds the FIRST verdict recorded by whichever goroutine
	// noticed the bridge is finished. It replaced a `closed atomic.Bool` that
	// several goroutines both set and polled: a boolean says a close happened,
	// which is not enough to build the close frame, so the code that needed the
	// code and the reason had to go and find them elsewhere.
	closeReason atomic.Pointer[wsCloseReason]
}

// WebSocketUpgrader upgrades HTTP connections to WebSocket.
var WebSocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Accept connections from any origin for cross-origin support
	CheckOrigin: func(r *http.Request) bool { return true },
	// Disable per-message compression to save ~300KB of flate state per connection.
	// Relay payloads are small protobuf messages where compression overhead exceeds savings.
	EnableCompression: false,
}

// NewWebSocketBridge creates a new WebSocket bridge.
// The dialTimeout is derived from the service's timeout profile.
//
// simulated, simVerifier, and simKeyID wire the simulated-relay Admission
// zone (mirrors the HTTP/gRPC serveSimulated* pattern). When simulated is
// true, the caller (WebSocketHandler) MUST also have passed a nil publisher
// -- the Accounting skip for a simulated connection. When simulated is
// false, simVerifier/simKeyID are unused.
func NewWebSocketBridge(
	logger logging.Logger,
	gatewayConn *websocket.Conn,
	backendURL string,
	serviceID string,
	supplierAddress string,
	arrivalHeight int64,
	relayProcessor RelayProcessor,
	publisher transport.MinedRelayPublisher,
	responseSigner *ResponseSigner,
	headers http.Header,
	sessionMonitor *SessionMonitor,
	relayPipeline *RelayPipeline,
	dialTimeout time.Duration,
	simulated bool,
	simVerifier *SimulationVerifier,
	simKeyID string,
	onBackendDial func(statusCode int, err error),
) (*WebSocketBridge, error) {
	// A nil relayProcessor used to drop us into a "fallback" emit path that
	// published MinedRelayMessage{RelayHash: nil, CU: 1}, which silently
	// collapsed every websocket event into a single SMST leaf (all empty
	// keys) and underbilled compute units. Fail loudly instead so a
	// mis-wired bridge cannot quietly eat revenue.
	if relayProcessor == nil {
		return nil, fmt.Errorf("websocket bridge requires a non-nil RelayProcessor (fallback emit path removed to prevent data loss)")
	}

	ctx, cancelFn := context.WithCancel(context.Background())

	// Bound inbound frame size on the gateway connection, the side reachable
	// pre-auth -- see wsMaxMessageBytes. The backend gets the same limit in
	// ensureBackend, when a frame has earned it.
	gatewayConn.SetReadLimit(wsMaxMessageBytes)

	bridge := &WebSocketBridge{
		logger:           logger.With().Str(logging.FieldComponent, logging.ComponentWebsocketBridge).Str(logging.FieldServiceID, serviceID).Logger(),
		gatewayConn:      gatewayConn,
		backendURL:       backendURL,
		backendHeaders:   headers,
		dialTimeout:      dialTimeout,
		firstFrameWait:   wsFirstFrameWait,
		relayProcessor:   relayProcessor,
		publisher:        countPublished(publisher),
		responseSigner:   responseSigner,
		relayPipeline:    relayPipeline,
		msgChan:          make(chan wsMessage, 100),
		serviceID:        serviceID,
		arrivalHeight:    arrivalHeight,
		simulated:        simulated,
		simVerifier:      simVerifier,
		simKeyID:         simKeyID,
		sessionMonitor:   sessionMonitor,
		onBackendDial:    onBackendDial,
		sessionEndHeight: 0, // Will be set from first relay request
		ctx:              ctx,
		cancelFn:         cancelFn,
	}

	// A v2 handshake names the supplier, so the bridge has an owner before the
	// first frame; v1 and sage do not, and the owner is adopted from the first
	// RelayRequest instead. Either way every later frame must name the same one.
	if supplierAddress != "" {
		bridge.owner.Store(&supplierAddress)
	}

	wsConnectionsTotal.WithLabelValues(serviceID).Inc()

	return bridge, nil
}

// ensureBackend dials the backend, and is reachable only from the first frame.
//
// No atomics, no re-checks, no closed flag -- awaitFirstFrame runs it on the Run
// goroutine before any other goroutine exists, and phase two never starts unless
// it succeeded. That is what the two-phase lifecycle buys: the previous shape
// dialled from inside the message loop with four goroutines already running, and
// every guard it needed was a consequence of that.
func (b *WebSocketBridge) ensureBackend() error {
	if b.backendConn != nil {
		return nil
	}

	// The backend's Pocket-Supplier header is set HERE and not at the handshake,
	// which is the second thing deferring the dial buys: at handshake time the
	// owner is unknown for v1 and sage, so the header the backend saw was empty.
	if owner := b.ownerAddress(); owner != "" {
		b.backendHeaders.Set(HeaderPocketSupplier, owner)
	}

	conn, err := connectWebSocketBackend(b.backendURL, b.backendHeaders, b.dialTimeout)

	// The circuit breaker learns about the backend HERE and nowhere else. It
	// used to be told "200, no error" right after the bridge was constructed,
	// which was true only while the constructor dialled.
	if b.onBackendDial != nil {
		if err != nil {
			b.onBackendDial(0, err)
		} else {
			b.onBackendDial(http.StatusOK, nil)
		}
	}
	if err != nil {
		return err
	}

	conn.SetReadLimit(wsMaxMessageBytes)
	b.backendConn = conn
	b.logger.Debug().Msg("backend connection established after the first admitted frame")
	return nil
}

// connectWebSocketBackend establishes a WebSocket connection to the backend.
// The dialTimeout is derived from the service's timeout profile.
func connectWebSocketBackend(backendURL string, headers http.Header, dialTimeout time.Duration) (*websocket.Conn, error) {
	parsedURL, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}

	// Create dialer with timeout from profile.
	// Compression is disabled to save ~150KB of flate state per connection.
	dialer := &websocket.Dialer{
		EnableCompression: false,
		HandshakeTimeout:  dialTimeout,
	}

	// Use TLS for wss:// scheme
	if parsedURL.Scheme == "wss" {
		dialer.TLSClientConfig = &tls.Config{}
	}

	conn, _, err := dialer.Dial(backendURL, headers)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// Run drives the bridge: first frame, then loops, then teardown. It blocks
// until the bridge is finished and it is the ONLY place that releases.
//
// THE LIFECYCLE HAS TWO PHASES AND ONLY THE SECOND IS CONCURRENT.
//
// Phase one reads the first frame synchronously, right here, with a native read
// deadline. No goroutine exists yet, so the backend dial that phase one performs
// cannot race a close, a panic, or another frame -- which is the whole class of
// defect this shape replaces. Four separate guards were written one at a time
// against that class before it was removed instead.
//
// Phase two starts the four loops and the message loop, by which point the
// backend either exists or the bridge is already gone.
func (b *WebSocketBridge) Run() {
	// Acquired here and released in release, three lines apart and in the same
	// function. It used to be incremented in the constructor and decremented in
	// the teardown, so a bridge that was built and never Run left the gauge
	// wrong for the life of the process.
	wsConnectionsActive.WithLabelValues(b.serviceID).Inc()
	defer b.release()

	if !b.awaitFirstFrame() {
		return
	}

	b.wg.Add(4)
	go logging.RecoverGoRoutine(b.logger, "websocket_read_gateway", func(ctx context.Context) {
		b.readLoop(b.gatewayConn, wsMessageSourceGateway)
	})(b.ctx)
	go logging.RecoverGoRoutine(b.logger, "websocket_read_backend", func(ctx context.Context) {
		b.readLoop(b.backendConn, wsMessageSourceBackend)
	})(b.ctx)
	go logging.RecoverGoRoutine(b.logger, "websocket_ping_gateway", func(ctx context.Context) {
		b.pingLoop(b.gatewayConn, &b.gatewayWriteMu, wsMessageSourceGateway)
	})(b.ctx)
	go logging.RecoverGoRoutine(b.logger, "websocket_ping_backend", func(ctx context.Context) {
		b.pingLoop(b.backendConn, &b.backendWriteMu, wsMessageSourceBackend)
	})(b.ctx)

	// Note: Session expiration monitoring happens via global SessionMonitor.
	// This bridge registers itself when session parameters are known.
	b.messageLoop()
}

// awaitFirstFrame reads and admits the frame that earns this connection its
// backend, and reports whether the bridge should go on to phase two.
//
// A NATIVE read deadline, not a goroutine watching a timer. That is possible
// only because pingLoop has not started: gorilla's DEFAULT ping handler replies
// to a ping without touching the read deadline (conn.go:1158-1170 in the
// vendored v1.5.3), while the SetPongHandler pingLoop installs REFRESHES it. So
// during this window the gateway's pings are answered and keep the peer happy,
// and a client that answers pings forever without asking for anything still
// hits the deadline. The previous shape needed a fifth goroutine, a flag and a
// package var to say the same thing.
func (b *WebSocketBridge) awaitFirstFrame() bool {
	if err := b.gatewayConn.SetReadDeadline(time.Now().Add(b.firstFrameWait)); err != nil {
		b.logger.Debug().Err(err).Msg("failed to set first-frame deadline")
		_ = b.closeWithReason(CloseInternalError, "internal error", wsCloseInitiatorRelayer)
		return false
	}

	// A shutdown that signalled this bridge BEFORE the line above would have
	// been erased by it: closeWithReason nudges the read deadline to now, and
	// the deadline just set is firstFrameWait away -- two minutes in production,
	// four times the whole shutdown budget. The signal is durable in the CONTEXT,
	// so re-reading it here makes the two orders equivalent: signalled first and
	// we leave now, signalled after and the nudge lands on a parked read.
	if b.ctx.Err() != nil {
		return false
	}

	messageType, data, err := b.gatewayConn.ReadMessage()
	if err != nil {
		code, text := closeInfoForReadError(err)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			code, text = CloseTryAgainLater, "no relay request within the first-frame deadline"
			b.logger.Debug().
				Dur("deadline", b.firstFrameWait).
				Msg("no relay request within the first-frame deadline - closing connection")
		}
		_ = b.closeWithReason(code, text, closeInitiatorForSource(wsMessageSourceGateway))
		return false
	}

	// From here the loops own the deadlines.
	if err := b.gatewayConn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		b.logger.Debug().Err(err).Msg("failed to hand the read deadline to the loops")
	}

	b.handleGatewayMessage(wsMessage{data: data, source: wsMessageSourceGateway, messageType: messageType})

	// handleGatewayMessage closes on every rejection, so a live context is
	// exactly "the frame was admitted and the backend is up".
	return b.ctx.Err() == nil && b.backendConn != nil
}

// release is the ONLY code that gives anything back: the SessionMonitor
// registration, the active-connections gauge, the close frames, the sockets, and
// the wait for every loop.
//
// It runs from exactly one place -- Run's defer -- and that is the invariant the
// four guards it replaces were each approximating: a teardown reachable from
// several goroutines is a teardown that some path can skip. closeWithReason and
// Close only SIGNAL; they do not release.
//
// Being a defer also makes it panic-proof for free: the recover here is why a
// panic on the message loop can no longer walk past the teardown, which net/http
// will not do for a hijacked connection.
func (b *WebSocketBridge) release() {
	if r := recover(); r != nil {
		logging.PanicRecoveriesTotal.WithLabelValues(logging.ComponentWebsocketBridge).Inc()
		b.logger.Error().
			Str("panic_value", fmt.Sprintf("%v", r)).
			Str("stack_trace", string(debug.Stack())).
			Msg("PANIC RECOVERED on the websocket bridge")
		b.recordCloseReason(CloseInternalError, "internal error", wsCloseInitiatorRelayer)
	}

	b.cancelFn()

	reason := b.closeReason.Load()
	if reason == nil {
		reason = &wsCloseReason{code: CloseNormalClosure, text: "bridge closing", initiator: wsCloseInitiatorRelayer}
	}

	if b.sessionMonitor != nil {
		b.sessionMonitor.UnregisterBridge(b)
	}

	wsConnectionsActive.WithLabelValues(b.serviceID).Dec()
	wsClosesTotal.WithLabelValues(b.serviceID, closeCodeName(reason.code), string(reason.initiator)).Inc()

	b.logger.Debug().
		Int("close_code", reason.code).
		Str("close_code_name", closeCodeName(reason.code)).
		Str("close_reason", reason.text).
		Str("initiated_by", string(reason.initiator)).
		Uint64("relays_emitted", b.relayCount.Load()).
		Msg("websocket bridge closing")

	deadline := time.Now().Add(wsWriteWait)

	// The gateway keeps the Pocket code -- PATH understands those, and the
	// private band passes whole -- but it is still sanitized: a 1006 that
	// gorilla fabricated locally for a dead backend must not go out as a
	// reserved code, which PATH would read as a protocol violation by us.
	gatewayCode, gatewayText := sanitizeCloseCode(reason.code, reason.text)
	gatewayCloseMsg := websocket.FormatCloseMessage(gatewayCode, gatewayText)
	b.gatewayWriteMu.Lock()
	gwErr := b.gatewayConn.WriteControl(websocket.CloseMessage, gatewayCloseMsg, deadline)
	b.gatewayWriteMu.Unlock()
	if gwErr != nil {
		b.logger.Debug().Err(gwErr).Msg("failed to send close to client (PATH)")
	}

	// The backend gets an RFC-compliant code, and may not exist at all: a
	// connection refused before the first frame never dialled one.
	if b.backendConn != nil {
		backendCode, backendReason := mapToRFCCloseCode(reason.code)
		if backendReason == "" {
			backendReason = reason.text
		}
		backendCloseMsg := websocket.FormatCloseMessage(backendCode, backendReason)
		b.backendWriteMu.Lock()
		beErr := b.backendConn.WriteControl(websocket.CloseMessage, backendCloseMsg, deadline)
		b.backendWriteMu.Unlock()
		if beErr != nil {
			b.logger.Debug().Err(beErr).Msg("failed to send close to backend")
		}
	}

	// Give the peers time to receive the close frame before the socket goes.
	time.Sleep(wsCloseSettle)

	// Closing is what unblocks a readLoop parked in ReadMessage, which does not
	// observe a context -- so it MUST happen before the wait, never after.
	_ = b.gatewayConn.Close()
	if b.backendConn != nil {
		_ = b.backendConn.Close()
	}

	b.wg.Wait()

	b.logger.Debug().Msg("websocket bridge stopped")
}

// readLoop reads messages from a WebSocket connection.
func (b *WebSocketBridge) readLoop(conn *websocket.Conn, source wsMessageSource) {
	defer b.wg.Done()

	for {
		if b.ctx.Err() != nil {
			return
		}

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			// If we're shutting down, don't log expected errors from closing connections
			select {
			case <-b.ctx.Done():
				// Context cancelled - this is expected during bridge shutdown
				b.logger.Debug().
					Str(logging.FieldSource, string(source)).
					Msg("readLoop exiting due to shutdown")
				return
			default:
				// Parse and log close error details
				b.logCloseError(err, source)
			}
			closeCode, closeText := closeInfoForReadError(err)
			_ = b.closeWithReason(closeCode, closeText, closeInitiatorForSource(source))
			return
		}

		// Reset read deadline on each message (not just pongs)
		// This is critical for long-running subscriptions where the backend
		// sends frequent data messages but may not respond to pings
		if err := conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
			b.logger.Debug().Err(err).Str(logging.FieldSource, string(source)).Msg("failed to reset read deadline")
		}

		b.logger.Debug().
			Str(logging.FieldSource, string(source)).
			Int("message_size", len(data)).
			Msg("readLoop received message")

		select {
		case <-b.ctx.Done():
			return
		case b.msgChan <- wsMessage{data: data, source: source, messageType: messageType}:
		}
	}
}

// pingLoop sends periodic ping messages to keep the connection alive.
func (b *WebSocketBridge) pingLoop(conn *websocket.Conn, writeMu *sync.Mutex, source wsMessageSource) {
	name := string(source)
	defer b.wg.Done()

	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()

	// Set initial read deadline
	if err := conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		b.logger.Debug().Err(err).Str("connection", name).Msg("failed to set initial read deadline")
	}

	// Reset deadline on pong
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait))
			writeMu.Unlock()
			if err != nil {
				b.logger.Debug().
					Err(err).
					Str("connection", name).
					Msg("ping failed - connection may be dead")
				// The SAME mapping readLoop uses. It was a second switch here,
				// on free-text names passed by the call sites, with no
				// compile-time link to the source constants.
				_ = b.closeWithReason(CloseGoingAway, "ping timeout", closeInitiatorForSource(source))
				return
			}
		}
	}
}

// handleSessionExpiration is called by the global SessionMonitor when the session expires.
// This is invoked as a callback, not in a dedicated goroutine per bridge.
func (b *WebSocketBridge) handleSessionExpiration(sessionEndHeight, graceEndHeight, currentHeight int64) {
	// Verify this expiration applies to our session
	if b.sessionEndHeight != sessionEndHeight {
		b.logger.Debug().
			Int64("our_session_end", b.sessionEndHeight).
			Int64("expired_session_end", sessionEndHeight).
			Msg("session expiration for different session - ignoring")
		return
	}

	// Session expiry is normal protocol behaviour, once per connection —
	// at session rollover every open connection emits this at once.
	b.logger.Debug().
		Int64("current_height", currentHeight).
		Int64("session_end_height", sessionEndHeight).
		Int64("grace_end_height", graceEndHeight).
		Int64("blocks_over", currentHeight-graceEndHeight).
		Msg("session expired - closing websocket connection")

	// Send session expiration message to client
	if err := b.sendSessionExpirationMessage(); err != nil {
		b.logger.Debug().Err(err).Msg("failed to send session expiration message")
	}

	// Close the connection with session expired code
	_ = b.closeWithReason(CloseSessionExpired, "session expired", wsCloseInitiatorRelayer)
}

// messageLoop processes messages from both connections.
func (b *WebSocketBridge) messageLoop() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case msg := <-b.msgChan:
			switch msg.source {
			case wsMessageSourceGateway:
				b.handleGatewayMessage(msg)
			case wsMessageSourceBackend:
				b.handleBackendMessage(msg)
			}
		}
	}
}

// writeDataFrame sets a write deadline and then writes a data frame to conn,
// serialised on writeMu.
//
// gorilla's WriteMessage does not observe a context and blocks indefinitely once
// the peer stops reading and its TCP receive buffer fills. Every data frame is
// written from the single messageLoop goroutine and serialised on writeMu, so
// one stalled peer would wedge the entire bridge: messageLoop can never reach
// its ctx.Done() select while blocked in a write, which means pingLoop
// cancelling the context cannot free it, and the connection holds its resources
// until the OS-level TCP timeout. The deadline bounds that wait so a stalled
// reader fails fast and the bridge tears down through its normal path.
//
// The deadline is set under writeMu because SetWriteDeadline and WriteMessage
// must apply as a pair -- another writer setting its own deadline in between
// would write under the wrong one. Control writes (ping/close) pass their own
// deadline to WriteControl and do not need this.
//
// writeWait is a parameter rather than a direct read of wsWriteWait so a test
// can drive the stalled-peer path in milliseconds instead of ten seconds.
func writeDataFrame(conn *websocket.Conn, writeMu *sync.Mutex, messageType int, data []byte, writeWait time.Duration) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
		return err
	}
	return conn.WriteMessage(messageType, data)
}

// writeToGateway writes a data frame to the gateway (PATH) connection.
func (b *WebSocketBridge) writeToGateway(messageType int, data []byte) error {
	return writeDataFrame(b.gatewayConn, &b.gatewayWriteMu, messageType, data, wsWriteWait)
}

// errBackendNotConnected is returned when a frame is written before any frame
// has earned the backend dial. It is a normal state, not a fault: the bridge
// exists from the WebSocket upgrade, the backend from the first admitted relay.
var errBackendNotConnected = errors.New("backend not connected yet")

// writeToBackend writes a data frame to the backend connection.
func (b *WebSocketBridge) writeToBackend(messageType int, data []byte) error {
	if b.backendConn == nil {
		return errBackendNotConnected
	}
	return writeDataFrame(b.backendConn, &b.backendWriteMu, messageType, data, wsWriteWait)
}

// ownerAddress returns the supplier this bridge belongs to, or "" while no
// frame has established one yet (v1/sage before the first RelayRequest).
//
// Every accounting and signing site reads THIS and not the address inside the
// frame it is handling. Preferring the frame's is exploitable: the backend
// headers and the response signer are bound to the owner, so a later frame
// naming a different supplier would be mined against that other supplier while
// the connection kept serving under the first one's identity.
func (b *WebSocketBridge) ownerAddress() string {
	if owner := b.owner.Load(); owner != nil {
		return *owner
	}
	return ""
}

// adoptOrVerifyOwner enforces the rule that a bridge has exactly one supplier.
//
// The first frame that names a supplier adopts it as the bridge's owner; every
// frame after that must name the same one. A frame naming a different supplier,
// or none at all, closes the connection -- the same convention every other
// admission failure on this bridge already follows, because a close code is the
// only thing a WebSocket client can tell apart from backend traffic.
//
// It returns false when it has closed the connection, and the caller returns.
func (b *WebSocketBridge) adoptOrVerifyOwner(relayReq *servicetypes.RelayRequest) bool {
	reqSupplier := relayReq.Meta.SupplierOperatorAddress

	if reqSupplier == "" {
		relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonMissingSupplierAddress).Inc()
		b.logger.Debug().
			Msg("relay request names no supplier operator address - closing connection")
		_ = b.closeWithReason(CloseValidationFailed, "relay request names no supplier", wsCloseInitiatorRelayer)
		return false
	}

	// A nil SessionHeader is refused HERE because the next thing this frame
	// reaches dereferences it unguarded (the RelayContext's SessionID), and a
	// remote peer chooses the frame's shape. This gate is the one place that
	// already asks whether the frame has the shape the rest of the path assumes.
	if relayReq.Meta.SessionHeader == nil {
		relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonInvalidRelayRequest).Inc()
		b.logger.Debug().
			Msg("relay request carries no session header - closing connection")
		_ = b.closeWithReason(CloseValidationFailed, "relay request carries no session header", wsCloseInitiatorRelayer)
		return false
	}

	owner := b.owner.Load()
	if owner == nil {
		// v1/sage: the handshake carried no supplier, so this frame establishes
		// the owner. Admission still runs below and closes the connection if it
		// fails, so an owner adopted here never outlives a rejected frame.
		b.owner.Store(&reqSupplier)
		return true
	}

	if *owner != reqSupplier {
		relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonSupplierChanged).Inc()
		b.logger.Debug().
			Str("owner", *owner).
			Str("requested", reqSupplier).
			Msg("relay request names a different supplier than this connection - closing connection")
		_ = b.closeWithReason(CloseValidationFailed, "supplier does not own this connection", wsCloseInitiatorRelayer)
		return false
	}

	return true
}

// handleGatewayMessage handles messages from the gateway.
func (b *WebSocketBridge) handleGatewayMessage(msg wsMessage) {
	wsMessagesForwarded.WithLabelValues(b.serviceID, "gateway_to_backend").Inc()

	// Try to parse as RelayRequest
	relayReq := &servicetypes.RelayRequest{}
	if err := relayReq.Unmarshal(msg.data); err != nil {
		// Not a valid RelayRequest - forward raw data to backend
		b.forwardToBackend(msg)
		return
	}

	// The owner gate runs before ANYTHING else this frame could reach: before
	// the session height is pinned and before the bridge is registered with the
	// global SessionMonitor, both of which are state a frame that does not own
	// this connection must not be able to set.
	if !b.adoptOrVerifyOwner(relayReq) {
		return
	}

	// Extract session parameters from first request and register with global monitor
	if b.sessionEndHeight == 0 && relayReq.Meta.SessionHeader != nil {
		b.sessionEndHeight = relayReq.Meta.SessionHeader.SessionEndBlockHeight
		b.logger.Debug().
			Int64("session_end_height", b.sessionEndHeight).
			Msg("session parameters initialized from first relay request")

		// Register with global session monitor
		if b.sessionMonitor != nil {
			b.sessionMonitor.RegisterBridge(b, b.sessionEndHeight)
		}
	}

	if b.simulated {
		// SIMULATION ADMISSION — replaces ValidateRelay/MeterRelay entirely for
		// a simulated message; a simulated relay must NEVER consume stake.
		// Mirrors the HTTP/gRPC Admission zone: global slot -> pinned-ring/
		// binding/freshness Verify -> per-key rate. On rejection the bridge
		// closes the connection, the existing convention on this bridge for a
		// gateway message that fails admission/validation.
		supplier := b.ownerAddress()
		recordSim := func(result string) {
			simulatedRelaysTotal.WithLabelValues("websocket", b.serviceID, supplier, result).Inc()
		}

		release, ok := b.simVerifier.AcquireGlobal()
		if !ok {
			recordSim(SimResultRateLimited)
			b.logger.Debug().Msg("simulation concurrency limit reached - closing connection")
			_ = b.closeWithReason(CloseTryAgainLater, "simulation concurrency limit reached", wsCloseInitiatorRelayer)
			return
		}
		defer release()

		if err := b.simVerifier.Verify(b.ctx, b.simKeyID, relayReq); err != nil {
			recordSim(SimResultForError(err))
			b.logger.Debug().Err(err).Msg("simulated relay admission failed - closing connection")
			_ = b.closeWithReason(CloseValidationFailed, "simulation rejected", wsCloseInitiatorRelayer)
			return
		}

		// R2 — per-key rate cap charged only AFTER a request verifies (so a
		// public key_id cannot be used pre-auth to starve a legit connection).
		if !b.simVerifier.AllowKey(b.simKeyID) {
			recordSim(SimResultRateLimited)
			b.logger.Debug().Msg("simulation rate limit reached - closing connection")
			_ = b.closeWithReason(CloseTryAgainLater, "simulation rate limit reached", wsCloseInitiatorRelayer)
			return
		}
	} else if b.relayPipeline != nil {
		// Validate and meter the relay if pipeline is available
		// Build relay context for validation/metering
		relayCtx := &RelayContext{
			Request:            relayReq,
			ServiceID:          b.serviceID,
			SupplierAddress:    b.ownerAddress(),
			SessionID:          relayReq.Meta.SessionHeader.SessionId,
			ArrivalBlockHeight: b.arrivalHeight,
		}

		// Validate relay request (ring signature + session)
		if err := b.relayPipeline.ValidateRelay(b.ctx, relayCtx); err != nil {
			relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonValidationFailed).Inc()
			b.logger.Debug().
				Err(err).
				Str("session_id", relayCtx.SessionID).
				Msg("relay validation failed - closing connection")
			_ = b.closeWithReason(CloseValidationFailed, "relay validation failed", wsCloseInitiatorRelayer)
			return
		}

		// Meter relay (check stake before serving)
		allowed, meterErr := b.relayPipeline.MeterRelay(b.ctx, relayCtx)
		if meterErr != nil && allowed {
			// The meter could not answer, but not because OUR store was
			// unreadable -- a chain query it depends on blinked. Served and
			// passed on: the miner re-derives what it needs and retries.
			relayMeterUnbilled.WithLabelValues(b.serviceID).Inc()
			b.logger.Debug().
				Err(meterErr).
				Str("session_id", relayCtx.SessionID).
				Msg("relay served unmetered; the miner arbitrates")
		} else if meterErr != nil {
			// The meter's own store is unreadable, so what this session has
			// already consumed is unknown. This is admission, and admission
			// refuses.
			//
			// The connection is CLOSED rather than the message refused, and the
			// cost of that is understood: every subscription on this socket
			// goes, and reconnecting a WebSocket is not free. A per-message
			// refusal was tried and dropped -- it would have to arrive as a
			// payload on a stream the client is reading as backend traffic, and
			// nothing in the shape of a WebSocket message lets the client tell
			// "the relayminer refused this" from "the backend said this". A
			// close code says exactly one thing, and PATH and SAGE both already
			// handle it, which is why every other refusal on this path closes
			// too.
			//
			// Nothing is emitted: emitRelay runs on the BACKEND-response path,
			// and this request never reached the backend.
			relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonMeterError).Inc()
			b.logger.Debug().
				Err(meterErr).
				Str("session_id", relayCtx.SessionID).
				Msg("relay rejected - unable to verify session budget, closing connection")
			_ = b.closeWithReason(CloseTryAgainLater, "unable to process relay request", wsCloseInitiatorRelayer)
			return
		} else if !allowed {
			// Stake limit exceeded - reject relay
			relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonStakeExhausted).Inc()
			b.logger.Debug().
				Str("session_id", relayCtx.SessionID).
				Msg("relay rejected - stake limit exceeded")
			_ = b.closeWithReason(CloseStakeLimitExceeded, "stake limit exceeded", wsCloseInitiatorRelayer)
			return
		}
	}

	// PAST THIS LINE THE FRAME HAS PASSED ADMISSION, and only now does the
	// operator's backend get dialled. Jorge, 2026-09-03: "en son de proteger el
	// recurso valioso (backend, blockchain) no hacemos el handshake al backend
	// hasta no tener validacion del supplier address, evitamos un ddos a sus
	// backends sin relays."
	//
	// Reachable with backendConn nil only from awaitFirstFrame, on the Run
	// goroutine, before any loop exists -- see ensureBackend.
	if err := b.ensureBackend(); err != nil {
		relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonBackendDialFailed).Inc()
		b.logger.Debug().Err(err).Msg("backend dial failed after admission - closing connection")
		_ = b.closeWithReason(CloseTryAgainLater, "backend unavailable", wsCloseInitiatorBackend)
		return
	}

	// Clear any previous request when new one arrives
	// This ensures each incoming request becomes the new latestRequest
	b.clearLatestRequest()

	// Store latest request for pairing with subsequent backend responses
	b.setLatestRequest(relayReq)

	b.logger.Debug().
		Int("payload_size", len(relayReq.Payload)).
		Msg("forwarding payload to backend")

	// Forward payload to backend echoing the frame type the client sent.
	// No relay protobuf carries a frame type, so the WebSocket envelope is the
	// only channel for it: hardcoding a type here destroys information nothing
	// downstream can reconstruct. Text-only JSON-RPC backends reject binary
	// frames. Matches poktroll, PATH, and the raw forwarding paths below.
	err := b.writeToBackend(msg.messageType, relayReq.Payload)
	if err != nil {
		b.logger.Debug().Err(err).Msg("failed to forward to backend")
		_ = b.closeWithReason(CloseInternalError, "backend write failed", wsCloseInitiatorRelayer)
		return
	}

	b.logger.Debug().
		Int("bytes_sent", len(relayReq.Payload)).
		Msg("successfully forwarded payload to backend")
}

// handleBackendMessage handles messages from the backend.
// Each backend message is billed as part of a relay.
func (b *WebSocketBridge) handleBackendMessage(msg wsMessage) {
	wsMessagesForwarded.WithLabelValues(b.serviceID, "backend_to_gateway").Inc()

	b.logger.Debug().
		Int("message_size", len(msg.data)).
		Str("backend_response", string(msg.data[:min(50, len(msg.data))])).
		Msg("handleBackendMessage called")

	latestReq := b.getLatestRequest()
	if latestReq == nil {
		b.logger.Debug().Msg("no latestRequest - forwarding raw to gateway")
		// No request yet - just forward raw data
		err := b.writeToGateway(msg.messageType, msg.data)
		if err != nil {
			b.logger.Debug().Err(err).Msg("failed to forward to client (PATH)")
			_ = b.closeWithReason(CloseInternalError, "client write failed", wsCloseInitiatorRelayer)
		}
		return
	}

	b.logger.Debug().Msg("latestRequest found - building signed response")

	// Build and sign RelayResponse
	// responseSigner is guaranteed to be non-nil (validated early in WebSocketHandler)
	var respBytes []byte
	var relayResp *servicetypes.RelayResponse

	// Build signed WebSocket response (raw payload, no HTTP wrapping)
	var signErr error
	relayResp, respBytes, signErr = b.responseSigner.BuildAndSignWebSocketRelayResponse(
		latestReq,
		msg.data, // Raw WebSocket payload (e.g., JSON-RPC response)
	)
	if signErr != nil {
		// An unsigned response is not a cheaper response, it is a different
		// thing: emitRelay below mines the RelayHash over {Req, Res}, so serving
		// one does not merely hand the gateway something it cannot verify -- it
		// commits an SMST leaf to a response no supplier ever signed. Nothing is
		// written and nothing is billed.
		//
		// Closing is the same answer every other refusal on this path already
		// gives (see the meter error above), and it is the only one that ends
		// the failure: admission already refused any request whose supplier we
		// hold no key for, so reaching here means the key set changed mid
		// connection -- a hot removal. The supplier is fixed for the life of
		// this bridge, so the next backend push would fail identically; skipping
		// the message instead would leave a subscription pushing into a bin.
		relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonSigningError).Inc()
		// Warn, not Debug: this fires at most once per CONNECTION rather than
		// once per message, and its causes are ours -- a missing signer, an
		// empty supplier address, a malformed header -- never client traffic.
		b.logger.Warn().
			Err(signErr).
			Str("session_id", latestReq.Meta.GetSessionHeader().GetSessionId()).
			Str("supplier", latestReq.Meta.SupplierOperatorAddress).
			Msg("cannot sign the websocket response: closing the connection, nothing served and nothing billed")
		_ = b.closeWithReason(CloseInternalError, "unable to sign response", wsCloseInitiatorRelayer)
		return
	}

	// Store latest response for relay emission
	b.setLatestResponse(relayResp)

	// Forward signed RelayResponse to gateway echoing the frame type the backend
	// sent. Backend frames cannot be paired with client frames (one eth_subscribe
	// yields N pushes), so each hop echoes its own inbound type instead. A browser
	// doing JSON.parse(e.data) needs text to survive the round trip.
	writeErr := b.writeToGateway(msg.messageType, respBytes)
	if writeErr != nil {
		b.logger.Debug().Err(writeErr).Msg("failed to forward signed response to client (PATH)")
		_ = b.closeWithReason(CloseInternalError, "client write failed", wsCloseInitiatorRelayer)
		return
	}

	b.logger.Debug().Msg("forwarded signed response to gateway successfully")

	// Emit relay for this request/response pair
	b.emitRelay(latestReq, relayResp, msg.data)

	b.logger.Debug().Msg("handleBackendMessage completed - latestRequest preserved for next message")

	// NOTE: latestRequest is NOT cleared here - it's reused for subsequent backend messages
	// This allows subscription-based APIs (eth_subscribe) to bill for each update
	// The request will be cleared when the client sends a new request (in handleGatewayMessage)
}

// forwardToBackend forwards a raw message to the backend.
//
// A raw frame -- one that does not parse as a RelayRequest -- is only
// forwardable once a relay has established this connection and dialled the
// backend. Before that there is nothing to forward to, and that is the point: a
// raw frame used to reach the operator's backend on a connection that had never
// carried a single relay.
func (b *WebSocketBridge) forwardToBackend(msg wsMessage) {
	err := b.writeToBackend(msg.messageType, msg.data)
	if errors.Is(err, errBackendNotConnected) {
		relaysRejected.WithLabelValues(b.serviceID, "websocket", rejectReasonNoRelayYet).Inc()
		b.logger.Debug().Msg("raw frame before any relay established this connection - closing connection")
		_ = b.closeWithReason(CloseValidationFailed, "no relay request has established this connection", wsCloseInitiatorRelayer)
		return
	}
	if err != nil {
		b.logger.Debug().Err(err).Msg("failed to forward raw message to backend")
		_ = b.closeWithReason(CloseInternalError, "backend write failed", wsCloseInitiatorRelayer)
	}
}

// emitRelay creates and publishes a mined relay for a request/response pair.
// This is the billing mechanism - each req/resp pair becomes a relay.
// wsPublishTimeout bounds the detached mining/WAL-publish work for one
// websocket relay. It is deliberately NOT grpcPublishTimeout (30s): there the
// budget is per request, here it is per open connection, and at session
// rollover every open bridge publishes at once -- 30s each would hold N sockets
// and their goroutines through the whole teardown. Small enough that a slow
// Redis delays teardown rather than stalling it, large enough that a single
// XAdd on a healthy localnet or production Redis has room to spare.
const wsPublishTimeout = 5 * time.Second

func (b *WebSocketBridge) emitRelay(req *servicetypes.RelayRequest, resp *servicetypes.RelayResponse, respPayload []byte) {
	if b.simulated {
		// ACCOUNTING gated off entirely for a simulated relay: no mining
		// (relayProcessor.ProcessRelay), no publish -- ever, regardless of
		// whether a publisher happens to be wired on this bridge. Production
		// always passes a nil publisher at handshake construction (see
		// WebSocketHandler) as an additional belt-and-suspenders guard, but
		// this check does not rely on that: it is what actually prevents the
		// WAL publish.
		simulatedRelaysTotal.WithLabelValues("websocket", b.serviceID, b.ownerAddress(), SimResultSuccess).Inc()
		return
	}

	// SERVED, and this is the only honest place to say so. handleBackendMessage
	// writes the signed response to the gateway and calls this on the next line
	// (websocket.go:1173), returning early if that write failed -- so reaching
	// here means the client HAS the response. Counting later, after the publish,
	// would put a relay that was served and failed to publish outside BOTH sides
	// of the drop rate the metric exists to feed: relaysDropped would rise while
	// its own denominator never counted the relay.
	//
	// AFTER the simulated guard above and BEFORE everything below it. Both
	// halves are load-bearing: docs/simulated-relays.md is explicit that a
	// simulated relay "never touches the counters that measure real traffic",
	// which HTTP gets structurally (proxy.go:903 diverts to another function)
	// and gRPC gets by placement -- so WebSocket has to get it here. And it
	// stays ahead of the difficulty filter and of the nil-publisher return,
	// because relays_served_total is everything SERVED, not everything mined
	// and not everything published.
	relaysServed.WithLabelValues(b.serviceID, BackendTypeWebSocket, statusCodeNoHTTP).Inc()

	if b.publisher == nil {
		return
	}

	// Increment relay count for this connection
	count := b.relayCount.Add(1)

	// The bridge's owner, never the address inside req: adoptOrVerifyOwner has
	// already proved they are equal for every frame that gets this far, and
	// reading the owner is what keeps mining, metering and signing on one
	// identity if that gate is ever weakened.
	supplierAddr := b.ownerAddress()

	// Extract session context for logging
	sessionCtx := logging.SessionContextFromRelayRequest(req)
	if sessionCtx.Supplier == "" {
		sessionCtx.Supplier = supplierAddr
	}
	if sessionCtx.ServiceID == "" {
		sessionCtx.ServiceID = b.serviceID
	}

	if supplierAddr == "" {
		relaysDropped.WithLabelValues(b.serviceID, dropReasonNoSupplier).Inc()
		logging.WithSessionContext(b.logger.Debug(), sessionCtx).
			Msg("no supplier address available for websocket relay")
		return
	}

	// Marshal the original request body for relay processing
	reqBytes, err := req.Marshal()
	if err != nil {
		relaysDropped.WithLabelValues(b.serviceID, dropReasonMarshalFailed).Inc()
		logging.WithSessionContext(b.logger.Debug(), sessionCtx).
			Err(err).
			Msg("failed to marshal relay request")
		return
	}

	// NewWebSocketBridge enforces b.relayProcessor != nil, so every event goes
	// through the full ProcessRelay path: compute RelayHash over {Req, Res},
	// attach correct CU from service config, and publish with a session ID.
	// publishCtx detaches mining and the WAL publish from the bridge LIFETIME.
	// Everything above this line already happened: handleBackendMessage signed
	// the response and wrote it to the gateway before calling emitRelay, so the
	// relay is served and billable by the gateway no matter what happens next.
	// b.ctx is cancelled by closeWithReason, which readLoop, pingLoop and the
	// SessionMonitor callback all reach from goroutines that are NOT serialised
	// with messageLoop -- so a cancel landing in the window between the gateway
	// write and this call used to make XAdd return context.Canceled before any
	// network I/O, leaving a served, signed relay with no WAL entry and no SMST
	// leaf: no claim, no proof, lost reward. Session rollover fires that cancel
	// on every open connection at once.
	//
	// It covers ProcessRelay too, not just Publish: ProcessRelay reads the ctx
	// to fetch compute units, and CU is what the leaf is worth -- degrading it
	// under a dead context would change the money without changing the relay
	// count, which is the one thing anything downstream compares.
	//
	// The timeout is what carries this, not WithoutCancel: b.ctx descends from
	// context.Background (see the constructor), so WithoutCancel drops a
	// cancellation and nothing else today. It is written this way for symmetry
	// with the gRPC path and so the call keeps any values b.ctx gains later.
	publishCtx, publishCancel := context.WithTimeout(context.WithoutCancel(b.ctx), wsPublishTimeout)
	defer publishCancel()

	msg, procErr := b.relayProcessor.ProcessRelay(
		publishCtx,
		reqBytes,
		respPayload,
		supplierAddr,
		b.serviceID,
		b.arrivalHeight,
	)
	if procErr != nil {
		relaysDropped.WithLabelValues(b.serviceID, dropReasonProcessFailed).Inc()
		logging.WithSessionContext(b.logger.Debug(), sessionCtx).
			Err(procErr).
			Msg("failed to process websocket relay")
		return
	}

	if msg == nil {
		// ProcessRelay returns (nil, nil) when the relay does not meet the
		// service's mining difficulty. That is an expected outcome, not a bug.
		return
	}

	if pubErr := b.publisher.Publish(publishCtx, msg); pubErr != nil {
		relaysDropped.WithLabelValues(b.serviceID, dropReasonPublishFailed).Inc()
		logging.WithSessionContext(b.logger.Debug(), sessionCtx).
			Err(pubErr).
			Msg("failed to publish websocket relay")
		return
	}
	wsRelaysEmitted.WithLabelValues(b.serviceID).Inc()
	logging.WithSessionContext(b.logger.Debug(), sessionCtx).
		Uint64("relay_count", count).
		Msg("websocket relay published")
}

// sendSessionExpirationMessage sends a signed error response to the client
// informing them that the session has expired.
func (b *WebSocketBridge) sendSessionExpirationMessage() error {
	// Get the latest request to extract session header
	b.latestMu.RLock()
	latestReq := b.latestRequest
	b.latestMu.RUnlock()

	if latestReq == nil || latestReq.Meta.SessionHeader == nil {
		return nil // No session to expire
	}

	supplierAddr := b.ownerAddress()

	// Build error response
	_, respBytes, err := b.responseSigner.BuildErrorRelayResponse(
		latestReq.Meta.SessionHeader,
		supplierAddr,
		410, // HTTP 410 Gone
		"session expired",
	)
	if err != nil {
		return err
	}

	// Sent as BinaryMessage rather than an echoed type: this frame is initiated
	// by the relayer on session expiry, not in response to an inbound frame, so
	// there is no type to echo. See writeToGateway for the deadline rationale.
	err = b.writeToGateway(websocket.BinaryMessage, respBytes)
	if err != nil {
		return err
	}

	b.logger.Debug().
		Str("session_id", latestReq.Meta.SessionHeader.SessionId).
		Msg("sent session expiration message to client")

	return nil
}

// Unused - deprecated method, use emitRelay directly
// tryEmitRelay is deprecated - use emitRelay directly.
// Kept for compatibility but does nothing.
// func (b *WebSocketBridge) tryEmitRelay() {
// 	// No-op: relay emission is now done in handleBackendMessage
// }

// setLatestRequest stores the latest request.
func (b *WebSocketBridge) setLatestRequest(req *servicetypes.RelayRequest) {
	b.latestMu.Lock()
	defer b.latestMu.Unlock()
	b.latestRequest = req
}

// getLatestRequest retrieves the latest request.
func (b *WebSocketBridge) getLatestRequest() *servicetypes.RelayRequest {
	b.latestMu.RLock()
	defer b.latestMu.RUnlock()
	return b.latestRequest
}

// setLatestResponse stores the latest response.
func (b *WebSocketBridge) setLatestResponse(resp *servicetypes.RelayResponse) {
	b.latestMu.Lock()
	defer b.latestMu.Unlock()
	b.latestResponse = resp
}

// Unused - reserved for future use
// getLatestResponse retrieves the latest response.
// func (b *WebSocketBridge) getLatestResponse() *servicetypes.RelayResponse {
// 	b.latestMu.RLock()
// 	defer b.latestMu.RUnlock()
// 	return b.latestResponse
// }

// clearLatestRequest clears the latest request after emitting a relay.
// This ensures each backend response is paired with a unique gateway request.
func (b *WebSocketBridge) clearLatestRequest() {
	b.latestMu.Lock()
	defer b.latestMu.Unlock()
	b.latestRequest = nil
}

// logCloseError parses and logs WebSocket close errors with proper RFC 6455 codes.
func (b *WebSocketBridge) logCloseError(err error, source wsMessageSource) {
	// Try to extract close code from error
	closeErr, ok := err.(*websocket.CloseError)
	if ok {
		// Log with structured close code info
		b.logger.Debug().
			Int("close_code", closeErr.Code).
			Str("close_code_name", closeCodeName(closeErr.Code)).
			Str("close_text", closeErr.Text).
			Str("initiated_by", string(source)).
			Msg("websocket connection closed by peer")
		return
	}

	// Not a close error - log as unexpected error
	b.logger.Debug().
		Err(err).
		Str("initiated_by", string(source)).
		Msg("websocket read error (not a close frame)")
}

// extractCloseInfo extracts close code and text from a WebSocket close error.
// Returns 0 and empty string if the error is not a close error.
// This is used to propagate close codes bidirectionally through the bridge.
// closeInfoForReadError decides the close code and reason the bridge reports when
// a readLoop read fails.
//
// A peer-sent close frame wins: propagating its code is what carries session
// rollover signalling (e.g. 4000 SessionExpired from PATH) through to the backend.
//
// websocket.ErrReadLimit is called out explicitly because it is NOT a close frame,
// so it would otherwise fall through to the generic "peer disconnected" default and
// report a wsMaxMessageBytes breach as 1001. gorilla does write its own 1009 from
// advanceFrame, but only as a documented BEST EFFORT — it discards the WriteControl
// error, and that write races an oversized frame still in flight from the peer, so
// it cannot be relied on to arrive. The peer must learn it breached a policy it has
// to fix, not that the far side went away and it should reconnect.
func closeInfoForReadError(err error) (code int, text string) {
	if code, text := extractCloseInfo(err); code != 0 {
		return code, text
	}
	if errors.Is(err, websocket.ErrReadLimit) {
		return CloseMessageTooBig, "message too big"
	}
	return CloseGoingAway, "peer disconnected"
}

func extractCloseInfo(err error) (int, string) {
	closeErr, ok := err.(*websocket.CloseError)
	if ok {
		return closeErr.Code, closeErr.Text
	}
	return 0, ""
}

// mapToRFCCloseCode converts custom Pocket close codes to standard RFC 6455 codes.
// This is used when closing the backend connection - backends don't understand Pocket codes.
// Pocket codes (4000-4999) are mapped to appropriate RFC codes for clean disconnection.
// sanitizeCloseCode returns a close code a real gorilla peer will accept, and
// the text to send beside it.
//
// The leak it plugs: gorilla manufactures CloseError{1006} LOCALLY for any dead
// TCP with no close frame -- an ordinary disconnect, not something a peer sent.
// closeInfoForReadError propagates whatever it finds, and release() used to put
// that straight on the wire in both directions. 1006 is reserved and must not
// be sent, so the receiving gorilla answers a protocol error: a backend that
// dies made PATH see a protocol violation by the RELAYER, which PATH charges to
// this endpoint's reputation.
//
// The accepted set is gorilla's own validReceivedCloseCodes, enumerated here
// because it is unexported. It is enumerated and NOT written as a range: the
// range 1000..1014 looks right and admits 1004 and 1014, which gorilla rejects.
// TestEveryCloseCodeTheBridgeCanPickIsAcceptedByARealPeer checks this against a
// real peer rather than against a predicate of ours, which would be circular.
func sanitizeCloseCode(code int, text string) (int, string) {
	// The private band passes WHOLE, never enumerated: gorilla accepts all of
	// 3000-4999, and the codes in it that matter belong to the END CLIENT, who
	// we cannot enumerate. Listing the Pocket codes and defaulting the rest is
	// how a client's own close code gets flattened on its way through us.
	if code >= 3000 && code <= 4999 {
		return code, text
	}
	switch code {
	case CloseNormalClosure, CloseGoingAway, CloseProtocolError, CloseUnsupportedData,
		CloseInvalidPayload, ClosePolicyViolation, CloseMessageTooBig,
		CloseMandatoryExtension, CloseInternalError, CloseServiceRestart,
		CloseTryAgainLater:
		return code, text
	}
	// 1004, 1005, 1006, 1014, 1015 and anything out of range: no endpoint may
	// put these on the wire. Going away is the truthful residue -- we are
	// closing, and we have nothing valid to say about why.
	return CloseGoingAway, text
}

func mapToRFCCloseCode(code int) (int, string) {
	// No pass-through for 1000..1015: that range is where the leak was, since
	// it admits the reserved codes gorilla refuses to receive. Anything in the
	// RFC space goes through the sanitizer, which enumerates what a peer will
	// actually take.
	if code < 3000 {
		out, _ := sanitizeCloseCode(code, "")
		return out, ""
	}

	// Pocket codes have no meaning to a backend, so they are translated. What a
	// code says about WHOSE fault it was matters here: 4001 and 4002 are
	// verdicts about the CLIENT, and the backend did nothing. Sending it 1008
	// ("you violated a policy") or 1013 ("you are overloaded") accuses it of a
	// fault it does not have, and an operator reading its logs sees us blaming
	// their node for someone else's bad frame.
	switch code {
	case CloseSessionExpired: // 4000 - session ended, clean shutdown
		return CloseGoingAway, "session ended"
	case CloseValidationFailed: // 4001 - the CLIENT's frame was rejected
		return CloseGoingAway, "client frame rejected"
	case CloseStakeLimitExceeded: // 4002 - the APPLICATION ran out of stake
		return CloseGoingAway, "client budget exhausted"
	case CloseBackendConnectionFailed: // 4003 - this one IS about the backend
		return CloseInternalError, "connection failed"
	default:
		// Unknown custom code - use generic going away
		return CloseGoingAway, "connection closing"
	}
}

// wsCloseReason is the verdict recorded by whichever goroutine decided the
// bridge is finished. It is DATA, not control flow: release reads it once to
// build the close frames.
type wsCloseReason struct {
	code      int
	text      string
	initiator wsCloseInitiator
}

// recordCloseReason keeps the FIRST verdict. Later ones are noise from goroutines
// noticing the same shutdown.
func (b *WebSocketBridge) recordCloseReason(code int, text string, initiator wsCloseInitiator) {
	b.closeReason.CompareAndSwap(nil, &wsCloseReason{code: code, text: text, initiator: initiator})
}

// closeWithReason SIGNALS that the bridge is finished. It does not release
// anything -- release does, from Run's defer, and only from there.
//
// The split is the point. This used to do the whole teardown and was reachable
// from six goroutines, so every path that reached it early, late, or not at all
// was a defect waiting to be found one at a time. Now the worst a caller can do
// is signal twice.
//
// It expires the gateway's READ deadline rather than closing the socket, and the
// difference matters twice: a synchronous ReadMessage in awaitFirstFrame does not
// observe the context and would otherwise sit there until the first-frame
// deadline, and release still has to WRITE close frames afterwards, which a
// closed socket would not accept.
func (b *WebSocketBridge) closeWithReason(code int, reason string, initiator wsCloseInitiator) error {
	b.recordCloseReason(code, reason, initiator)
	b.cancelFn()
	_ = b.gatewayConn.SetReadDeadline(time.Now()) //nolint:errcheck // expiring the read deadline is a nudge so a blocked ReadMessage returns; if it cannot be set the socket is already unusable and that read fails on its own
	return nil
}

// Close asks the bridge to shut down. Like every other caller it only SIGNALS:
// Run's deferred release does the work, so a bridge that was never Run is not
// released by this either -- not its sockets, not its close frames.
//
// That state DOES exist in production, in exactly one place: the WebSocket
// handler builds a bridge and then refuses it when the proxy is already
// draining. It is safe there only because ownership of the gateway connection
// is transferred AFTER that refusal, so the handler's own defer still closes
// it. This paragraph used to say the state could not arise; the shutdown drain
// created it, and the refusal leaked a connection until the transfer moved.
func (b *WebSocketBridge) Close() error {
	return b.closeWithReason(CloseNormalClosure, "bridge closing", wsCloseInitiatorRelayer)
}

// WebSocketHandler returns an HTTP handler for WebSocket upgrades.
// This should be used when detecting WebSocket upgrade requests.
func (p *ProxyServer) WebSocketHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serviceID := p.extractServiceID(r)
		if serviceID == "" {
			p.sendError(w, http.StatusBadRequest, "missing service ID")
			return
		}

		svcConfig, ok := p.config.Services[serviceID]
		if !ok {
			p.sendError(w, http.StatusNotFound, "unknown service")
			return
		}

		// SIMULATION SEAM — the only simulation-aware line on the WebSocket
		// handshake path. Whether this CONNECTION is simulated is decided once,
		// here, from the handshake headers (mirrors SimDirectiveFromHTTP/GRPC).
		// Every subsequent message on this connection is admitted through
		// simVerifier inside handleGatewayMessage instead of
		// relayPipeline.ValidateRelay/MeterRelay. When simulation is disabled or
		// the header is absent, simulated is false and behavior is unchanged
		// (R7): the header is ignored and the bridge runs the normal path.
		simDirective := SimDirectiveFromWS(r.Header)
		simulated := simDirective.KeyID != "" && p.simVerifier != nil && p.simVerifier.Enabled()

		// Validate critical dependencies are configured - fail fast before upgrade
		if p.responseSigner == nil {
			p.logger.Error().Str(logging.FieldServiceID, serviceID).Msg("response signer not configured")
			p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
			return
		}

		if p.relayProcessor == nil {
			p.logger.Error().Str(logging.FieldServiceID, serviceID).Msg("relay processor not configured")
			p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
			return
		}

		if p.publisher == nil {
			p.logger.Error().Str(logging.FieldServiceID, serviceID).Msg("publisher not configured")
			p.sendError(w, http.StatusInternalServerError, "relayer not properly configured")
			return
		}

		// Fast-fail pre-check: reject WebSocket upgrade before Accept() when all backends unhealthy.
		// CRITICAL: This must happen BEFORE Upgrade() to return HTTP 503 (not a WS close frame).
		wsPool := p.config.GetPool(serviceID, "websocket")
		if wsPool == nil || !wsPool.HasHealthy() {
			p.sendServiceUnavailable(w, serviceID)
			fastFailsTotal.WithLabelValues(serviceID).Inc()
			p.logger.Debug().Str("service_id", serviceID).Msg("fast-fail: all WebSocket backends unhealthy")
			return
		}

		// The v2 half of the owner rule: a handshake that NAMES a supplier is
		// checked against the live key set before anything is upgraded or
		// dialled. Jorge, 2026-09-03: "la primera vez lo miras y ves que lo
		// tengas etc, si pasa ... se crea el handshake (v2)".
		//
		// An HTTP status and not a 4xxx close code, and the difference is
		// deliberate: this is a verdict about the ENDPOINT -- this relayer does
		// not serve that supplier, so the gateway picked the wrong one -- while
		// a frame that contradicts an established owner is a verdict about the
		// CLIENT and closes with CloseValidationFailed instead.
		//
		// v1 and sage name no supplier here; their gate is adoptOrVerifyOwner on
		// the first frame, whose ring signature and ownsSupplierKey check are
		// the real authority for both protocols.
		if handshakeSupplier := r.Header.Get(HeaderPocketSupplierAddress); handshakeSupplier != "" {
			if p.responseSigner == nil || !p.responseSigner.HasSigner(handshakeSupplier) {
				relaysRejected.WithLabelValues(serviceID, "websocket", rejectReasonNoLocalSigner).Inc()
				p.logger.Debug().
					Str(logging.FieldServiceID, serviceID).
					Str("supplier", handshakeSupplier).
					Msg("websocket handshake names a supplier this relayer holds no key for")
				p.sendError(w, http.StatusForbidden,
					fmt.Sprintf("supplier %s is not served by this relayer", handshakeSupplier))
				return
			}
		}

		// Validate and log WebSocket handshake (permissive - never rejects)
		// - PATH v2: Attempts signature verification, logs WARN if fails
		// - PATH v1: Logs INFO about legacy handshake
		p.validateAndLogWebSocketHandshake(r, serviceID)

		// Upgrade HTTP connection to WebSocket
		gatewayConn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			p.logger.Debug().Err(err).Msg("failed to upgrade to websocket")
			return
		}

		// PAST THIS LINE THE CONNECTION IS HIJACKED, and this function owns it
		// until the bridge takes it.
		//
		// Every early return below used to leak it. http.Error and
		// sendServiceUnavailable write NOWHERE once the connection is hijacked
		// -- net/http logs "WriteHeader on hijacked connection" and drops it --
		// so a bare `return` left the client holding an open WebSocket that
		// nothing would ever read or close: one FD here and one on the client,
		// reclaimed only when the peer gave up.
		//
		// A defer and not a close at each return, because the defect is the
		// SHAPE: an early return that skips teardown. Four of those were fixed
		// one at a time inside the bridge before this one was found here, a
		// hundred lines away. This makes the next early return safe without
		// anyone remembering, which a fifth point fix would not.
		bridgeOwnsConn := false
		defer func() {
			if !bridgeOwnsConn {
				_ = gatewayConn.Close()
			}
		}()

		// Get WebSocket backend endpoint from pre-checked pool
		// (wsPool is guaranteed non-nil and HasHealthy() from pre-check above)
		wsEndpoint := wsPool.Next()
		if wsEndpoint == nil {
			p.logger.Debug().
				Str(logging.FieldServiceID, serviceID).
				Msg("no healthy websocket backend at selection time")
			return
		}
		backendURL := wsEndpoint.RawURL
		// Headers still from BackendConfig (pool-level shared config)
		var configHeaders map[string]string
		if backend, ok := svcConfig.Backends["websocket"]; ok {
			configHeaders = backend.Headers
		}

		// Build headers
		headers := make(http.Header)
		for k, v := range configHeaders {
			headers.Set(k, v)
		}

		// Extract supplier address from handshake header (sent by PATH v2 protocol).
		// Empty for PATH v1 and for sage, which send no supplier header; the
		// bridge adopts the owner from the first RelayRequest in that case.
		supplierAddress := r.Header.Get(HeaderPocketSupplierAddress)

		// Add Pocket context headers for backend visibility
		headers.Set(HeaderPocketSupplier, supplierAddress)
		headers.Set(HeaderPocketService, serviceID)
		// Note: Application address is not known until first relay request arrives on the WebSocket

		arrivalHeight := p.currentBlockHeight.Load()

		// Get dial timeout from service's timeout profile
		dialTimeout := getWSDialTimeout(p.config.GetServiceTimeoutProfile(serviceID))

		// A simulated connection publishes nothing: passing a nil publisher
		// means emitRelay's Accounting skip does not depend on the bridge
		// remembering `simulated` correctly in a second place. simVerifier/
		// simKeyID are only meaningful (and only read) when simulated is true.
		bridgePublisher := p.publisher
		var bridgeSimVerifier *SimulationVerifier
		bridgeSimKeyID := ""
		if simulated {
			bridgePublisher = nil
			bridgeSimVerifier = p.simVerifier
			bridgeSimKeyID = simDirective.KeyID
		}

		// Create and run bridge
		// Session end height will be set when the first relay request arrives.
		// supplierAddress is empty for PATH v1 and sage; the bridge adopts its
		// owner from the first RelayRequest (see WebSocketBridge.owner).
		bridge, err := NewWebSocketBridge(
			p.logger,
			gatewayConn,
			backendURL,
			serviceID,
			supplierAddress,
			arrivalHeight,
			p.relayProcessor,
			bridgePublisher,
			p.responseSigner,
			headers,
			p.sessionMonitor, // Global session monitor (shared across all connections)
			p.relayPipeline,  // Unified relay pipeline for validation/metering/signing
			dialTimeout,      // Dial timeout from service's timeout profile
			simulated,
			bridgeSimVerifier,
			bridgeSimKeyID,
			// The breaker is told about the backend when the bridge actually
			// dials it, which is now the first admitted frame rather than
			// construction. Recording a success here instead would report a
			// backend nobody contacted, and a dead backend would stop being
			// visible -- the one signal an operator uses to find them.
			func(statusCode int, dialErr error) {
				threshold := p.getCircuitBreakerThreshold(serviceID, "websocket")
				transition := wsPool.RecordResult(wsEndpoint, statusCode, dialErr, threshold)
				if transition != nil {
					logCircuitBreakerTransition(p.logger, transition, serviceID, "websocket", threshold)
				}
			},
		)
		if err != nil {
			// Construction no longer dials, so a failure here is a wiring fault
			// on OUR side and says nothing about the backend. Reporting it to
			// the breaker would trip an endpoint that was never contacted.
			p.logger.Debug().Err(err).Msg("failed to create websocket bridge")
			return
		}

		// Join the shutdown drain BEFORE running, with no I/O in between: a
		// bridge that is running and not counted is one the drain cannot wait
		// for, and it is the transport that publishes last.
		//
		// Ownership is transferred AFTER this and not before, because the
		// refusal below releases NOTHING: Run never starts, so its deferred
		// release -- the only code that closes these connections -- never runs,
		// and closeWithReason only signals. The connection has to fall through
		// to this function's own defer, and that defer fires only while
		// bridgeOwnsConn is still false. Setting it above this guard left the
		// client holding an open socket until the process died: the fifth
		// instance of the early-return-after-hijack shape, and the first to
		// reach it from the far side of the ownership transfer.
		if !p.trackBridge(bridge) {
			// The proxy is already draining. Refusing here rather than running
			// is what keeps the counter honest: an Add after the drain's Wait
			// reached zero is misuse of sync.WaitGroup, not a lost connection.
			// Signalled anyway so the bridge's context is cancelled rather than
			// leaked, and so the close reason is recorded once.
			_ = bridge.closeWithReason(CloseGoingAway, "relayer shutting down", wsCloseInitiatorRelayer)
			return
		}

		// Ownership transfers here: from now on the bridge closes it, because
		// from here it is Run and release() is what tears the connections down.
		bridgeOwnsConn = true

		// Run returns after release() and its b.wg.Wait(), so this fires after
		// the last relay this bridge could publish.
		defer p.untrackBridge(bridge)

		// Run bridge (blocking)
		bridge.Run()
	}
}

// IsWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func IsWebSocketUpgrade(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}

// WebSocket handshake header constants (matching PATH's request/parser.go)
const (
	// Headers sent by PATH during WebSocket handshake for validation
	HeaderPocketSessionID          = "Pocket-Session-Id"
	HeaderPocketSessionStartHeight = "Pocket-Session-Start-Height"
	HeaderPocketSessionEndHeight   = "Pocket-Session-End-Height"
	HeaderPocketSupplierAddress    = "Pocket-Supplier-Address"
	HeaderPocketSignature          = "Pocket-Signature"
	HeaderPocketAppAddress         = "App-Address"
	HeaderTargetServiceID          = "Target-Service-Id"
	HeaderRpcType                  = "Rpc-Type"
)

// validateAndLogWebSocketHandshake validates and logs WebSocket handshake.
// This function is permissive - it never rejects connections, only logs validation results.
// - PATH v2: Attempts signature verification, logs WARN if fails (but continues)
// - PATH v1: Logs INFO about legacy handshake (no validation headers)
func (p *ProxyServer) validateAndLogWebSocketHandshake(r *http.Request, serviceID string) {
	// Extract handshake headers
	sessionID := r.Header.Get(HeaderPocketSessionID)
	sessionStartHeight := r.Header.Get(HeaderPocketSessionStartHeight)
	sessionEndHeight := r.Header.Get(HeaderPocketSessionEndHeight)
	supplierAddress := r.Header.Get(HeaderPocketSupplierAddress)
	appAddress := r.Header.Get(HeaderPocketAppAddress)
	signature := r.Header.Get(HeaderPocketSignature)
	rpcType := r.Header.Get(HeaderRpcType)

	// Check if we have PATH v2 validation headers
	hasV2Headers := sessionID != "" && supplierAddress != "" && signature != ""

	if !hasV2Headers {
		// PATH v1 - no validation headers present
		p.logger.Debug().
			Str(logging.FieldServiceID, serviceID).
			Str("remote_addr", r.RemoteAddr).
			Str("app_address", appAddress).
			Str("rpc_type", rpcType).
			Msg("websocket handshake from PATH v1 (no validation headers)")
		return
	}

	// PATH v2 - validation headers present, attempt verification
	p.logger.Debug().
		Str(logging.FieldServiceID, serviceID).
		Str("remote_addr", r.RemoteAddr).
		Str("session_id", sessionID).
		Str("session_start_height", sessionStartHeight).
		Str("session_end_height", sessionEndHeight).
		Str("supplier_address", supplierAddress).
		Str("app_address", appAddress).
		Str("rpc_type", rpcType).
		Bool("has_signature", true).
		Int("signature_length", len(signature)).
		Msg("websocket handshake from PATH v2 (with validation headers)")

	// TODO(PATH-v2): Implement full handshake signature verification
	// The signature should be verified against a reconstructed message containing:
	// - Session ID, session start/end heights
	// - Supplier address, application address
	// - Service ID, RPC type
	//
	// For now, we accept all handshakes permissively until PATH v2 protocol is finalized.
	// When ready, we should:
	// 1. Reconstruct the signed message structure (matching PATH's signing logic)
	// 2. Verify signature using application's public key (from session or blockchain)
	// 3. Log WARN if verification fails (but still allow connection for backward compatibility)

	if p.validator != nil {
		// Placeholder for future signature verification
		// When PATH v2 is stable, uncomment and implement:
		/*
			verifyErr := p.verifyWebSocketHandshakeSignature(sessionID, sessionStartHeight, sessionEndHeight,
				supplierAddress, appAddress, serviceID, rpcType, signature)
			if verifyErr != nil {
				p.logger.Warn().
					Err(verifyErr).
					Str(logging.FieldServiceID, serviceID).
					Str("session_id", sessionID).
					Str("supplier_address", supplierAddress).
					Msg("websocket handshake signature verification failed (accepting permissively)")
				return
			}
			p.logger.Debug().
				Str(logging.FieldServiceID, serviceID).
				Str("session_id", sessionID).
				Msg("websocket handshake signature verified successfully")
		*/

		// For now, just log that verification is not yet implemented
		p.logger.Debug().
			Str(logging.FieldServiceID, serviceID).
			Str("session_id", sessionID).
			Msg("websocket handshake signature verification not yet implemented (accepting permissively)")
	}
}
