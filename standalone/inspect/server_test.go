//go:build test

package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	supplierA = "pokt1inspecta"
	supplierB = "pokt1inspectb"
)

// fixture is a real store with every kind of state the server reads, the way
// the standalone process wires it.
type fixture struct {
	t       *testing.T
	backend *miner.PebbleStoreBackend
	kv      *kv.Pebble
	broker  *pebblequeue.Broker
	srv     *httptest.Server
	lastID  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
	require.NoError(t, err)
	kb := redisutil.NewKeyBuilder(config.RedisNamespaceConfig{})
	store := kv.NewPebble(zerolog.Nop(), db, kb)
	broker := pebblequeue.NewBroker(zerolog.Nop(), db, store, kb.StreamPrefix())
	backend := miner.NewPebbleStoreBackend(zerolog.Nop(), db, broker, miner.SupplierManagerConfig{BlockTimeSeconds: 30})
	f := &fixture{t: t, backend: backend, kv: store, broker: broker}
	server := New(zerolog.Nop(), "127.0.0.1:0", Sources{
		Miner:  func() Miner { return backend },
		Queues: broker,
		KV:     store,
	})
	f.srv = httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		f.srv.Close()
		_ = store.Close()
		_ = db.Close()
	})
	f.seed()
	return f
}

func (f *fixture) seed() {
	t, ctx, kb := f.t, context.Background(), f.kv.KB()
	for id, state := range map[string]miner.SessionState{"s1": miner.SessionStateActive, "s2": miner.SessionStateClaimTxError} {
		require.NoError(t, f.backend.SeedSessionForTest(supplierA, &miner.SessionSnapshot{
			SessionID: id, SupplierOperatorAddress: supplierA, ServiceID: "svc", State: state,
		}))
	}
	require.NoError(t, f.backend.SeedDedupForTest("s1", []byte{0xab, 0xcd}))
	require.NoError(t, f.backend.SeedLiveRootForTest(supplierA, "s1", []byte{0xee}))
	require.NoError(t, f.kv.Set(ctx, kb.SupplierStateKey(supplierA), []byte(`{"status":"active","staked":true,"services":["svc"]}`), 0))
	require.NoError(t, f.kv.Set(ctx, kb.MeterMetaKey("s1", supplierA), []byte(`{"session_id":"s1","supplier_address":"`+supplierA+`"}`), 0))
	require.NoError(t, f.kv.Set(ctx, kb.MeterConsumedKey("s1", supplierA), []byte("42"), 0))
	require.NoError(t, f.kv.Set(ctx, kb.TxTrackKey(supplierA, 100, "s1"), []byte(`{"supplier":"`+supplierA+`","session_id":"s1"}`), 0))
	pub := f.broker.Publisher(0)
	require.NoError(t, pub.Publish(ctx, &transport.MinedRelayMessage{
		SupplierOperatorAddress: supplierA, ServiceId: "svc", SessionId: "s1", SessionEndHeight: 100, RelayBytes: []byte("r"),
	}))
	st, err := f.broker.Stats(supplierA)
	require.NoError(t, err)
	require.NotEmpty(t, st.LastID, "premise: the relay was queued")
	f.lastID = st.LastID
}

// get fetches path and decodes a 200 answer into out; it returns the status.
func (f *fixture) get(path string, out any) int {
	f.t.Helper()
	resp, err := http.Get(f.srv.URL + path)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	if resp.StatusCode == http.StatusOK && out != nil {
		require.NoError(f.t, json.Unmarshal(body, out), string(body))
	}
	return resp.StatusCode
}

func sessionIDs(sessions []map[string]string) []string {
	var ids []string
	for _, s := range sessions {
		ids = append(ids, s["session_id"])
	}
	return ids
}

// Sessions are read per supplier, narrowed by state or by session, as the
// fields `redis sessions --json` prints.
func TestServer_Sessions(t *testing.T) {
	f := newFixture(t)
	var all, failed, one, none []map[string]string
	require.Equal(t, http.StatusOK, f.get(PathSessions+"?supplier="+supplierA, &all))
	require.ElementsMatch(t, []string{"s1", "s2"}, sessionIDs(all))

	require.Equal(t, http.StatusOK, f.get(PathSessions+"?supplier="+supplierA+"&state="+string(miner.SessionStateClaimTxError), &failed))
	require.Equal(t, []string{"s2"}, sessionIDs(failed))
	require.Equal(t, string(miner.SessionStateClaimTxError), failed[0]["state"])
	require.Equal(t, supplierA, failed[0]["supplier_operator_address"])

	require.Equal(t, http.StatusOK, f.get(PathSessions+"?supplier="+supplierA+"&session=s1", &one))
	require.Equal(t, []string{"s1"}, sessionIDs(one))

	require.Equal(t, http.StatusOK, f.get(PathSessions+"?supplier="+supplierB, &none))
	require.NotNil(t, none, "an empty array, not null")
	require.Empty(t, none)

	require.Equal(t, http.StatusBadRequest, f.get(PathSessions, nil), "a supplier is required")
}

