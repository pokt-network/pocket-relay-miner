//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// A WebSocket client frame is only checked against its session budget. Every
// message the backend answers it with is a relay, served and then charged. The
// message that takes the pair to its budget is still served and charged, and
// then closes the connection.

// ownerTestCap is the per-supplier budget of newOwnerTestPipelineWithCharges at
// 1 uPOKT a relay: app stake 1,000,000 times service factor 1.
const ownerTestCap = int64(1_000_000)

// publishSignal reports every publish on a channel. The bridge charges a backend
// message and then publishes it on the same goroutine, so an observed publish
// means that message's charge is already in the ledger.
type publishSignal struct{ published chan struct{} }

func newPublishSignal() *publishSignal {
	return &publishSignal{published: make(chan struct{}, 64)}
}

func (p *publishSignal) Publish(context.Context, *transport.MinedRelayMessage) error {
	p.published <- struct{}{}
	return nil
}

func (p *publishSignal) Close() error { return nil }

// await waits for n publishes.
func (p *publishSignal) await(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		awaitSignal(t, p.published, "publish")
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// silentWSBackend reads client messages and never answers, reporting each one it
// read. A message read here has already been checked against the budget: the
// bridge checks a frame before it forwards it.
func silentWSBackend(t *testing.T) (wsURL string, received chan struct{}) {
	t.Helper()
	received = make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := WebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			received <- struct{}{}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), received
}

// newUnstartedOwnerBridge builds a bridge owned by supplier and never runs it, so
// the test hands backend messages to handleBackendMessage itself, in the order it
// chooses.
func newUnstartedOwnerBridge(t *testing.T, signer *ResponseSigner, pipeline *RelayPipeline, supplier string) (*WebSocketBridge, *websocket.Conn) {
	t.Helper()
	relayerConn, gwClient := newGatewaySideHarness(t)
	backendURL, _, _ := countingWSBackend(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", atHeight(100),
		&recordingProcessor{}, &recordingPublisher{}, signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bridge.Close() })
	bridge.owner.Store(&supplier)
	return bridge, gwClient
}

// admitFrame puts req through the budget check the way the bridge's first frame
// goes, and makes it the request backend messages answer.
func admitFrame(t *testing.T, pipeline *RelayPipeline, bridge *WebSocketBridge, req *servicetypes.RelayRequest) {
	t.Helper()
	allowed, err := pipeline.MeterRelay(context.Background(), &RelayContext{
		Request:            req,
		ServiceID:          simWSTestService,
		SupplierAddress:    req.Meta.SupplierOperatorAddress,
		SessionID:          req.Meta.SessionHeader.SessionId,
		ArrivalBlockHeight: 100,
	})
	require.NoError(t, err)
	require.True(t, allowed, "premise: the frame fits the budget")
	bridge.setLatestRequest(req)
}

func backendMessage(body string) wsMessage {
	return wsMessage{data: []byte(body), source: wsMessageSourceBackend, messageType: websocket.TextMessage}
}

// readServedPayload reads one signed response and returns the backend payload it
// carries.
func readServedPayload(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, bz, err := conn.ReadMessage()
	require.NoError(t, err)
	var resp servicetypes.RelayResponse
	require.NoError(t, resp.Unmarshal(bz))
	return string(resp.Payload)
}

func readRaw(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, bz, err := conn.ReadMessage()
	require.NoError(t, err)
	return string(bz)
}

