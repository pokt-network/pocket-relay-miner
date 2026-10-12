//go:build test

package miner

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
)

// A rebroadcast payload is fsynced before Put returns. The session's tx hash
// is fsynced when it is recorded, and a session that carries a hash is resent
// only from this entry: an OS crash that kept the hash and lost the payload
// would leave a dropped proof never resent.
func TestPebbleRebroadcastStore_APayloadSurvivesAnOSCrash(t *testing.T) {
	fs := vfs.NewStrictMem()
	require.NoError(t, fs.MkdirAll("db", 0o755))
	root, err := fs.OpenDir("")
	require.NoError(t, err)
	require.NoError(t, root.Sync())
	require.NoError(t, root.Close())
	open := func() (*pebblestore.Store, *pebbleRebroadcastStore) {
		store, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: "db", FS: fs, SyncInterval: time.Hour})
		require.NoError(t, err)
		return store, newPebbleRebroadcastStore(store, time.Hour)
	}
	ctx := context.Background()
	store, s := open()

	for _, phase := range []RebroadcastPhase{RebroadcastPhaseClaim, RebroadcastPhaseProof} {
		require.NoError(t, s.Put(ctx, phase, "pokt1sync", 100, "s1", []byte(string(phase)+"-payload")))
	}
	// Control: a commit after the last Put, never fsynced, is lost by the crash.
	require.NoError(t, store.DB().Set([]byte("unsynced-control"), []byte("x"), nil))

	fs.SetIgnoreSyncs(true)
	_ = store.Close()
	fs.ResetToSyncedState()
	fs.SetIgnoreSyncs(false)
	store, s = open()
	defer func() { _ = store.Close() }()

	for _, phase := range []RebroadcastPhase{RebroadcastPhaseClaim, RebroadcastPhaseProof} {
		got, err := s.List(ctx, phase, "pokt1sync", 100)
		require.NoError(t, err)
		require.Equal(t, map[string][]byte{"s1": []byte(string(phase) + "-payload")}, got,
			"the %s payload was fsynced on write", phase)
	}
	_, closer, err := store.DB().Get([]byte("unsynced-control"))
	if err == nil {
		_ = closer.Close()
	}
	require.Error(t, err, "control: the harness drops what was not fsynced")
}
