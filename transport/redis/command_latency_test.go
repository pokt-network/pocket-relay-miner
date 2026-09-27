//go:build test

package redis

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// samples returns how many observations the latency histogram holds for one
// command label, read out of the shared registry the factory writes to.
func samples(t *testing.T, component, command string) uint64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 512)
	go func() {
		commandLatency.Collect(ch)
		close(ch)
	}()

	var count uint64
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		var gotComponent, gotCommand string
		for _, l := range pb.GetLabel() {
			switch l.GetName() {
			case "component":
				gotComponent = l.GetValue()
			case "command":
				gotCommand = l.GetValue()
			}
		}
		if gotComponent == component && gotCommand == command && pb.GetHistogram() != nil {
			count += pb.GetHistogram().GetSampleCount()
		}
	}
	return count
}

// TestCommandLatencyHookTimesSingleCommands is the baseline: without it the
// pipeline test below could pass while nothing worked.
func TestCommandLatencyHookTimesSingleCommands(t *testing.T) {
	client := testredis.Client(t)
	const component = "test-single"
	client.AddHook(NewCommandLatencyHook(component))

	before := samples(t, component, "set")
	require.NoError(t, client.Set(context.Background(), testredis.Prefix(t)+":k", "v", 0).Err())

	require.Equal(t, before+1, samples(t, component, "set"),
		"a single command must produce exactly one observation")
}

// TestCommandLatencyHookTimesTxPipelines is the case that hides. Returning
// ProcessPipelineHook as a pass-through is the natural shortcut and it leaves
// the histogram blind to EVERY pipelined write -- which is the whole bulk
// publish path -- while a suite full of single commands stays green.
//
// The traffic here is exclusively a TxPipeline on purpose: one stray single
// command would move the "pipeline" label's neighbour, not this series, but a
// test that mixed them could be read as covering this when it does not.
func TestCommandLatencyHookTimesTxPipelines(t *testing.T) {
	client := testredis.Client(t)
	const component = "test-txpipeline"
	client.AddHook(NewCommandLatencyHook(component))

	before := samples(t, component, pipelineCommandLabel)

	prefix := testredis.Prefix(t)
	_, err := client.TxPipelined(context.Background(), func(pipe goredis.Pipeliner) error {
		pipe.Set(context.Background(), prefix+":a", "1", 0)
		pipe.Set(context.Background(), prefix+":b", "2", 0)
		return nil
	})
	require.NoError(t, err)

	require.Equal(t, before+1, samples(t, component, pipelineCommandLabel),
		"a TxPipeline must produce ONE observation under the pipeline label: it is one "+
			"round trip and one pool acquisition, and if this is zero the pipeline hook "+
			"is a pass-through and every bulk write is unmeasured")

	require.Equal(t, uint64(0), samples(t, component, "set"),
		"and its commands must NOT be counted individually: attributing the batch's "+
			"duration to each command turns one wait into N")
}
