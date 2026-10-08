package pebblequeue

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// gatherBySupplier scrapes c alone and returns family -> supplier -> value.
func gatherBySupplier(t *testing.T, c prometheus.Collector) map[string]map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(c))
	families, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]map[string]float64{}
	for _, f := range families {
		require.Equal(t, dto.MetricType_GAUGE, f.GetType(), f.GetName())
		byS := map[string]float64{}
		for _, m := range f.GetMetric() {
			require.Len(t, m.GetLabel(), 1, f.GetName())
			require.Equal(t, "supplier", m.GetLabel()[0].GetName())
			byS[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
		out[f.GetName()] = byS
	}
	return out
}

// The gauges count what each supplier's queue stores and what its consumer
// holds, and follow an acknowledgement and a release on the next scrape. A
// supplier with no consumer has its entries stored and none pending.
func TestCollector_ReportsLengthAndPendingBySupplier(t *testing.T) {
	f := open(t, t.TempDir())
	c := f.broker.Collector()
	require.Empty(t, gatherBySupplier(t, c), "nothing published, no series")

	f.publish(5)
	got := f.receive(5)
	other := relay("s2", 0)
	other.SupplierOperatorAddress = "pokt1other"
	require.NoError(t, f.pub.Publish(context.Background(), other))
	require.NoError(t, f.pub.Publish(context.Background(), other))

	require.Equal(t, map[string]map[string]float64{
		"ha_standalone_queue_length":  {testSupplier: 5, "pokt1other": 2},
		"ha_standalone_queue_pending": {testSupplier: 5, "pokt1other": 0},
	}, gatherBySupplier(t, c))

	require.NoError(t, f.consumer.AckMessage(context.Background(), got[0]))
	// Released: still stored, no longer pending, and not delivered again
	// before the release delay.
	require.NoError(t, f.consumer.ReleaseMessage(context.Background(), got[1]))

	require.Equal(t, map[string]map[string]float64{
		"ha_standalone_queue_length":  {testSupplier: 4, "pokt1other": 2},
		"ha_standalone_queue_pending": {testSupplier: 3, "pokt1other": 0},
	}, gatherBySupplier(t, c))
}

// A scrape after the store closed reads nothing (Pebble panics on a closed
// database) and reports no series.
func TestCollector_AClosedStoreReportsNoQueue(t *testing.T) {
	f := open(t, t.TempDir())
	c := f.broker.Collector()
	f.publish(2)
	require.Len(t, gatherBySupplier(t, c), 2, "the control: an open store reports both families")

	f.close()

	require.Empty(t, gatherBySupplier(t, c))
}
