//go:build test

package rings

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A gateway left out of a ring is reported once per process at Warn: the ring is
// rebuilt every session and the gateway's key does not change between them.
func TestGatewayLeftOutIsAWarnOncePerGateway(t *testing.T) {
	var buf bytes.Buffer
	rc := &ringClient{logger: zerolog.New(&buf).Level(zerolog.DebugLevel), gatewaysLeftOut: xsync.NewMap[string, struct{}]()}
	for _, gw := range []string{"pokt1gwa", "pokt1gwa", "pokt1gwb", "pokt1gwa"} {
		rc.gatewayLeftOutEvent(gw).Str("gateway_address", gw).Msg("left out")
	}
	var levels []string
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		levels = append(levels, line["level"].(string))
	}
	require.Equal(t, []string{"warn", "debug", "warn", "debug"}, levels)
}
