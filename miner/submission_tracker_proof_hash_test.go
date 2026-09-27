//go:build test

package miner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// TestProofTrackingRecordHoldsTheProofHashNotTheProof: the submission tracking
// record used to hold the whole proof in hex in proof_hash -- 4.2 MB per record
// with 1 MiB relays, because a closest proof carries the relay. It must hold the
// proof's SHA-256 and its size, and stay small whatever the proof weighs, on every
// path that writes it: updating the claim's record, creating a missing one, and
// recording a failed submission.
func TestProofTrackingRecordHoldsTheProofHashNotTheProof(t *testing.T) {
	// Incompressible, so the size asserted is the proof's and not a filler's.
	proof := transport.ChainedHashBytes("proof", 1<<20)
	digest := sha256.Sum256(proof)
	wantHash := hex.EncodeToString(digest[:])

	cases := []struct {
		name      string
		seedClaim bool
		success   bool
	}{
		{name: "updates the claim's record", seedClaim: true, success: true},
		{name: "creates a missing record", seedClaim: false, success: true},
		{name: "records a failed submission", seedClaim: true, success: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			client, _ := newTestRedis(t)
			tr := NewSubmissionTracker(zerolog.Nop(), client, time.Hour)
			const supplier, sessionID = "pokt1proofhash", "sess-proofhash"
			if tc.seedClaim {
				seedClaim(t, tr, supplier, sessionID, "0xclaimtx")
			}
			errorReason := ""
			if !tc.success {
				errorReason = "broadcast failed"
			}

			require.NoError(t, tr.TrackProofSubmission(ctx, supplier, 110, sessionID,
				proof, "0xprooftx", tc.success, errorReason, 105, 110, true, ""))

			record, err := tr.GetRecord(ctx, supplier, 110, sessionID)
			require.NoError(t, err)
			require.Equal(t, wantHash, record.ProofHash, "proof_hash is the SHA-256 of the proof bytes")
			require.Equal(t, int64(len(proof)), record.ProofSizeBytes, "and the size is the proof's")
			size, err := client.StrLen(ctx, client.KB().TxTrackKey(supplier, 110, sessionID)).Result()
			require.NoError(t, err)
			require.Less(t, size, int64(8<<10), "a 1 MiB proof leaves the record small")
		})
	}
}
