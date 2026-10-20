//go:build test

package miner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func logLevels(t *testing.T, buf *bytes.Buffer, msg string) []string {
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

// A plan that cannot be read is a Warn when reads start failing, Debug while
// they keep failing, and an Info when they recover: not a line for every
// supplier batch and every retry.
func TestProofShape_AFailingReadWarnsOnceAndSaysWhenItRecovers(t *testing.T) {
	var buf bytes.Buffer
	errNode := errors.New("node unavailable")
	shape := NewProofShape(zerolog.New(&buf).Level(zerolog.DebugLevel), &scriptedUpgrades{answers: []upgradeAnswer{
		{err: errNode}, {err: errNode}, {err: errNode}, {height: 0}, {err: errNode},
	}})
	for h := int64(1); h <= 5; h++ {
		shape.Weighted(context.Background(), h)
	}
	require.Equal(t, []string{"warn", "debug", "debug", "warn"},
		logLevels(t, &buf, "could not read the upgrade plan; building closest proofs until it is read"),
		"a Warn when reads start failing, and again after they recovered")
	require.Equal(t, []string{"info"}, logLevels(t, &buf, "the upgrade plan is read again"))
}
