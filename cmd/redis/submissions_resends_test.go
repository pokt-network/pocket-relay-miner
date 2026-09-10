//go:build test

package redis

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// capture returns everything f writes to stdout.
func capture(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	f()
	require.NoError(t, w.Close())
	return <-done
}

// The resend counters must reach the HUMAN output, not only the record.
//
// This is the last of four layers and the one that was missing for the proof
// side all along: proof_rebroadcasts has been persisted by the miner for a
// while, and this command never showed it, because cmd/redis carries its OWN
// copy of the record struct and that copy did not declare the field. A field
// absent from the copy is invisible in BOTH outputs -- the detail view and
// --json -- since the JSON is marshalled from the copy too. Persisting a
// counter nothing prints leaves the operator exactly where they started.
func TestPrintSubmissionDetail_ShowsResendsForBothPhases(t *testing.T) {
	out := capture(t, func() {
		printSubmissionDetail(&submissionRecord{
			SessionID:         "sess-1",
			Supplier:          "pokt1test",
			ClaimTxHash:       "0xclaim",
			ClaimRebroadcasts: 2,
			ProofTxHash:       "0xproof",
			ProofRebroadcasts: 3,
		})
	})
	require.Contains(t, out, "Claim Resends:     2", "a claim resent twice must say so in the output an operator reads")
	require.Contains(t, out, "Proof Resends:     3", "the proof side was persisted and never printed; that is the half this fixes")
}

// Zero must stay silent: printing "0" on the overwhelmingly common record that
// was never resent reads as "measured, and it was none", which is a different
// claim from "nothing to report here".
func TestPrintSubmissionDetail_OmitsResendsWhenThereWereNone(t *testing.T) {
	out := capture(t, func() {
		printSubmissionDetail(&submissionRecord{
			SessionID:   "sess-2",
			Supplier:    "pokt1test",
			ClaimTxHash: "0xclaim",
			ProofTxHash: "0xproof",
		})
	})
	require.False(t, strings.Contains(out, "Resends:"), "no resend happened, so no resend line; got:\n%s", out)
}
