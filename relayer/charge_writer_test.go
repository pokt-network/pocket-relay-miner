//go:build test

package relayer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// chargeWriter puts a meter's served charges in Redis the way the relayer does:
// through a real batching publisher wired to the meter's ledger and heartbeat.
//
// Its interval is an hour, so nothing is written until flush. flush closes the
// publisher, whose final flush writes everything settled so far, and wires a
// fresh one so the meter keeps admitting. That makes the write a step the test
// takes, not a tick it has to wait for.
type chargeWriter struct {
	t       *testing.T
	meter   *RelayMeter
	client  *redisutil.Client
	batcher *redisutil.BatchingPublisher
}

func newChargeWriter(t *testing.T, meter *RelayMeter, client *redisutil.Client) *chargeWriter {
	t.Helper()
	w := &chargeWriter{t: t, meter: meter, client: client}
	w.wire()
	t.Cleanup(func() { _ = w.batcher.Close() })
	return w
}

func (w *chargeWriter) wire() {
	w.batcher = redisutil.NewBatchingPublisher(testLogger(), w.client.UniversalClient, w.client.KB().StreamPrefix(), time.Hour)
	w.batcher.SetChargeLedger(w.meter.ChargeLedger())
	w.meter.SetDispatcherHeartbeat(w.batcher.LastSuccess)
}

// flush writes every charge settled so far.
func (w *chargeWriter) flush() {
	w.t.Helper()
	require.NoError(w.t, w.batcher.Close())
	w.wire()
}
