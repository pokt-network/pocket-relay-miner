//go:build test

package relayer

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// execStall holds every MULTI/EXEC of the client it is installed on until
// release is closed, and reports the first one on entered. PINGs pass: a stalled
// EXEC is a Redis that answers some commands and not the write, which is what the
// dispatcher's in-flight round saw on 2026-09-23 at 03:08:20-24.
type execStall struct {
	entered chan struct{}
	release chan struct{}
	first   chan struct{}
}

func newExecStall(client redis.UniversalClient) *execStall {
	s := &execStall{entered: make(chan struct{}), release: make(chan struct{}), first: make(chan struct{}, 1)}
	s.first <- struct{}{}
	client.AddHook(s)
	return s
}

func (s *execStall) DialHook(next redis.DialHook) redis.DialHook { return next }

func (s *execStall) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (s *execStall) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		select {
		case <-s.first:
			close(s.entered)
		default:
		}
		<-s.release
		return next(ctx, cmds)
	}
}

// sizedMinedProcessor mines every relay into a message whose RelayBytes is the
// payload it was handed, so the bytes a relay retains in the batch queue scale
// with what the backend answered, as they do in production.
type sizedMinedProcessor struct {
	sessionID string
}

func (p *sizedMinedProcessor) ProcessRelay(
	_ context.Context,
	_, respBody []byte,
	supplierAddr, serviceID string,
	_ int64,
) (*transport.MinedRelayMessage, error) {
	return &transport.MinedRelayMessage{
		RelayHash:               []byte("hash"),
		RelayBytes:              append([]byte(nil), respBody...),
		ComputeUnitsPerRelay:    1,
		SessionId:               p.sessionID,
		SessionEndHeight:        100,
		SupplierOperatorAddress: supplierAddr,
		ServiceId:               serviceID,
		ApplicationAddress:      ownerTestAppAddr,
	}, nil
}

func (p *sizedMinedProcessor) GetServiceDifficulty(context.Context, string, int64) ([]byte, error) {
	return nil, nil
}

func (p *sizedMinedProcessor) SetDifficultyProvider(DifficultyProvider) {}

// servedUntilClose hands the bridge up to n backend messages of payload and reads
// the relay each one serves, until the bridge closes. It returns how many were
// served, the index of the first message handed while queueFull already said
// full (-1 if none), and the bridge's close verdict (nil while open).
//
// It ends ON THE CLOSE and not on a read timeout: this bridge is never Run, so
// nothing writes a close frame, and a read after the close would only wait out
// its deadline.
func servedUntilClose(t *testing.T, bridge *WebSocketBridge, gwClient *websocket.Conn,
	payload []byte, n int, queueFull func() bool,
) (served, firstFull int, verdict *wsCloseReason) {
	t.Helper()
	firstFull = -1
	for i := 0; i < n; i++ {
		if firstFull < 0 && queueFull() {
			firstFull = i
		}
		bridge.handleBackendMessage(backendMessage(string(payload)))
		if bridge.ctx.Err() != nil {
			break
		}
		readServedPayload(t, gwClient)
		served++
	}
	return served, firstFull, bridge.closeReason.Load()
}

// stalledQueueBridge is the 2026-09-23 excess put in place: a batcher whose
// only dispatch round hangs on its EXEC (so nothing drains, and it is inside the
// silence budget, so DispatcherHealthy stays true), and an open connection that
// publishes into it, admitted while the queue was empty. Its gate is the closure
// cmd_relayer.go wires, QueuedBytes() >= cap. release lets the round finish.
type stalledQueueBridge struct {
	bridge    *WebSocketBridge
	gwClient  *websocket.Conn
	batcher   *redisutil.BatchingPublisher
	queueFull func() bool
	release   func()
	stream    string
	client    *redisutil.Client
	consumed  func() int64
}

