//go:build test

package relayer

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// hangingPublishProcessor stands in for a relay processor whose difficulty
// lookup hangs for one service: that service's publish tasks wait for release,
// every other service's run at once.
type hangingPublishProcessor struct {
	hung    string
	release chan struct{}
	entered chan struct{}

	mu        sync.Mutex
	processed map[string]int
	retained  map[string]int64
	queues    map[string]*servicePublishQueue

	othersWant int
	othersCh   chan struct{}
}

func newHangingPublishProcessor(hung string) *hangingPublishProcessor {
	return &hangingPublishProcessor{
		hung:      hung,
		release:   make(chan struct{}),
		entered:   make(chan struct{}, 16),
		processed: map[string]int{},
		retained:  map[string]int64{},
	}
}

func (h *hangingPublishProcessor) ProcessRelay(ctx context.Context, reqBody, respBody []byte, supplierAddr, serviceID string, arrivalBlockHeight int64) (*transport.MinedRelayMessage, error) {
	return h.ProcessRelayPayloadHash(ctx, reqBody, sha256.Sum256(respBody), supplierAddr, serviceID, arrivalBlockHeight)
}

func (h *hangingPublishProcessor) ProcessRelayPayloadHash(_ context.Context, _ []byte, _ [32]byte, _, serviceID string, _ int64) (*transport.MinedRelayMessage, error) {
	if serviceID == h.hung {
		h.mu.Lock()
		h.retained[serviceID] = h.queues[serviceID].queued.Load()
		h.mu.Unlock()
		h.entered <- struct{}{}
		<-h.release
	}
	h.mu.Lock()
	h.processed[serviceID]++
	h.signalOthersLocked()
	h.mu.Unlock()
	return nil, nil
}

func (h *hangingPublishProcessor) GetServiceDifficulty(context.Context, string, int64) ([]byte, error) {
	return nil, nil
}

func (h *hangingPublishProcessor) SetDifficultyProvider(DifficultyProvider) {}

// othersDone returns a channel closed once n relays of services other than the
// hung one were processed.
func (h *hangingPublishProcessor) othersDone(n int) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.othersWant, h.othersCh = n, make(chan struct{})
	h.signalOthersLocked()
	return h.othersCh
}

func (h *hangingPublishProcessor) signalOthersLocked() {
	if h.othersCh == nil {
		return
	}
	got := 0
	for svc, c := range h.processed {
		if svc != h.hung {
			got += c
		}
	}
	if got >= h.othersWant {
		close(h.othersCh)
		h.othersCh = nil
	}
}

func (h *hangingPublishProcessor) count(serviceID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.processed[serviceID]
}

func servicePublishRejections(serviceID string) float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(serviceID, BackendTypeJSONRPC, rejectReasonServicePublishQueueFull))
}