func TestServer_SuppliersStreamsSMSTDedupMeterSubmissions(t *testing.T) {
	f := newFixture(t)
	kb := f.kv.KB()

	var suppliers map[string]map[string]any
	require.Equal(t, http.StatusOK, f.get(PathSuppliers, &suppliers))
	require.Equal(t, map[string]map[string]any{supplierA: {"status": "active", "staked": true, "services": []any{"svc"}}}, suppliers)

	var streams []pebblequeue.QueueStats
	require.Equal(t, http.StatusOK, f.get(PathStreams+"?supplier="+supplierA, &streams))
	require.Equal(t, []pebblequeue.QueueStats{{Supplier: supplierA, Stream: transport.SupplierStreamName(kb.StreamPrefix(), supplierA), Length: 1, LastID: f.lastID}}, streams)
	var allStreams []pebblequeue.QueueStats
	require.Equal(t, http.StatusOK, f.get(PathStreams, &allStreams))
	require.Equal(t, streams, allStreams)

	var trees []miner.SMSTView
	require.Equal(t, http.StatusOK, f.get(PathSMST+"?session=s1", &trees))
	require.Equal(t, []miner.SMSTView{{Supplier: supplierA, SessionID: "s1", LiveRoot: "ee"}}, trees)
	require.Equal(t, http.StatusBadRequest, f.get(PathSMST, nil), "a session is required")

	var dedup miner.DedupView
	require.Equal(t, http.StatusOK, f.get(PathDedup+"?session=s1", &dedup))
	require.Equal(t, "s1", dedup.SessionID)
	require.Equal(t, 1, dedup.Count)
	require.Equal(t, []string{"abcd"}, dedup.Sample)
	require.True(t, dedup.Live)

	var meters []MeterEntry
	require.Equal(t, http.StatusOK, f.get(PathMeter+"?session=s1", &meters))
	require.Len(t, meters, 1)
	require.Equal(t, kb.MeterMetaKey("s1", supplierA), meters[0].Key)
	require.JSONEq(t, `{"session_id":"s1","supplier_address":"`+supplierA+`"}`, string(meters[0].Meta))
	require.NotNil(t, meters[0].ConsumedUpokt)
	require.Equal(t, "42", *meters[0].ConsumedUpokt)
	var meterKeys []string
	require.Equal(t, http.StatusOK, f.get(PathMeter, &meterKeys))
	require.Equal(t, []string{kb.MeterConsumedKey("s1", supplierA), kb.MeterMetaKey("s1", supplierA)}, meterKeys)

	var subs, otherSubs []map[string]any
	require.Equal(t, http.StatusOK, f.get(PathSubmissions, &subs))
	require.Equal(t, []map[string]any{{"supplier": supplierA, "session_id": "s1"}}, subs)
	require.Equal(t, http.StatusOK, f.get(PathSubmissions+"?supplier="+supplierB, &otherSubs))
	require.NotNil(t, otherSubs)
	require.Empty(t, otherSubs)
}

// The server only reads: any method but GET is refused, on every route.
func TestServer_RefusesEveryMethodButGet(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{PathHealth, PathSessions, PathSuppliers, PathStreams, PathSMST, PathDedup, PathMeter, PathSubmissions} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req, err := http.NewRequest(method, f.srv.URL+path+"?supplier="+supplierA+"&session=s1", strings.NewReader("{}"))
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "%s %s", method, path)
		}
	}
}

// Until the miner has built its state, its routes answer 503, never an empty
// list a caller would read as "no sessions".
func TestServer_MinerRoutesWaitForTheMiner(t *testing.T) {
	srv := httptest.NewServer(New(zerolog.Nop(), "127.0.0.1:0", Sources{Miner: func() Miner { return nil }}).Handler())
	defer srv.Close()
	for _, path := range []string{PathSessions + "?supplier=" + supplierA, PathSMST + "?session=s1", PathDedup + "?session=s1"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, path)
	}
}

type failingKV struct{ kb *redisutil.KeyBuilder }

var errStore = errors.New("store unreadable")

func (f failingKV) KB() *redisutil.KeyBuilder                          { return f.kb }
func (failingKV) Get(context.Context, string) ([]byte, error)          { return nil, errStore }
func (failingKV) ScanPrefix(context.Context, string) ([]string, error) { return nil, errStore }

// A store that cannot be read answers 500: an error is never an empty answer.
func TestServer_AnUnreadableStoreIsAnError(t *testing.T) {
	srv := httptest.NewServer(New(zerolog.Nop(), "127.0.0.1:0", Sources{
		KV: failingKV{kb: redisutil.NewKeyBuilder(config.RedisNamespaceConfig{})},
	}).Handler())
	defer srv.Close()
	for _, path := range []string{PathSuppliers, PathMeter, PathMeter + "?session=s1", PathSubmissions} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode, path)
	}
}

// Start listens before it returns, so a taken port is a startup error, and
// Stop is idempotent.
func TestServer_StartFailsOnATakenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	err = New(zerolog.Nop(), ln.Addr().String(), Sources{}).Start()
	require.Error(t, err)

	s := New(zerolog.Nop(), "127.0.0.1:0", Sources{})
	require.NoError(t, s.Start())
	s.Stop()
	s.Stop()
}
