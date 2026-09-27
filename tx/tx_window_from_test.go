//go:build test

package tx

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The bool has to DISCRIMINATE, which is the whole reason it is there: "no
// budget" and "a budget of zero" are different facts, and a reader that cannot
// tell them apart puts the caller back where it started.
func TestTxWindowFrom(t *testing.T) {
	t.Run("a context carrying a budget reports it", func(t *testing.T) {
		d, regime, ok := TxWindowFrom(WithTxWindowTimeout(context.Background(), 100*time.Second, "window"))
		require.True(t, ok)
		require.Equal(t, 100*time.Second, d)
		require.Equal(t, "window", regime)
	})

	t.Run("a bare context reports none", func(t *testing.T) {
		d, regime, ok := TxWindowFrom(context.Background())
		require.False(t, ok, "nothing was ever written; saying otherwise invents a budget")
		require.Zero(t, d)
		require.Empty(t, regime)
	})

	// Only reachable from inside this package, because the key is private. It is
	// worth pinning anyway: the reader is a type assertion, and an assertion that
	// silently yields a zero value is how "absent" and "malformed" become the
	// same answer.
	t.Run("a foreign value under the same key is not a budget", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), txWindowTimeoutKey{}, "not a txWindow")
		d, regime, ok := TxWindowFrom(ctx)
		require.False(t, ok)
		require.Zero(t, d)
		require.Empty(t, regime)
	})

	// A zero-duration budget was really written: `ok` must say so even though the
	// duration looks like the absent case. This is the pair the bool exists for.
	t.Run("a zero budget is present, not absent", func(t *testing.T) {
		d, regime, ok := TxWindowFrom(WithTxWindowTimeout(context.Background(), 0, "unknown"))
		require.True(t, ok, "the value was written; only the DURATION is zero")
		require.Zero(t, d)
		require.Equal(t, "unknown", regime)
	})
}
