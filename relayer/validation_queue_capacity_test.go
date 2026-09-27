//go:build test

package relayer

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheQueuesBuiltAreExactlyTheQueuesReported ties the two readers of the
// "this service queues" rule to each other.
//
// They are the only two, and they answer different questions about the same
// fact: which services GET a queue, and which are COUNTED in the memory the
// operator provisions. While the rule was written out twice they disagreed --
// the builder asked only about the mode, so an optimistic service served only
// over WebSocket got a queue AND a published validation_queue_max_bytes series
// while the report left it out of its total. Summing that series on a dashboard
// then contradicted the startup warning, which excludes those services on
// purpose.
//
// Nothing else in the suite compares the two sets, so without this the rule can
// be changed in one place again and every other test stays green.
func TestTheQueuesBuiltAreExactlyTheQueuesReported(t *testing.T) {
	c := capConfig(128, 128)
	c.Services["ws-only"] = ServiceConfig{
		Backends: map[string]BackendConfig{"websocket": {URL: "ws://backend"}},
	}
	c.Services["eager"] = ServiceConfig{
		ValidationMode: ValidationModeEager,
		Backends:       map[string]BackendConfig{"jsonrpc": {URL: "http://backend"}},
	}

	built := make([]string, 0, len(c.Services))
	for serviceID := range newValidationQueues(c) {
		built = append(built, serviceID)
	}
	sort.Strings(built)

	reported := make([]string, 0, len(c.Services))
	for _, svc := range BuildValidationQueueReport(c, 8<<30, "cgroup").Services {
		reported = append(reported, svc.ServiceID)
	}

	require.Equal(t, reported, built,
		"LINK cap-one-predicate: a service has a queue if and only if the capacity report counts it")
	require.NotContains(t, built, "ws-only",
		"a service that never reaches the HTTP handler gets no queue, and so publishes no ceiling series")
	require.NotContains(t, built, "eager", "an eager service does not queue")
}

// capConfig is the smallest config that says something about the bound: two
// services, one taking the default and one overriding it.
func capConfig(defaultMiB, overrideMiB int) *Config {
	return &Config{
		DefaultValidationMode:        ValidationModeOptimistic,
		DefaultMaxBodySizeBytes:      10 << 20,
		DefaultValidationQueueMaxMiB: defaultMiB,
		Services: map[string]ServiceConfig{
			"takes-default": {
				Backends: map[string]BackendConfig{"jsonrpc": {URL: "http://backend"}},
			},
			"overrides": {
				ValidationQueueMaxMiB: overrideMiB,
				Backends:              map[string]BackendConfig{"jsonrpc": {URL: "http://backend"}},
			},
		},
	}
}

// TestTheDefaultAppliesAndTheOverrideIsRespected is the first half of what the
// bound was asked for: one number for everyone, and a way to say otherwise for
// one service.
func TestTheDefaultAppliesAndTheOverrideIsRespected(t *testing.T) {
	c := capConfig(256, 512)

	require.Equal(t, int64(256)<<20, c.ValidationQueueMaxBytes("takes-default"),
		"a service that configures nothing takes the default")
	require.Equal(t, int64(512)<<20, c.ValidationQueueMaxBytes("overrides"),
		"a service that configures its own bound gets THAT one")

	// And zero is the default, never "no bound": an unbounded queue is what
	// this exists to prevent, and it is the reading an operator most easily
	// assumes.
	zeroed := capConfig(0, 0)
	require.Equal(t, int64(DefaultValidationQueueMaxMiB)<<20, zeroed.ValidationQueueMaxBytes("takes-default"))
	require.Equal(t, int64(DefaultValidationQueueMaxMiB)<<20, zeroed.ValidationQueueMaxBytes("overrides"))
}

