//go:build test

package tx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// levelsOf returns the level of every log line with this message.
func levelsOf(t *testing.T, buf *bytes.Buffer, msg string) []string {
	t.Helper()
	var levels []string
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line["message"] == msg {
			levels = append(levels, line["level"].(string))
		}
	}
	return levels
}

// A CheckTx refusal is Warn, except code 19: a re-sent transaction the node
// already holds, which is the resend loop working (ErrTxAlreadyQueued) and is
// counted by the rebroadcast metrics.
func TestCheckTxRefusalIsWarnExceptAlreadyInTheMempool(t *testing.T) {
	for _, tc := range []struct {
		name      string
		codespace string
		code      uint32
		want      string
	}{
		{name: "already in the mempool", codespace: "sdk", code: 19, want: "debug"},
		{name: "out of gas", codespace: "sdk", code: 11, want: "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := setupMockGRPCServer(t)
			t.Cleanup(srv.cleanup)
			srv.setBroadcastFailureFrom(tc.codespace, tc.code, "refused")

			srv.addAccount(budgetTestSupplier, 1, 0)
			km := setupTestKeyManager(t, budgetTestSupplier)
			t.Cleanup(func() { _ = km.Close() })
			var buf bytes.Buffer
			client, err := NewTxClient(zerolog.New(&buf).Level(zerolog.DebugLevel), km, TxClientConfig{
				GRPCEndpoint:      srv.address,
				ChainID:           "test-chain",
				GasPrice:          parseGasPrice(t, "0.001upokt"),
				ConnProbeInterval: time.Hour,
				BlockTimeProvider: testBlockTime(),
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })

			_ = claim(client, context.Background(), t)
			require.Equal(t, []string{tc.want}, levelsOf(t, &buf, "transaction CheckTx failed"))
		})
	}
}
