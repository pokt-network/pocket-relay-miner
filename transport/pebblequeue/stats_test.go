package pebblequeue

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// A queue's stats count what is stored, what the miner holds and what it handed
// back, and name the last entry published.
func TestStats_CountStoredPendingAndReleasedEntries(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(3)
	got := f.receive(3)
	require.NoError(t, f.consumer.AckMessage(context.Background(), got[0]))
	require.NoError(t, f.consumer.ReleaseMessage(context.Background(), got[1]))

	st, err := f.broker.Stats(testSupplier)
	require.NoError(t, err)
	require.Equal(t, QueueStats{
		Supplier: testSupplier,
		Stream:   transport.SupplierStreamName(testPrefix, testSupplier),
		Length:   2,
		Pending:  1,
		Released: 1,
		LastID:   got[2].ID,
	}, st)

	all, err := f.broker.AllStats()
	require.NoError(t, err)
	require.Equal(t, []QueueStats{st}, all)

	none, err := f.broker.Stats("pokt1nobody")
	require.NoError(t, err)
	require.Equal(t, QueueStats{Supplier: "pokt1nobody", Stream: transport.SupplierStreamName(testPrefix, "pokt1nobody")}, none)
}

// After a restart the stored entries and the last ID are read from the store,
// with nothing pending until the new consumer delivers.
func TestStats_ReadTheStoreAfterARestart(t *testing.T) {
	f := open(t, t.TempDir())
	f.publish(2)
	got := f.receive(2)
	f = f.restart()

	st, err := f.broker.Stats(testSupplier)
	require.NoError(t, err)
	require.Equal(t, int64(2), st.Length)
	require.Equal(t, got[1].ID, st.LastID)
}
