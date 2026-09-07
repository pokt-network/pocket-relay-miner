//go:build test

package miner

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// A panic inside a reconcile task is recovered by pond and delivered as the
// error of group.Wait(). That error used to be discarded, so the panic left no
// trace at all: no log, no metric, no crash. The pass still "succeeds" and the
// entry stays pending, which is exactly what makes the loss invisible.
//
// The panic is injected through the phase's onChainSessions hook rather than by
// submitting a panicking task to a fresh pool: a fresh pool would prove that
// pond recovers panics, which is pond's property, not ours. Going through
// runPass exercises the line under test.
func TestInclusionReconciler_PanicInPassTaskIsCountedAndLogged(t *testing.T) {
	rc, _ := newTestRedis(t)
	store := NewRebroadcastStore(rc, time.Hour)

	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:     []byte("session-1"),
		SubmitHeight: testSubmit,
		TxHash:       "hash-1",
		OrigTxHash:   "hash-1",
	})
	require.NoError(t, err)
	require.NoError(t, store.Put(context.Background(), RebroadcastPhaseClaim, "supplier-1", 100, "session-1", b))

	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.TraceLevel)

	panicking := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return testWindowClose },
			onChainSessions: func(_ context.Context, _ string) (map[string]struct{}, error) {
				panic("injected: inclusion query blew up")
			},
			recordOutcome: func(_ context.Context, _ rebroadcastEntry, _ string, _ int64, _, _ string, _ int64) error {
				return nil
			},
			recordRebroadcast: func(_, _, _ string) {},
		}
	}

	cfg := DefaultInclusionReconcilerConfig()
	cfg.MaxConcurrent = 2
	cfg.PerGroupTimeout = 2 * time.Second

	r := NewInclusionReconciler(logger, &mockSharedQueryClient{}, store, &mockResubmitter{},
		panicking(RebroadcastPhaseClaim), panicking(RebroadcastPhaseProof), cfg)
	t.Cleanup(func() { _ = r.Close() })

	before := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("inclusion_reconcile_group"))
	r.OnBlock(testMid)
	after := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("inclusion_reconcile_group"))
	require.Greater(t, after, before,
		"the repo's convention is that a recovered panic is COUNTED and logged; the log alone leaves it out of alerting")

	out := buf.String()
	require.Contains(t, out, "a pass task panicked",
		"a panic inside a reconcile task must be logged; without it the panic leaves no trace at all")
	require.Contains(t, out, `"level":"error"`,
		"a recovered panic needs immediate attention, so it must not be logged below Error")
}

// The trim pool is the one site of the three whose tasks go in via SubmitErr, so
// its Wait can carry either a recovered panic or a task error. This exercises the
// panic half, through the REAL TrimStream: a zero-value consumer dereferences a
// nil client, which is the shape of bug the discarded error used to hide.
func TestSupplierManager_PanicInTrimTaskIsCountedAndLogged(t *testing.T) {
	var buf bytes.Buffer

	suppliers := xsync.NewMap[string, *SupplierState]()
	suppliers.Store("supplier-1", &SupplierState{Consumer: &redistransport.StreamsConsumer{}})

	m := &SupplierManager{
		logger:       zerolog.New(&buf).Level(zerolog.TraceLevel),
		suppliers:    suppliers,
		querySubpool: pond.NewPool(2),
	}
	t.Cleanup(m.querySubpool.StopAndWait)

	before := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("supplier_stream_trim"))
	m.trimAllSupplierStreams(context.Background(), time.Hour)
	after := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("supplier_stream_trim"))
	require.Greater(t, after, before,
		"the repo's convention is that a recovered panic is COUNTED and logged; the log alone leaves it out of alerting")

	out := buf.String()
	require.Contains(t, out, "a trim task panicked",
		"a panic inside a trim task must be logged; discarding Wait's error left it with no trace at all")
	require.Contains(t, out, `"level":"error"`,
		"a recovered panic needs immediate attention, so it must not be logged below Error")
}

// The shutdown half, and it reproduces the REAL window rather than an analogy.
// Close() takes the mutex, sets closed = true and only then calls StopAndWait,
// so a pass that already read closed == false can be submitting while the pool
// goes down. Stopping the pool directly, without setting closed, IS that state:
// OnBlock walks past its guard and pond answers the Submits with ErrPoolStopped.
//
// Raising that at Error would spend, on every rollout landing in this window,
// exactly the signal the panic branch exists to create.
func TestInclusionReconciler_PoolStoppedIsNotReportedAsAPanic(t *testing.T) {
	rc, _ := newTestRedis(t)
	store := NewRebroadcastStore(rc, time.Hour)

	b, err := marshalRebroadcastEntry(rebroadcastEntry{
		MsgBytes:     []byte("session-1"),
		SubmitHeight: testSubmit,
		TxHash:       "hash-1",
		OrigTxHash:   "hash-1",
	})
	require.NoError(t, err)
	require.NoError(t, store.Put(context.Background(), RebroadcastPhaseClaim, "supplier-1", 100, "session-1", b))

	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.TraceLevel)

	phase := func(p RebroadcastPhase) reconcilePhase {
		return reconcilePhase{
			phase:             p,
			windowCloseHeight: func(_ *sharedtypes.Params, _ int64) int64 { return testWindowClose },
			onChainSessions: func(_ context.Context, _ string) (map[string]struct{}, error) {
				return map[string]struct{}{}, nil
			},
			recordOutcome: func(_ context.Context, _ rebroadcastEntry, _ string, _ int64, _, _ string, _ int64) error {
				return nil
			},
			recordRebroadcast: func(_, _, _ string) {},
		}
	}

	cfg := DefaultInclusionReconcilerConfig()
	cfg.MaxConcurrent = 2
	cfg.PerGroupTimeout = 2 * time.Second

	r := NewInclusionReconciler(logger, &mockSharedQueryClient{}, store, &mockResubmitter{},
		phase(RebroadcastPhaseClaim), phase(RebroadcastPhaseProof), cfg)
	t.Cleanup(func() { _ = r.Close() })

	// The window: pool down, r.closed still false, so OnBlock does NOT return early.
	r.pool.StopAndWait()

	before := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("inclusion_reconcile_group"))
	r.OnBlock(testMid)
	after := testutil.ToFloat64(logging.PanicRecoveriesTotal.WithLabelValues("inclusion_reconcile_group"))

	require.Equal(t, before, after,
		"a stopped pool is not a panic; counting it would make the panic counter unusable as an alert")

	out := buf.String()
	require.NotContains(t, out, `"level":"error"`,
		"a pool stopped during shutdown must not raise Error -- that is the rollout noise this branch exists to avoid")
	require.Contains(t, out, "shutting down",
		"the abandonment must still be visible at Debug: silence and success would look identical")
}