// A service whose publish tasks hang -- its difficulty lookup does not answer --
// fills its own bound and is refused with 429, while another service is still
// admitted AND published: its tasks run on its own workers and its bytes count
// against its own bound.
func TestPublishQueue_AHungServiceIsRefusedAndTheOthersKeepPublishing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`))
	}))
	defer backend.Close()

	f, _, _, _ := newEagerChargeFixture(t, backend.URL, acceptAnyValidator{})
	proc := newHangingPublishProcessor(simTestService)
	proc.queues = f.proxy.publishQueues
	f.proxy.relayProcessor = proc
	hung := f.proxy.publishQueues[simTestService]
	other := f.proxy.publishQueues[simTestService2]
	require.NotNil(t, hung)
	require.NotNil(t, other)
	// Two hung relays pin both of the hung service's workers and fill its bound.
	hungBody := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-hung")
	hung.maxBytes = int64(2 * (len(hungBody) + sha256.Size))
	for i := 0; i < publishWorkersPerService; i++ {
		w := f.post(t, hungBody, false)
		require.Equal(t, http.StatusOK, w.Code, "hung relay %d is admitted; body=%s", i, w.Body.String())
		<-proc.entered
	}

	before := servicePublishRejections(simTestService)
	w := f.post(t, hungBody, false)
	require.Equal(t, http.StatusTooManyRequests, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	require.Equal(t, before+1, servicePublishRejections(simTestService), "the refusal names the service that filled its bound")

	otherBefore := servicePublishRejections(simTestService2)
	const others = 3
	for i := 0; i < others; i++ {
		w = f.post(t, f.buildSignedSimBody(t, f.appAddr, simTestService2, "sess-other"), false)
		require.Equal(t, http.StatusOK, w.Code, "the other service is admitted; body=%s", w.Body.String())
	}
	// Bounded wait: with workers shared, the other service's tasks never run.
	select {
	case <-proc.othersDone(others):
	case <-time.After(5 * time.Second):
		t.Fatalf("the other service's tasks did not run while the hung one waits: %d of %d", proc.count(simTestService2), others)
	}
	require.Equal(t, 0, proc.count(simTestService), "premise: the hung service's tasks are still waiting")
	require.Equal(t, otherBefore, servicePublishRejections(simTestService2))

	close(proc.release)
	hung.pool.StopAndWait()
	other.pool.StopAndWait()
	require.Equal(t, publishWorkersPerService, proc.count(simTestService))
	require.Zero(t, hung.queued.Load(), "a finished task gives its bytes back")
}

// A waiting publish task retains the request and the 32-byte hash of the
// response, not the response: a large response does not make the task large.
func TestPublishQueue_ATaskRetainsTheRequestAndTheResponseHashOnly(t *testing.T) {
	big := make([]byte, 256<<10)
	for i := range big {
		big[i] = 'x'
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	defer backend.Close()

	f, _, _, _ := newEagerChargeFixture(t, backend.URL, acceptAnyValidator{})
	proc := newHangingPublishProcessor(simTestService)
	proc.queues = f.proxy.publishQueues
	f.proxy.relayProcessor = proc
	queue := f.proxy.publishQueues[simTestService]

	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-retained")
	w := f.post(t, body, false)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	<-proc.entered

	proc.mu.Lock()
	retained := proc.retained[simTestService]
	proc.mu.Unlock()
	require.Equal(t, int64(len(body)+sha256.Size), retained, "request plus the hash, not the %d-byte response", len(big))
	require.Equal(t, float64(retained), testutil.ToFloat64(publishQueueBytes.WithLabelValues(simTestService)), "the service's gauge reads what its queue retains")
	require.Equal(t, float64(queue.maxBytes), testutil.ToFloat64(publishQueueMaxBytes.WithLabelValues(simTestService)), "and its ceiling is published next to it")

	close(proc.release)
	queue.pool.StopAndWait()
	require.Zero(t, testutil.ToFloat64(publishQueueBytes.WithLabelValues(simTestService)), "a finished task takes its bytes off the gauge")
}

// The relay mined from the response's hash is the relay mined from the response:
// same bytes, same hash, and a supplier signature that verifies.
func TestProcessRelayPayloadHash_MinesTheSameRelayAsTheBody(t *testing.T) {
	priv := secp256k1.GenPrivKey()
	addr := cosmostypes.AccAddress(priv.PubKey().Address()).String()
	signer, err := NewResponseSigner(testLogger(), map[string]cryptotypes.PrivKey{addr: priv})
	require.NoError(t, err)
	rp := NewRelayProcessor(testLogger(), nil, NewResponseSignerAdapter(signer), nil)
	rp.SetDifficultyProvider(zeroTargetDifficultyForPayload{})

	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress: addr, ServiceId: "svc-hash", SessionId: "sess-hash",
				SessionStartBlockHeight: 100, SessionEndBlockHeight: 110,
			},
			SupplierOperatorAddress: addr,
		},
		Payload: []byte("request"),
	}
	reqBz, err := req.Marshal()
	require.NoError(t, err)
	resp := []byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`)
	ctx := WithRPCType(context.Background(), BackendTypeJSONRPC)

	fromBody, err := rp.ProcessRelay(ctx, reqBz, resp, addr, "svc-hash", 105)
	require.NoError(t, err)
	fromHash, err := rp.ProcessRelayPayloadHash(ctx, reqBz, sha256.Sum256(resp), addr, "svc-hash", 105)
	require.NoError(t, err)
	require.NotNil(t, fromBody)
	require.NotNil(t, fromHash)
	require.Equal(t, fromBody.RelayBytes, fromHash.RelayBytes)
	require.Equal(t, fromBody.RelayHash, fromHash.RelayHash)

	relay := &servicetypes.Relay{}
	require.NoError(t, relay.Unmarshal(fromHash.RelayBytes))
	require.Nil(t, relay.Res.Payload, "the mined relay carries no payload")
	wantHash := sha256.Sum256(resp)
	require.Equal(t, wantHash[:], relay.Res.PayloadHash)
	require.NoError(t, relay.Res.VerifySupplierOperatorSignature(priv.PubKey()), "the chain's signature check passes")
}

// zeroTargetDifficultyForPayload mines every relay: the target is all ones.
type zeroTargetDifficultyForPayload struct{}

func (zeroTargetDifficultyForPayload) GetTargetHash(context.Context, string, int64) ([]byte, error) {
	target := make([]byte, 32)
	for i := range target {
		target[i] = 0xff
	}
	return target, nil
}

// An empty response is not mined, from the body or from its hash, as before the
// publish path kept only the hash.
func TestProcessRelayPayloadHash_AnEmptyResponseIsNotMined(t *testing.T) {
	priv := secp256k1.GenPrivKey()
	addr := cosmostypes.AccAddress(priv.PubKey().Address()).String()
	signer, err := NewResponseSigner(testLogger(), map[string]cryptotypes.PrivKey{addr: priv})
	require.NoError(t, err)
	rp := NewRelayProcessor(testLogger(), nil, NewResponseSignerAdapter(signer), nil)
	rp.SetDifficultyProvider(zeroTargetDifficultyForPayload{})
	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress: addr, ServiceId: "svc-empty", SessionId: "sess-empty",
				SessionStartBlockHeight: 100, SessionEndBlockHeight: 110,
			},
			SupplierOperatorAddress: addr,
		},
		Payload: []byte("request"),
	}
	reqBz, err := req.Marshal()
	require.NoError(t, err)
	ctx := WithRPCType(context.Background(), BackendTypeJSONRPC)

	msg, err := rp.ProcessRelay(ctx, reqBz, nil, addr, "svc-empty", 105)
	require.ErrorIs(t, err, errEmptyResponsePayload)
	require.Nil(t, msg)
	msg, err = rp.ProcessRelayPayloadHash(ctx, reqBz, sha256.Sum256(nil), addr, "svc-empty", 105)
	require.ErrorIs(t, err, errEmptyResponsePayload)
	require.Nil(t, msg)
}