func newStalledQueueBridge(t *testing.T, sessionID string, capBytes int, stall bool) *stalledQueueBridge {
	t.Helper()
	pipeline, rc, prefix, charges := newOwnerTestPipelineWithCharges(t)
	supplier, signer := newSupplier(t)

	stallClient := newTestRedisOnPrefix(t, prefix)
	st := newExecStall(stallClient.UniversalClient)
	batcher := redisutil.NewBatchingPublisher(testLogger(), stallClient.UniversalClient,
		stallClient.KB().StreamPrefix(), 5*time.Millisecond)
	t.Cleanup(func() { _ = batcher.Close() })
	// Registered after Close, so it runs first: a Close with the EXEC still held
	// would wait out its final flush.
	var once sync.Once
	release := func() { once.Do(func() { close(st.release) }) }
	t.Cleanup(release)
	if !stall {
		release()
	}
	queueFull := func() bool { return batcher.QueuedBytes() >= capBytes }

	require.Eventually(t, func() bool { alive, _ := batcher.DispatcherHealthy(); return alive },
		10*time.Second, time.Millisecond, "premise: the dispatcher reached Redis (its PING passes the stall)")
	if stall {
		// One relay starts a round, and the round hangs on its EXEC.
		seed := &transport.MinedRelayMessage{
			RelayHash: []byte("seed"), RelayBytes: []byte("seed"), SessionId: sessionID,
			SessionEndHeight: 100, SupplierOperatorAddress: supplier, ServiceId: simWSTestService,
		}
		require.NoError(t, batcher.Publish(context.Background(), seed))
		select {
		case <-st.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("premise: the dispatcher never started its round")
		}
		require.Equal(t, 0, batcher.QueuedBytes(), "premise: the seed left the queue with the hung round")
	}

	require.False(t, queueFull(), "premise: the handshake would have been admitted")
	relayerConn, gwClient := newGatewaySideHarness(t)
	backendURL, _, _ := countingWSBackend(t)
	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", atHeight(100),
		&sizedMinedProcessor{sessionID: sessionID}, batcher, signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", nil, queueFull,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bridge.Close() })
	bridge.owner.Store(&supplier)
	admitFrame(t, pipeline, bridge, ownerTestRelay(sessionID, supplier))

	key := rc.KB().MeterConsumedKey(sessionID, supplier)
	return &stalledQueueBridge{
		bridge: bridge, gwClient: gwClient, batcher: batcher, queueFull: queueFull, release: release,
		stream: transport.SupplierStreamName(stallClient.KB().StreamPrefix(), supplier), client: stallClient,
		consumed: func() int64 { charges.flush(); return consumedIn(t, rc, key) },
	}
}

// TestAnOpenWebSocketClosesWhenThePublishQueueIsFull is the 2026-09-23 excess,
// turned: max(ha_relayer_batch_queue_bytes) read 1248 and 1568 MiB against a
// 512 MiB cap, 93% of the excess from WebSocket connections ALREADY OPEN, because
// the queue gate was asked only at the handshake. An open connection now asks it
// for every backend message, before signing: the message that finds the queue
// full is neither served, nor charged, nor published, and the connection closes
// with 1013 and its own text.
func TestAnOpenWebSocketClosesWhenThePublishQueueIsFull(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const (
		payloadBytes = 64 << 10
		relays       = 16
		// About four relays' worth: the queue crosses it within the first five.
		capBytes = 4 * payloadBytes
	)
	h := newStalledQueueBridge(t, "ws-queue-full", capBytes, true)
	rejectedBefore := queueFullRejections(simWSTestService, BackendTypeWebSocket)

	// Incompressible: the relayer compresses relay bytes before they are queued,
	// and a compressible filler would never fill the queue.
	served, firstFull, verdict := servedUntilClose(t, h.bridge, h.gwClient,
		transport.ChainedHashBytes("ws-queue-full", payloadBytes), relays, h.queueFull)

	require.True(t, firstFull > 0 && firstFull <= 5,
		"premise: the queue crossed its cap within the first five relays (first full before relay %d)", firstFull)
	require.NotNil(t, verdict, "the connection must close once the queue is full")
	require.Equal(t, CloseTryAgainLater, verdict.code, "closed with 1013, try again later")
	require.Equal(t, wsCloseTextPublishQueueFull, verdict.text, "with the queue's own close text")
	require.Equal(t, firstFull, served, "every message before the queue was full is served, and none after")
	require.Equal(t, rejectedBefore+1, queueFullRejections(simWSTestService, BackendTypeWebSocket),
		"the refused message is one publish_queue_full rejection")
	require.Less(t, h.batcher.QueuedBytes(), capBytes+2*payloadBytes,
		"the queue holds at most the relay that crossed its cap past it, not 3x its cap")
	require.Equal(t, int64(served), h.consumed(), "only the served messages are charged")
	alive, healthErr := h.batcher.DispatcherHealthy()
	require.True(t, alive, "control: the close came from the queue, not from the dispatcher refusal: %v", healthErr)

	// Released, the dispatcher writes what was published: the seed and every
	// served relay, and nothing the closed connection refused.
	h.release()
	require.Eventually(t, func() bool { return h.batcher.QueuedBytes() == 0 }, 10*time.Second, time.Millisecond,
		"control: released, the dispatcher drains the queue")
	require.Eventually(t, func() bool {
		n, err := h.client.XLen(context.Background(), h.stream).Result()
		return err == nil && n == int64(served)+1
	}, 10*time.Second, time.Millisecond, "the seed and the served relays landed, and nothing else")
}