// TestABackendMessageThatReachesTheBudgetIsServedChargedAndCloses leaves one
// relay of room: the frame fits, the first message spends it, and the connection
// closes before the backend's next message is served.
func TestABackendMessageThatReachesTheBudgetIsServedChargedAndCloses(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "ws-at-budget"
	pipeline, rc, _, charges := newOwnerTestPipelineWithCharges(t)
	supplier, signer := newSupplier(t)
	key := rc.KB().MeterConsumedKey(sessionID, supplier)
	require.NoError(t, rc.Set(context.Background(), key, ownerTestCap-1, time.Hour).Err())
	overBudget := relaysServedOverBudget.WithLabelValues(simWSTestService, BackendTypeWebSocket, overBudgetReasonPushAtBudget)
	before := testutil.ToFloat64(overBudget)

	conn := newSageShapedBridge(t, pushingWSBackend(t, 3), signer, pipeline)
	sendRelay(t, conn, ownerTestRelay(sessionID, supplier))

	require.Contains(t, readServedPayload(t, conn), `"sequence":1`,
		"the message that reaches the budget is still served")
	_, _, err := conn.ReadMessage()
	require.True(t, websocket.IsCloseError(err, CloseStakeLimitExceeded),
		"and then the connection closes at the budget, before the next message is served; got %v", err)

	// The close frame is written after the message loop returned, so nothing can
	// be charged past this line.
	charges.flush()
	require.Equal(t, ownerTestCap, consumedIn(t, rc, key), "the served message is charged, and only it")
	require.Equal(t, before+1, testutil.ToFloat64(overBudget), "one connection crossed the budget: one count")
}

// TestABackendMessageQueuedBehindTheBudgetCloseIsNeitherServedNorCharged: the
// close only cancels the bridge context, and the message loop may still take a
// message the backend had already queued. Handing both messages in order forces
// that pick.
func TestABackendMessageQueuedBehindTheBudgetCloseIsNeitherServedNorCharged(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "ws-queued-behind-close"
	pipeline, rc, _, charges := newOwnerTestPipelineWithCharges(t)
	supplier, signer := newSupplier(t)
	key := rc.KB().MeterConsumedKey(sessionID, supplier)
	require.NoError(t, rc.Set(context.Background(), key, ownerTestCap-1, time.Hour).Err())
	overBudget := relaysServedOverBudget.WithLabelValues(simWSTestService, BackendTypeWebSocket, overBudgetReasonPushAtBudget)
	before := testutil.ToFloat64(overBudget)

	bridge, gwClient := newUnstartedOwnerBridge(t, signer, pipeline, supplier)
	admitFrame(t, pipeline, bridge, ownerTestRelay(sessionID, supplier))

	bridge.handleBackendMessage(backendMessage(`{"push":1}`))
	require.Error(t, bridge.ctx.Err(), "premise: the first message closed the connection at the budget")
	bridge.handleBackendMessage(backendMessage(`{"push":2}`))

	require.NoError(t, bridge.writeToGateway(websocket.TextMessage, []byte("sentinel")))
	require.Contains(t, readServedPayload(t, gwClient), `"push":1`)
	require.Equal(t, "sentinel", readRaw(t, gwClient), "the message queued behind the close reached the client")

	charges.flush()
	require.Equal(t, ownerTestCap, consumedIn(t, rc, key), "the message queued behind the close was charged")
	require.Equal(t, before+1, testutil.ToFloat64(overBudget), "one connection crossed the budget: one count")
}

// TestAFrameRefusedAtTheBudgetIsNotCountedAsServedOverIt: a frame that does not
// fit is refused before anything is served, so it counts as a rejection and not
// as a relay served over the budget.
func TestAFrameRefusedAtTheBudgetIsNotCountedAsServedOverIt(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "ws-frame-over-budget"
	pipeline, rc, _, _ := newOwnerTestPipelineWithCharges(t)
	supplier, signer := newSupplier(t)
	key := rc.KB().MeterConsumedKey(sessionID, supplier)
	require.NoError(t, rc.Set(context.Background(), key, ownerTestCap, time.Hour).Err())
	overBudget := relaysServedOverBudget.WithLabelValues(simWSTestService, BackendTypeWebSocket, overBudgetReasonPushAtBudget)
	overBefore := testutil.ToFloat64(overBudget)
	rejected := relaysRejected.WithLabelValues(simWSTestService, "websocket", rejectReasonStakeExhausted)
	rejectedBefore := testutil.ToFloat64(rejected)

	conn := newSageShapedBridge(t, pushingWSBackend(t, 1), signer, pipeline)
	sendRelay(t, conn, ownerTestRelay(sessionID, supplier))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, _, err := conn.ReadMessage()
	require.True(t, websocket.IsCloseError(err, CloseStakeLimitExceeded),
		"premise: the frame is refused at the budget; got %v", err)
	require.Equal(t, rejectedBefore+1, testutil.ToFloat64(rejected), "the refusal is a rejection")
	require.Equal(t, overBefore, testutil.ToFloat64(overBudget), "nothing was served, so nothing was served over the budget")
}