// TestABoundBelowItsFloorIsRaisedInsteadOfRefusingEveryRelay pins the branch
// that protects an operator from their own number.
//
// A bound under what ONE relay of that service retains makes the service reject
// 100% of its traffic, forever. Refusing to boot instead would turn one dead
// service into a dead relayer, so the value is raised and the operator is told.
func TestABoundBelowItsFloorIsRaisedInsteadOfRefusingEveryRelay(t *testing.T) {
	// 64 MiB is the smallest an operator may write; the floor for a service
	// with 30 MiB bodies is above it.
	c := capConfig(DefaultValidationQueueMaxMiB, MinValidationQueueMaxMiB)
	svc := c.Services["overrides"]
	svc.MaxBodySizeBytes = 30 << 20
	c.Services["overrides"] = svc

	floor := c.ValidationQueueFloorBytes("overrides")
	require.Equal(t, int64(2*(30<<20)+(30<<20)), floor,
		"one queued relay holds its request TWICE -- the body and the copied Payload -- plus one response, "+
			"and the response is bounded by the LARGEST service, not by this one")
	require.Greater(t, floor, int64(MinValidationQueueMaxMiB)<<20, "precondition: the configured value is under the floor")

	require.Equal(t, floor, c.ValidationQueueMaxBytes("overrides"),
		"a bound under the floor is RAISED to it")

	report := BuildValidationQueueReport(c, 8<<30, "cgroup")
	var raised []string
	for _, s := range report.RaisedToFloor() {
		raised = append(raised, s.ServiceID)
	}
	require.Equal(t, []string{"overrides"}, raised,
		"and the operator is told WHICH service, because the number they wrote is not the one in force")
}

// TestTheReportCountsOnlyWhatTheseQueuesCanHold guards the number the operator
// sizes RAM with.
//
// Only the HTTP handler feeds this queue: an optimistic service served over
// WebSocket or gRPC never enters it. Counting those would add bounds that
// cannot be reached to the total, and the operator would provision for memory
// this queue can never hold.
func TestTheReportCountsOnlyWhatTheseQueuesCanHold(t *testing.T) {
	c := capConfig(128, 128)
	c.Services["websocket-only"] = ServiceConfig{
		Backends: map[string]BackendConfig{"websocket": {URL: "ws://backend"}},
	}
	c.Services["eager"] = ServiceConfig{
		ValidationMode: ValidationModeEager,
		Backends:       map[string]BackendConfig{"jsonrpc": {URL: "http://backend"}},
	}

	report := BuildValidationQueueReport(c, 8<<30, "cgroup")

	var counted []string
	for _, s := range report.Services {
		counted = append(counted, s.ServiceID)
	}
	require.Equal(t, []string{"overrides", "takes-default"}, counted,
		"an eager service does not queue, and neither does one served only over WebSocket")
	require.Equal(t, int64(256)<<20, report.TotalBytes, "the total is the SUM of the two that can queue")
}

// TestAnUnknownMemoryLimitIsReportedAndNeverCountedAsAFit pins the difference
// between "it fits" and "nobody looked".
func TestAnUnknownMemoryLimitIsReportedAndNeverCountedAsAFit(t *testing.T) {
	report := BuildValidationQueueReport(capConfig(128, 128), 0, "none")

	require.False(t, report.Fits(), "an unknown limit is not a fit: it is an unknown")
	require.False(t, report.MarginIsThin(), "and it has no margin to call thin")
	require.Contains(t, report.String(), "unknown", "the operator is told the comparison could not be made")
}

// TestAThinMarginIsWorthSayingOutLoud covers the warning that fires while
// everything still technically fits.
func TestAThinMarginIsWorthSayingOutLoud(t *testing.T) {
	c := capConfig(1024, 1024) // 2 GiB of queues

	fits := BuildValidationQueueReport(c, 8<<30, "cgroup")
	require.True(t, fits.Fits())
	require.False(t, fits.MarginIsThin(), "6 GiB of headroom is not worth a warning")

	thin := BuildValidationQueueReport(c, (2<<30)+(512<<20), "cgroup")
	require.True(t, thin.Fits(), "precondition: it still fits")
	require.True(t, thin.MarginIsThin(), "half a gigabyte of headroom is: an ordinary spike finishes the job")

	over := BuildValidationQueueReport(c, 1<<30, "cgroup")
	require.False(t, over.Fits(), "the queues alone are over the limit")
	require.Negative(t, over.MarginBytes())
}
