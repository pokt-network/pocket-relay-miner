//go:build test

package relayer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// A service factor above the protocol guarantee is reported once per service at
// Warn: the limit is recomputed for every session meter, and the factor of a
// service does not change between them.
func TestServiceFactorOverGuaranteeIsAWarnOncePerService(t *testing.T) {
	redisClient, _ := newTestRedis(t)
	app := &fakeAppClient{addr: "pokt1app_scoped"}
	app.stakeUpokt.Store(1_000_000)
	paramCache := &fakeSharedParamCache{params: &sharedtypes.Params{
		NumBlocksPerSession:            10,
		ComputeUnitsToTokensMultiplier: 1,
		ComputeUnitCostGranularity:     1,
		ClaimWindowOpenOffsetBlocks:    2,
		ClaimWindowCloseOffsetBlocks:   2,
		ProofWindowOpenOffsetBlocks:    3,
		ProofWindowCloseOffsetBlocks:   3,
	}}

	var buf bytes.Buffer
	// A factor of 1 is the whole stake, above the guarantee of half of it with
	// 2 pending sessions.
	meter := NewRelayMeter(
		zerolog.New(&buf).Level(zerolog.DebugLevel),
		kv.NewRedis(zerolog.Nop(), redisClient),
		app,
		nil,
		&fakeSessionClient{numSuppliers: 1},
		nil,
		paramCache,
		nil,
		staticServiceFactor{f: 1},
		RelayMeterConfig{},
	)

	ctx := context.Background()
	for _, serviceID := range []string{"seda", "seda", "eth", "seda"} {
		_, _, _, err := meter.calculateMaxStake(ctx, "pokt1app_scoped", serviceID, 100, 95)
		require.NoError(t, err)
	}

	var levels []string
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		if line["message"] == "serviceFactor results in limit exceeding protocol guarantee - may result in unpaid work" {
			levels = append(levels, line["level"].(string))
		}
	}
	require.Equal(t, []string{"warn", "debug", "warn", "debug"}, levels)
}
