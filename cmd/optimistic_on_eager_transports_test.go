//go:build test

package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// startupLines runs the startup check against config and returns the log lines
// it wrote, decoded.
func startupLines(t *testing.T, config *relayer.Config) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logOptimisticOnEagerOnlyTransports(zerolog.New(&buf).Level(zerolog.DebugLevel), config)
	var lines []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		lines = append(lines, line)
	}
	return lines
}

// Optimistic validation on WebSocket or gRPC is one Warn at start naming every
// such service with those rpc types, whether the mode is the service's own or
// the default; eager services and HTTP-only ones are not named.
func TestOptimisticOnEagerOnlyTransportsIsOneWarnAtStart(t *testing.T) {
	config := &relayer.Config{
		DefaultValidationMode: relayer.ValidationModeOptimistic,
		Services: map[string]relayer.ServiceConfig{
			"eth": {Backends: map[string]relayer.BackendConfig{
				"jsonrpc": {URL: "http://backend"}, "websocket": {URL: "ws://backend"},
			}},
			"cosmos": {
				ValidationMode: relayer.ValidationModeOptimistic,
				Backends: map[string]relayer.BackendConfig{
					"grpc": {URL: "grpc://backend"}, "websocket": {URL: "ws://backend"},
				},
			},
			"ws-eager": {
				ValidationMode: relayer.ValidationModeEager,
				Backends:       map[string]relayer.BackendConfig{"websocket": {URL: "ws://backend"}},
			},
			"http-only": {Backends: map[string]relayer.BackendConfig{"jsonrpc": {URL: "http://backend"}}},
		},
	}

	lines := startupLines(t, config)
	require.Len(t, lines, 1)
	require.Equal(t, "warn", lines[0]["level"])
	require.Equal(t, []any{"cosmos (grpc, websocket)", "eth (websocket)"}, lines[0]["services"])
}

// A config with no such service writes nothing.
func TestOptimisticOnEagerOnlyTransportsIsSilentWhenNoneApplies(t *testing.T) {
	config := &relayer.Config{
		DefaultValidationMode: relayer.ValidationModeEager,
		Services: map[string]relayer.ServiceConfig{
			"ws": {Backends: map[string]relayer.BackendConfig{"websocket": {URL: "ws://backend"}}},
			"http": {
				ValidationMode: relayer.ValidationModeOptimistic,
				Backends:       map[string]relayer.BackendConfig{"jsonrpc": {URL: "http://backend"}},
			},
		},
	}
	require.Empty(t, startupLines(t, config))
}