// TestAnOpenWebSocketKeepsServingWhileThePublishQueueHasRoom is the control of
// the test above: the same connection and gate with nothing holding the queue
// serves every message and stays open.
func TestAnOpenWebSocketKeepsServingWhileThePublishQueueHasRoom(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const (
		payloadBytes = 64 << 10
		relays       = 16
		capBytes     = 4 * payloadBytes
	)
	h := newStalledQueueBridge(t, "ws-queue-room", capBytes, false)
	rejectedBefore := queueFullRejections(simWSTestService, BackendTypeWebSocket)

	// Each relay is drained before the next is handed over, so the queue never
	// fills: what it holds is waited out, not raced.
	var served int
	for i := 0; i < relays; i++ {
		require.Eventually(t, func() bool { return h.batcher.QueuedBytes() < capBytes }, 10*time.Second, time.Millisecond)
		n, _, verdict := servedUntilClose(t, h.bridge, h.gwClient,
			transport.ChainedHashBytes(fmt.Sprint("ws-queue-room-", i), payloadBytes), 1, h.queueFull)
		require.Nil(t, verdict, "a queue with room must not close the connection")
		served += n
	}
	require.Equal(t, relays, served, "every message is served")
	require.Equal(t, rejectedBefore, queueFullRejections(simWSTestService, BackendTypeWebSocket))
	require.Equal(t, int64(relays), h.consumed(), "every served message is charged")
}

// TestAClientFrameIsRefusedBeforeTheBackendWhenThePublishQueueIsFull: with the
// queue full, a relay frame from the client closes the connection with 1013
// before validation and before the backend is dialled -- the frame would become
// a relay the queue cannot take. The control, with the same frame and a gate
// that says there is room, reaches the backend.
func TestAClientFrameIsRefusedBeforeTheBackendWhenThePublishQueueIsFull(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	for _, tc := range []struct {
		name  string
		full  bool
		dials int32
	}{{"full", true, 0}, {"room", false, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			const sessionID = "ws-client-frame-queue"
			pipeline, _, _, _ := newOwnerTestPipelineWithCharges(t)
			supplier, signer := newSupplier(t)
			relayerConn, _ := newGatewaySideHarness(t)
			backendURL, dials, _ := countingWSBackend(t)
			bridge, err := NewWebSocketBridge(
				testLogger(), relayerConn, backendURL, simWSTestService, "", atHeight(100),
				&recordingProcessor{}, &recordingPublisher{}, signer, http.Header{},
				nil, pipeline, 5*time.Second, false, nil, "", nil, func() bool { return tc.full },
			)
			require.NoError(t, err)
			t.Cleanup(func() { _ = bridge.Close() })
			// This bridge is never Run, so nothing releases the backend
			// connection an admitted frame opens: close it here. backendConn is
			// written on this goroutine, by handleGatewayMessage below.
			t.Cleanup(func() {
				if bridge.backendConn != nil {
					_ = bridge.backendConn.Close()
				}
			})
			rejectedBefore := queueFullRejections(simWSTestService, BackendTypeWebSocket)

			bz, err := ownerTestRelay(sessionID, supplier).Marshal()
			require.NoError(t, err)
			bridge.handleGatewayMessage(wsMessage{data: bz, source: wsMessageSourceGateway, messageType: websocket.BinaryMessage})

			if tc.full {
				verdict := bridge.closeReason.Load()
				require.NotNil(t, verdict, "a client frame with the queue full closes the connection")
				require.Equal(t, CloseTryAgainLater, verdict.code)
				require.Equal(t, wsCloseTextPublishQueueFull, verdict.text)
				require.Equal(t, rejectedBefore+1, queueFullRejections(simWSTestService, BackendTypeWebSocket))
				require.Equal(t, tc.dials, dials.Load(), "the backend is never dialled for a refused frame")
				return
			}
			require.Nil(t, bridge.closeReason.Load(), "control: with room the frame is not refused")
			require.Eventually(t, func() bool { return dials.Load() == tc.dials }, 10*time.Second, time.Millisecond,
				"control: with room the same frame reaches the backend")
			require.Equal(t, rejectedBefore, queueFullRejections(simWSTestService, BackendTypeWebSocket))
		})
	}
}
