//go:build test

package miner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const redisOOMReplyText = "OOM command not allowed when used memory > 'maxmemory'."

// failingStateStore answers UpdateState with the error set for the session.
type failingStateStore struct {
	SessionStore
	mu      sync.Mutex
	errs    map[string]error
	updated []string
}

func (s *failingStateStore) UpdateState(_ context.Context, sessionID string, _ SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updated = append(s.updated, sessionID)
	return s.errs[sessionID]
}

func TestPersistProving_AnOOMStillSubmitsTheProofAndOtherErrorsSkip(t *testing.T) {
	store := &failingStateStore{errs: map[string]error{
		"oom":   errors.New(redisOOMReplyText),
		"other": errors.New("connection reset"),
	}}
	m := &SessionLifecycleManager{
		logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sessionStore: store,
		config:       SessionLifecycleConfig{SupplierAddress: "pokt1proving"},
	}
	valid := m.persistProving(context.Background(), []*SessionSnapshot{
		{SessionID: "oom"}, {SessionID: "other"}, {SessionID: "ok"},
	})
	var ids []string
	for _, s := range valid {
		ids = append(ids, s.SessionID)
	}
	require.Equal(t, []string{"oom", "ok"}, ids,
		"LINK proving-oom: a proving write refused for memory still sends the proof; any other failure still skips it")
}

func TestExecuteBatchedProofTransition_AProvedWriteThatFailsStillFreesTheTree(t *testing.T) {
	store := &failingStateStore{errs: map[string]error{"a": errors.New(redisOOMReplyText)}}
	cb := &partialProofCallback{settle: []string{"a"}}
	m := &SessionLifecycleManager{
		logger:         logging.NewLoggerFromConfig(logging.DefaultConfig()),
		sessionStore:   store,
		callback:       cb,
		config:         SessionLifecycleConfig{SupplierAddress: "pokt1proved"},
		activeSessions: xsync.NewMap[string, *SessionSnapshot](),
	}
	session := &SessionSnapshot{SessionID: "a", State: SessionStateProving}
	m.activeSessions.Store("a", session)

	m.executeBatchedProofTransition(context.Background(), []*SessionSnapshot{session})

	require.Equal(t, []string{"a"}, cb.prove,
		"LINK proved-free: the proof is out, so the cleanup that deletes the tree runs even when the proved write fails")
	_, stillActive := m.activeSessions.Load("a")
	require.False(t, stillActive)
}

// The tracker's writes are covered by submission_tracker_brake_test.go: what
// used to be asserted here -- that a closed store writes no record -- is the
// defect that lost 250 claim records, so the assertion is inverted there.

// TestStoreMemory_TellsStreamsFromTrees is the measurement behind "a store full of
// stream backlog stays full while the miner is paused": what it attributes to
// streams is freed only by consuming, which the pause stops, and what it
// attributes to SMSTs is freed by DeleteTree after a proof.
func TestStoreMemory_TellsStreamsFromTrees(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	kb := client.KB()
	payload := make([]byte, 64<<10)
	for i := 0; i < 20; i++ {
		require.NoError(t, client.XAdd(ctx, &redis.XAddArgs{Stream: kb.StreamKey("pokt1memstream"), Values: map[string]any{"data": payload}}).Err())
	}
	require.NoError(t, client.HSet(ctx, kb.SMSTNodesKey("pokt1memtree", "sess"), "n", make([]byte, 256<<10)).Err())

	streams, smst, used, err := measureStoreMemory(ctx, client)
	require.NoError(t, err)
	require.Greater(t, streams, uint64(20*64<<10), "the stream backlog is attributed to streams")
	require.Greater(t, smst, uint64(256<<10), "the nodes hash is attributed to SMSTs")
	require.Less(t, smst, streams, "control: the two families are not summed together")
	require.Greater(t, used, streams)

	require.NoError(t, client.Del(ctx, kb.SMSTNodesKey("pokt1memtree", "sess")).Err())
	_, smstAfter, _, err := measureStoreMemory(ctx, client)
	require.NoError(t, err)
	require.Zero(t, smstAfter, "deleting the tree, as a proof does, frees the SMST share")
	streamsAfter, _, _, err := measureStoreMemory(ctx, client)
	require.NoError(t, err)
	require.Equal(t, streams, streamsAfter, "and leaves the stream backlog, which only consuming frees")
}

func TestRecordStoreMemoryOnClose_MeasuresWhenTheStoreCloses(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	require.NoError(t, client.HSet(ctx, client.KB().SMSTNodesKey("pokt1onclose", "sess"), "n", make([]byte, 128<<10)).Err())
	health := redistransport.NewStoreHealth(zerolog.Nop(), client.UniversalClient, "test_on_close", redistransport.StoreGateIngestion)
	storeMemoryAtClose.WithLabelValues("smst").Set(0)
	RecordStoreMemoryOnClose(ctx, zerolog.Nop(), client, health)

	health.ReportOOM()
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(storeMemoryAtClose.WithLabelValues("smst")) >= float64(128<<10)
	}, 10*time.Second, 10*time.Millisecond, "LINK memory-on-close: closing the store measures what Redis holds")
}

func TestRecordStoreMemoryOnClose_LogsEveryFamily(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	require.NoError(t, client.HSet(ctx, client.KB().SMSTNodesKey("pokt1logfamily", "sess"), "n", make([]byte, 64<<10)).Err())
	var buf syncBuffer
	health := redistransport.NewStoreHealth(zerolog.Nop(), client.UniversalClient, "test_log_family", redistransport.StoreGateIngestion)
	RecordStoreMemoryOnClose(ctx, zerolog.New(&buf), client, health)

	health.ReportOOM()
	require.Eventually(t, func() bool { return strings.Contains(buf.String(), "Redis memory at store close") }, 10*time.Second, 10*time.Millisecond)
	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &line))
	for _, key := range []string{"stream_bytes", "smst_bytes", "other_bytes", "used_memory"} {
		require.Contains(t, line, key, "LINK log-family: the close measurement logs %s", key)
	}
}
