package relay_client

import (
	"sync"
	"testing"

	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
)

// newTestSession builds a minimal session carrying only the fields the cache
// reads: the service it belongs to and the height at which it stops being valid.
func newTestSession(serviceID, sessionID string, endHeight int64) *sessiontypes.Session {
	return &sessiontypes.Session{
		Header: &sessiontypes.SessionHeader{
			ServiceId:             serviceID,
			SessionId:             sessionID,
			SessionEndBlockHeight: endHeight,
		},
	}
}

// TestCachedSessionFor_ServesTheSessionForTheSameService proves the read path
// returns what was stored, so callers within a session never query the chain.
func TestCachedSessionFor_ServesTheSessionForTheSameService(t *testing.T) {
	c := &RelayClient{}
	stored := newTestSession("develop-http", "session-a", 300)

	c.ReplaceCachedSession(stored)

	got := c.cachedSessionFor("develop-http")
	require.NotNil(t, got, "a session stored for this service must be served from cache")
	require.Same(t, stored, got, "the cache must serve the exact session that was stored")
	require.Equal(t, "session-a", got.Header.SessionId)
	require.Equal(t, int64(300), got.Header.SessionEndBlockHeight)
}

// TestCachedSessionFor_MissesForADifferentService proves the cache is scoped by
// service: asking for another service must miss rather than hand back a session
// whose suppliers belong to a different one.
func TestCachedSessionFor_MissesForADifferentService(t *testing.T) {
	c := &RelayClient{}
	c.ReplaceCachedSession(newTestSession("develop-http", "session-a", 300))

	require.Nil(t, c.cachedSessionFor("develop-websocket"),
		"a session cached for one service must not be served for another")
}

// TestCachedSessionFor_MissesWhenEmpty proves the zero value is a miss and not
// a nil dereference.
func TestCachedSessionFor_MissesWhenEmpty(t *testing.T) {
	c := &RelayClient{}
	require.Nil(t, c.cachedSessionFor("develop-http"))
}

// TestReplaceCachedSession_SwapsInOneStep is the rollover case. The old session
// must never be observable once the new one is in, and the cache must never be
// empty in between: an empty cache at a session boundary is what let a stale
// session be re-fetched and re-cached under load.
func TestReplaceCachedSession_SwapsInOneStep(t *testing.T) {
	c := &RelayClient{}
	c.ReplaceCachedSession(newTestSession("develop-http", "session-a", 300))

	c.ReplaceCachedSession(newTestSession("develop-http", "session-b", 310))

	got := c.cachedSessionFor("develop-http")
	require.NotNil(t, got)
	require.Equal(t, "session-b", got.Header.SessionId, "the new session must replace the old one")
	require.Equal(t, int64(310), got.Header.SessionEndBlockHeight)
}

// TestClearSessionCache_Empties proves the explicit clear still works for the
// callers that want the next relay to go to chain.
func TestClearSessionCache_Empties(t *testing.T) {
	c := &RelayClient{}
	c.ReplaceCachedSession(newTestSession("develop-http", "session-a", 300))

	c.ClearSessionCache()

	require.Nil(t, c.cachedSessionFor("develop-http"))
}

// TestSessionCache_ConcurrentAccess runs the production shape under the race
// detector: many relay workers reading the cached session while the rollover
// monitor swaps it. Without a lock on the field this is a data race, and the
// load-test path does exactly this.
func TestSessionCache_ConcurrentAccess(t *testing.T) {
	const (
		readers      = 32
		readsEach    = 200
		swaps        = 200
		serviceID    = "develop-http"
		firstSession = "session-a"
	)

	c := &RelayClient{}
	c.ReplaceCachedSession(newTestSession(serviceID, firstSession, 300))

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < swaps; i++ {
			c.ReplaceCachedSession(newTestSession(serviceID, "session-b", int64(310+i)))
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < readsEach; i++ {
				// The cache is never emptied by the swapper, so a reader must
				// always see a usable session for this service.
				got := c.cachedSessionFor(serviceID)
				require.NotNil(t, got)
				require.Equal(t, serviceID, got.Header.ServiceId)
			}
		}()
	}

	wg.Wait()
}

// TestClearSessionCache_ConcurrentWithReads covers the other writer: an explicit
// clear racing readers. Readers must tolerate the miss, not tear.
func TestClearSessionCache_ConcurrentWithReads(t *testing.T) {
	const readers = 16

	c := &RelayClient{}
	c.ReplaceCachedSession(newTestSession("develop-http", "session-a", 300))

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.ClearSessionCache()
			c.ReplaceCachedSession(newTestSession("develop-http", "session-a", 300))
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if got := c.cachedSessionFor("develop-http"); got != nil {
					require.Equal(t, "develop-http", got.Header.ServiceId)
				}
			}
		}()
	}

	wg.Wait()
}
