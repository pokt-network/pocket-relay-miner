//go:build test

package query

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// uniqueServiceID returns a label value guaranteed to be different
// across test invocations in the same process. Matters because
// observability.SharedRegistry is a package-global that outlives
// individual Test* functions: reusing a static label would let one
// test read another test's gauge value and pass for the wrong reason.
func uniqueServiceID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// scrapeSharedMetrics scrapes the promhttp handler bound to
// SharedRegistry. Returned string is the full /metrics body — callers
// assert on the exact labelled sample line they care about.
func scrapeSharedMetrics(t *testing.T) string {
	t.Helper()
	handler := promhttp.HandlerFor(
		prometheus.Gatherers{observability.SharedRegistry},
		promhttp.HandlerOpts{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

// TestCountLeadingZeroBits pins the bit-counting invariants the gauge
// value depends on: each case encodes a specific "bit N halves the
// mineable fraction" boundary.
func TestCountLeadingZeroBits(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"nil input is base difficulty (0 bits)", nil, 0},
		{"empty slice is base difficulty (0 bits)", []byte{}, 0},
		{"all-ones is base difficulty (0 bits)", []byte{0xff, 0xff, 0xff, 0xff}, 0},
		{"half (0x7f...) is 1 bit", []byte{0x7f, 0xff}, 1},
		{"quarter (0x3f...) is 2 bits", []byte{0x3f, 0xff}, 2},
		{"eighth (0x1f...) is 3 bits", []byte{0x1f, 0xff}, 3},
		{"first byte zero + 0x80 is 8 bits", []byte{0x00, 0xff}, 8},
		{"first byte zero + 0x7f is 9 bits", []byte{0x00, 0x7f}, 9},
		{"all zero 32 bytes (realistic chain length) is 256 bits", make([]byte, 32), 256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, countLeadingZeroBits(c.in))
		})
	}
}

// TestRecordRelayMiningDifficulty_WiringScrapeable proves the gauge is
// registered in SharedRegistry and its sample appears at /metrics.
// Uses a unique service_id per invocation so the assertion cannot be
// satisfied by a value left over from a prior test or prior
// -count=N run.
func TestRecordRelayMiningDifficulty_WiringScrapeable(t *testing.T) {
	svc := uniqueServiceID("test_wiring")
	// 0x3f... → 2 leading zero bits.
	recordRelayMiningDifficulty(svc, []byte{0x3f, 0xff, 0x00, 0x00})

	body := scrapeSharedMetrics(t)
	needle := fmt.Sprintf(
		`ha_query_relay_mining_difficulty_target_bits{service_id=%q} 2`,
		svc,
	)
	require.Truef(t, strings.Contains(body, needle),
		"expected sample line missing\nwant: %s", needle)
}

// TestRecordRelayMiningDifficulty_Overwrite proves gauge semantics:
// later writes for the same service_id overwrite earlier ones, not
// accumulate. This is the contract operators rely on when the chain
// adjusts difficulty down (which happens on EMA-based readjustments).
func TestRecordRelayMiningDifficulty_Overwrite(t *testing.T) {
	svc := uniqueServiceID("test_overwrite")
	recordRelayMiningDifficulty(svc, []byte{0xff}) // 0 bits
	recordRelayMiningDifficulty(svc, []byte{0x1f}) // 3 bits
	recordRelayMiningDifficulty(svc, []byte{0x7f}) // 1 bit — difficulty went DOWN

	body := scrapeSharedMetrics(t)
	needle := fmt.Sprintf(
		`ha_query_relay_mining_difficulty_target_bits{service_id=%q} 1`,
		svc,
	)
	require.Truef(t, strings.Contains(body, needle),
		"gauge did not reflect latest write\nwant: %s", needle)
}

// TestGetServiceRelayDifficulty_EmitsMetric closes the integration
// loop: a chain query that goes through the real
// GetServiceRelayDifficulty path must emit the gauge sample. Without
// this, a future refactor that accidentally moves
// recordRelayMiningDifficulty out of the cache-miss path (e.g., into
// a helper that gets called on only some branches) would silently
// kill the metric and the unit-level tests would still pass.
func TestGetServiceRelayDifficulty_EmitsMetric(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	svc := uniqueServiceID("test_integration_latest")
	// Build a realistic 32-byte target_hash with exactly 2 leading
	// zero bits so the assertion is tight.
	target := make([]byte, 32)
	target[0] = 0x3f
	for i := 1; i < 32; i++ {
		target[i] = 0xff
	}
	mock.getRelayMiningDifficultyFunc = func(
		_ context.Context, req *servicetypes.QueryGetRelayMiningDifficultyRequest,
	) (*servicetypes.QueryGetRelayMiningDifficultyResponse, error) {
		require.Equal(t, svc, req.ServiceId)
		return &servicetypes.QueryGetRelayMiningDifficultyResponse{
			RelayMiningDifficulty: servicetypes.RelayMiningDifficulty{
				ServiceId:    svc,
				TargetHash:   target,
				NumRelaysEma: 100,
			},
		}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	defer qc.Close()

	_, err = qc.Service().GetServiceRelayDifficulty(context.Background(), svc)
	require.NoError(t, err)

	body := scrapeSharedMetrics(t)
	needle := fmt.Sprintf(
		`ha_query_relay_mining_difficulty_target_bits{service_id=%q} 2`,
		svc,
	)
	require.Truef(t, strings.Contains(body, needle),
		"GetServiceRelayDifficulty did not emit the expected gauge sample\nwant: %s", needle)
}

// TestGetServiceRelayDifficultyAtHeight_EmitsMetric does the same for
// the height-aware query path, which is the one actually used in
// production by relay validation and the miner's lifecycle callback.
// If only the non-height path were instrumented, production miners
// would never see the gauge populated.
func TestGetServiceRelayDifficultyAtHeight_EmitsMetric(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()

	svc := uniqueServiceID("test_integration_at_height")
	const sessionStartHeight = int64(100_000)
	// 0x1f → 3 leading zero bits.
	target := make([]byte, 32)
	target[0] = 0x1f
	for i := 1; i < 32; i++ {
		target[i] = 0xff
	}
	mock.getRelayMiningDifficultyAtHeightFunc = func(
		_ context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest,
	) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
		require.Equal(t, svc, req.ServiceId)
		require.Equal(t, sessionStartHeight, req.BlockHeight)
		return &servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse{
			RelayMiningDifficulty: servicetypes.RelayMiningDifficulty{
				ServiceId:    svc,
				TargetHash:   target,
				NumRelaysEma: 50,
				BlockHeight:  sessionStartHeight,
			},
		}, nil
	}

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	qc, err := NewQueryClients(logger, ClientConfig{
		GRPCEndpoint: address,
		QueryTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	defer qc.Close()

	_, err = qc.ServiceDifficulty().GetServiceRelayDifficultyAtHeight(
		context.Background(), svc, sessionStartHeight,
	)
	require.NoError(t, err)

	body := scrapeSharedMetrics(t)
	needle := fmt.Sprintf(
		`ha_query_relay_mining_difficulty_target_bits{service_id=%q} 3`,
		svc,
	)
	require.Truef(t, strings.Contains(body, needle),
		"GetServiceRelayDifficultyAtHeight did not emit the expected gauge sample\nwant: %s", needle)
}
