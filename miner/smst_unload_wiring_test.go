//go:build test

package miner

import (
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/stretchr/testify/require"
)

func TestConsumeLoop_UnloadsTheTreeOfASessionPastItsGracePeriodAfterItsRelaysAreFlushed(t *testing.T) {
	const supplier, session = "pokt1unload_wiring", "sess-wired"
	f := newPanicLoopFixture(t, supplier, func(int32) {})

	// newStreamMessage relays belong to a session that ends at 10.
	// The heights come from the params the manager reads.
	params := unloadParams()
	const sessionEnd = 10
	height := sharedtypes.GetSessionGracePeriodEndHeight(params, sessionEnd) + 1
	require.Less(t, height, sharedtypes.GetClaimWindowOpenHeight(params, sessionEnd),
		"premise: before its claim window the tree is unloaded")
	require.Less(t, height, sharedtypes.GetClaimWindowOpenHeight(params, sessionEnd)+claimFlushCapBlocks,
		"premise: past its grace period, its relays are still taken")
	f.w.mgr.config.BlockClient = &mockBlockClient{currentHeight: height}
	f.w.mgr.config.SharedClient = &mockSharedQueryClient{params: params}
	sessions := xsync.NewMap[string, *SessionSnapshot]()
	sessions.Store(session, &SessionSnapshot{SessionID: session, SupplierOperatorAddress: supplier, SessionEndHeight: sessionEnd})
	f.w.state.LifecycleManager = &SessionLifecycleManager{activeSessions: sessions}
	before := testutil.ToFloat64(smstTreesUnloaded.WithLabelValues(supplier))

	f.start(t)
	id := f.addRelay(t, session, "unload-wiring-relay")
	deadline := time.After(30 * time.Second)
	for processed := false; !processed; {
		select {
		case got := <-f.processed:
			processed = got == id
		case <-deadline:
			t.Fatal("the consume loop never processed the relay")
		}
	}
	for testutil.ToFloat64(smstTreesUnloaded.WithLabelValues(supplier)) == before {
		select {
		case <-deadline:
			t.Fatal("LINK unload-wired: the consume loop unloads, after a flush, the tree of a session past its grace period")
		case <-time.After(time.Millisecond):
		}
	}
	require.Equal(t, before+1, testutil.ToFloat64(smstTreesUnloaded.WithLabelValues(supplier)), "once: the unloaded tree is not imported again at the next flush")
	require.True(t, isUnloaded(t, f.w.state.SMSTManager, session), "LINK unload-wired: the tree is out of memory")
}