// ctxObservingSharedParams fails a lookup whose context is already done, as a
// lookup that had to reach the network would.
type ctxObservingSharedParams struct{ SharedParamCache }

func (c ctxObservingSharedParams) GetSharedParams(ctx context.Context, height int64) (*sharedtypes.Params, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.SharedParamCache.GetSharedParams(ctx, height)
}

func (c ctxObservingSharedParams) GetLatestSharedParams(ctx context.Context) (*sharedtypes.Params, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.SharedParamCache.GetLatestSharedParams(ctx)
}

// TestABackendMessageServedAsTheBridgeClosesIsStillCharged: the message was
// written to the client, and something outside the message loop closes the
// bridge before the message is charged. It was served, so it is charged.
func TestABackendMessageServedAsTheBridgeClosesIsStillCharged(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "ws-served-as-closing"
	pipeline, rc, _, _ := newOwnerTestPipelineWithCharges(t)
	meter := pipeline.relayMeter
	meter.sharedParamCache = ctxObservingSharedParams{meter.sharedParamCache}
	supplier, signer := newSupplier(t)
	req := ownerTestRelay(sessionID, supplier)

	bridge, _ := newUnstartedOwnerBridge(t, signer, pipeline, supplier)
	admitFrame(t, pipeline, bridge, req)
	require.NoError(t, bridge.closeWithReason(CloseSessionExpired, "session expired", wsCloseInitiatorRelayer))

	bridge.chargeServedMessage(req)

	require.Equal(t, int64(1), meter.ChargeLedger().Pending(rc.KB().MeterConsumedKey(sessionID, supplier)),
		"a message already served must be charged even when the bridge closed right after serving it")
}

// TestAStaleDispatcherClosesAnOpenWebSocketOnItsNextBackendMessage: once the
// dispatcher stops reaching Redis, a charge may never be written, so the next
// backend message closes the connection instead of being served.
func TestAStaleDispatcherClosesAnOpenWebSocketOnItsNextBackendMessage(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const sessionID = "ws-stale-dispatcher"
	pipeline, rc, _, _ := newOwnerTestPipelineWithCharges(t)
	supplier, signer := newSupplier(t)
	key := rc.KB().MeterConsumedKey(sessionID, supplier)
	rejected := relaysRejected.WithLabelValues(simWSTestService, "websocket", rejectReasonMeterError)
	before := testutil.ToFloat64(rejected)
	heartbeatErrors := relayMeterErrors.WithLabelValues(meterOperationDispatcherHeartbeat)
	heartbeatErrorsBefore := testutil.ToFloat64(heartbeatErrors)

	bridge, gwClient := newUnstartedOwnerBridge(t, signer, pipeline, supplier)
	admitFrame(t, pipeline, bridge, ownerTestRelay(sessionID, supplier))
	pipeline.relayMeter.SetDispatcherHeartbeat(func() time.Time { return time.Time{} })

	bridge.handleBackendMessage(backendMessage(`{"push":1}`))

	reason := bridge.closeReason.Load()
	require.NotNil(t, reason, "the connection must close")
	require.Equal(t, CloseTryAgainLater, reason.code)
	require.NoError(t, bridge.writeToGateway(websocket.TextMessage, []byte("sentinel")))
	require.Equal(t, "sentinel", readRaw(t, gwClient), "a message was served while its charge could not be written")
	require.Zero(t, pipeline.relayMeter.ChargeLedger().Pending(key), "and nothing was charged")
	require.Equal(t, before+1, testutil.ToFloat64(rejected))
	require.Equal(t, heartbeatErrorsBefore+1, testutil.ToFloat64(heartbeatErrors),
		"relay_meter_errors_total{operation=\"dispatcher heartbeat\"} counts the close, as it counts HTTP and gRPC refusals")
}
