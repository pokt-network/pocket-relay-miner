//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// sessionReadCounter counts SessionStore.Get calls for ONE session by counting
// the HGETALL each Get issues against that session's key (session_store.go
// getHash), on the real Redis the fixture uses. INFO commandstats would count
// the whole server, which other packages share while `go test ./...` runs, so a
// hook on this test's own client is the only count that is this test's alone.
//
// It used to count the TYPE that Get opened with, until Get stopped asking TYPE.
// HGETALL is the honest replacement for BOTH halves of this hook -- the count
// and the failure injection -- because exactly one HGETALL reaches this key per
// Get: getHash is its only caller (the legacy fallback issues GET, not a second
// HGETALL), CreateIfAbsent writes through a TxPipeline, which lands in
// ProcessPipelineHook rather than here, and the other HGETALL in this package
// (rebroadcast_store.go) uses a different key, which the key filter drops.
type sessionReadCounter struct {
	key string

	reads atomic.Int64
	// failNext makes that many of the next reads fail, as a Redis blip would.
	failNext atomic.Int64
}

func countSessionReads(t *testing.T, f *handlerTestFixture, sessionID string) *sessionReadCounter {
	t.Helper()
	c := &sessionReadCounter{key: f.sessionStore.sessionKey(sessionID)}
	f.redisClient.AddHook(c)
	return c
}

func (c *sessionReadCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *sessionReadCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() == "hgetall" && len(args) == 2 && fmt.Sprint(args[1]) == c.key {
			c.reads.Add(1)
			if c.failNext.Load() > 0 {
				c.failNext.Add(-1)
				return errors.New("injected: session read failed")
			}
		}
		return next(ctx, cmd)
	}
}

func (c *sessionReadCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestHandleRelay_ReadsAnExistingSessionOnce pins that an existing session is
// read once, not once per relay: the first relay's read confirms it exists, the
// answer stays on the session's tree, and the relays after it skip the HGETALL.
func TestHandleRelay_ReadsAnExistingSessionOnce(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1read_once")
	const (
		sessionID = "sess-read-once"
		relays    = 5
		cu        = uint64(100)
	)

	// The session exists before the relays arrive, as it does for every relay
	// but a session's first. Heights match newStreamMessage.
	require.NoError(t, f.coordinator.OnSessionCreated(f.ctx, sessionID,
		f.supplierAddr, "svc-1", "pokt1app", 1, 10))

	reads := countSessionReads(t, f, sessionID)
	for i := 0; i < relays; i++ {
		msg := newStreamMessage(f.supplierAddr, sessionID, fmt.Sprintf("relay-%d", i), cu)
		require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msg))
	}

	require.Equal(t, int64(1), reads.reads.Load(),
		"%d relays read session %s %d times: the first relay's read confirms it exists, "+
			"and the relays after it must not read it again", relays, sessionID, reads.reads.Load())

	snap, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.Equal(t, int64(relays), snap.RelayCount, "every relay must still be counted")
	require.Equal(t, uint64(relays)*cu, snap.TotalComputeUnits)
}

// TestHandleRelay_FirstRelayOfANewSessionStillCreatesIt pins the other side of
// the handed-over read: an answered read that found nothing means the session
// does not exist, and the first relay creates it -- once, with one
// session-created callback, which production wires to TrackSession.
func TestHandleRelay_FirstRelayOfANewSessionStillCreatesIt(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1first_relay")
	const sessionID = "sess-first-relay"

	var created atomic.Int32
	f.coordinator.SetOnSessionCreatedCallback(func(_ context.Context, snap *SessionSnapshot) error {
		require.Equal(t, sessionID, snap.SessionID)
		created.Add(1)
		return nil
	})
	reads := countSessionReads(t, f, sessionID)

	first := newStreamMessage(f.supplierAddr, sessionID, "relay-first", 100)
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, first))

	snap, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, snap, "the first relay of a session must create it, or nothing claims its tree")
	require.Equal(t, f.supplierAddr, snap.SupplierOperatorAddress)
	require.Equal(t, "svc-1", snap.ServiceID)
	require.Equal(t, "pokt1app", snap.ApplicationAddress)
	require.Equal(t, int64(1), snap.SessionStartHeight)
	require.Equal(t, int64(10), snap.SessionEndHeight)
	require.Equal(t, SessionStateActive, snap.State)
	require.Equal(t, int64(1), snap.RelayCount)
	require.Equal(t, int32(1), created.Load(), "the session-created callback must fire once")
	// One read by EnsureSession, one by the Get above.
	require.Equal(t, int64(2), reads.reads.Load(),
		"an answered read that found nothing must go straight to the create, not read again")

	second := newStreamMessage(f.supplierAddr, sessionID, "relay-second", 100)
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, second))
	require.Equal(t, int32(1), created.Load(), "a second relay must not create the session again")
}

// TestHandleRelay_AFailedSessionReadIsNotTakenAsAbsent pins what EnsureSession
// does when its own read fails: it has learned nothing about the session, so it
// goes on to CreateIfAbsent -- it neither assumes the session exists (and skips
// creating it) nor drops the relay.
func TestHandleRelay_AFailedSessionReadIsNotTakenAsAbsent(t *testing.T) {
	f := newHandlerTestFixture(t, "pokt1failed_read")
	const sessionID = "sess-failed-read"

	var created atomic.Int32
	f.coordinator.SetOnSessionCreatedCallback(func(_ context.Context, _ *SessionSnapshot) error {
		created.Add(1)
		return nil
	})
	reads := countSessionReads(t, f, sessionID)
	reads.failNext.Store(1) // EnsureSession's read fails; nothing after it does

	msg := newStreamMessage(f.supplierAddr, sessionID, "relay-after-blip", 100)
	require.NoError(t, f.worker.handleRelay(f.ctx, f.supplierAddr, msg),
		"a failed session read is not a reason to drop the relay")

	require.Equal(t, int64(0), reads.failNext.Load(),
		"the injected failure must have been applied to a real command: if the hook matches "+
			"nothing, this test proves nothing")
	require.Equal(t, int64(1), reads.reads.Load(),
		"the one read of %s is the one that failed: the create below proves the failure "+
			"was not taken as 'exists'", sessionID)

	snap, err := f.sessionStore.Get(f.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, snap, "the session must be created: a failed read taken as 'exists' leaves the tree unclaimed")
	require.Equal(t, int64(1), snap.RelayCount)
	require.Equal(t, int32(1), created.Load())
}
