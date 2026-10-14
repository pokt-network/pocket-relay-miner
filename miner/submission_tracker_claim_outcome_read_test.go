//go:build test

package miner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// trackerStores runs body over the tracker's kv on both backends.
func trackerStores(t *testing.T, body func(t *testing.T, store kv.Store)) {
	t.Run("redis", func(t *testing.T) {
		client, _ := newTestRedis(t)
		body(t, kv.NewRedis(zerolog.Nop(), client))
	})
	t.Run("pebble", func(t *testing.T) {
		db, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		body(t, kv.NewPebble(zerolog.Nop(), db, redisutil.NewKeyBuilder(config.RedisNamespaceConfig{})))
	})
}

// expiresOnScan is a store in which one key expires the moment the supplier's
// prefix is listed: the TTL edge, where a record is listed and gone by the read.
type expiresOnScan struct {
	kv.Store
	expire string
}

func (s *expiresOnScan) ScanPrefix(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.Store.ScanPrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if delErr := s.Del(ctx, s.expire); delErr != nil {
		return nil, delErr
	}
	return keys, nil
}

// failsReads is a store whose reads fail while its writes work.
type failsReads struct{ kv.Store }

var errReadInjected = errors.New("injected read failure")

func (s *failsReads) Get(context.Context, string) ([]byte, error) { return nil, errReadInjected }

func (s *failsReads) MGet(context.Context, ...string) ([][]byte, error) {
	return nil, errReadInjected
}

// An outcome for one session does not read the records of the supplier's other
// sessions, so another session's record expiring at that moment is not a
// failed read.
func TestUpdateClaimOnChainOutcome_AnotherSessionExpiringIsNotAFailedRead(t *testing.T) {
	trackerStores(t, func(t *testing.T, store kv.Store) {
		ctx := context.Background()
		const supplier = "pokt1edge"
		seedClaim(t, NewSubmissionTracker(zerolog.Nop(), store, time.Hour), supplier, "sess-old", "0xtx-old")
		seedClaim(t, NewSubmissionTracker(zerolog.Nop(), store, time.Hour), supplier, "sess-new", "0xtx-new")

		buf := &syncBuf{}
		edge := &expiresOnScan{Store: store, expire: store.KB().TxTrackKey(supplier, 110, "sess-old")}
		tr := NewSubmissionTracker(zerolog.New(buf).Level(zerolog.TraceLevel), edge, time.Hour)

		require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
			Supplier: supplier, SessionEnd: 110, SessionID: "sess-new", TxHash: "0xtx-new",
			Outcome: "on_chain_found", InclusionHeight: 111,
		}))
		got, err := tr.GetRecord(ctx, supplier, 110, "sess-new")
		require.NoError(t, err)
		require.Equal(t, "on_chain_found", got.ClaimOnChainOutcome, "CONTROL: the outcome reached its record")
		require.NotContains(t, buf.String(), "failed to get record",
			"another session's record expiring is not a failed read of this one")

		// CONTROL: the logger is wired, so the absence above is evidence.
		require.NoError(t, tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
			Supplier: supplier, SessionEnd: 110, SessionID: "sess-nobody", TxHash: "0xtx-nobody",
			Outcome: "on_chain_found", InclusionHeight: 111,
		}))
		require.True(t, strings.Contains(buf.String(), "found no submission record"),
			"CONTROL: an outcome with no record logs to the same buffer")
	})
}

// A read that fails is not a missing record: it reaches the caller and is not
// counted as an outcome without a record.
func TestUpdateClaimOnChainOutcome_AFailedReadIsReturnedNotCountedMissing(t *testing.T) {
	trackerStores(t, func(t *testing.T, store kv.Store) {
		ctx := context.Background()
		const supplier = "pokt1readfail"
		seedClaim(t, NewSubmissionTracker(zerolog.Nop(), store, time.Hour), supplier, "sess-a", "0xtx-a")

		tr := NewSubmissionTracker(zerolog.Nop(), &failsReads{Store: store}, time.Hour)
		missed := testutil.ToFloat64(trackingOutcomesWithoutRecord.WithLabelValues("claim"))

		err := tr.UpdateClaimOnChainOutcome(ctx, ClaimOnChainUpdate{
			Supplier: supplier, SessionEnd: 110, SessionID: "sess-a", TxHash: "0xtx-a",
			Outcome: "on_chain_found", InclusionHeight: 111,
		})
		require.ErrorIs(t, err, errReadInjected)
		require.Equal(t, missed, testutil.ToFloat64(trackingOutcomesWithoutRecord.WithLabelValues("claim")),
			"a failed read is not an outcome without a record")
	})
}
