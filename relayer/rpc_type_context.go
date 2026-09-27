package relayer

import "context"

// rpcTypeContextKey is unexported so no other package can set or read the value
// by building an equal key.
type rpcTypeContextKey struct{}

// WithRPCType returns ctx carrying the transport label a relay is counted under.
//
// The counters for publishes, drops and difficulty skips are incremented by code
// every transport shares, so that code cannot know which transport called it:
// the counting publisher is ONE wrapper handed to HTTP, WebSocket and gRPC alike
// (countPublished does not wrap twice), HTTP alone carries several rpc types, and
// the mined relay message is the wire format to the miner, which has no field
// for a transport and should not grow one for a metric.
func WithRPCType(ctx context.Context, rpcType string) context.Context {
	return context.WithValue(ctx, rpcTypeContextKey{}, rpcType)
}

// RPCTypeFrom returns the transport label set by WithRPCType, or
// metricLabelUnknown when none was set. "unknown" is a visible series of its
// own rather than a silent merge into some transport, which is what makes a
// path that forgot to set the label show up.
func RPCTypeFrom(ctx context.Context) string {
	if v, ok := ctx.Value(rpcTypeContextKey{}).(string); ok && v != "" {
		return v
	}
	return metricLabelUnknown
}
