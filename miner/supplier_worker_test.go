//go:build test

package miner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDefaultSupplierReconcileIntervalMatchesTTLMarginAssumption pins the
// other half of cache.TestSupplierCacheTTLFromParams_MarginAboveReconcileInterval:
// that test hardcodes 60s as "the reconcile interval" because cache cannot
// import miner (miner already imports cache — a cycle). This test makes sure
// the two numbers cannot drift apart silently: if
// DefaultSupplierReconcileInterval ever changes, this fails and says to go
// re-check the TTL margin in cache/supplier_cache.go, instead of the margin
// quietly shrinking underneath an unrelated tuning change.
func TestDefaultSupplierReconcileIntervalMatchesTTLMarginAssumption(t *testing.T) {
	require.Equal(t, 60*time.Second, DefaultSupplierReconcileInterval,
		"cache.SupplierCacheTTLFromParams' margin test assumes this exact value — "+
			"changing DefaultSupplierReconcileInterval requires re-checking that the "+
			"supplier cache TTL still comfortably outlives it")
}
