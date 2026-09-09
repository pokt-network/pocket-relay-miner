//go:build test

package tx

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
)

// The classifiers are exercised through the REAL client, and that is the whole
// point of this file.
//
// The test that motivated it lived in the reconciler and built a *TxRejection by
// hand with ABCICode 19 and codespace "sdk". Its assertion was true -- a resend
// handed that sentinel does not spend an attempt -- but nothing in it ever
// reached the function that DECIDES whether a 19 becomes that sentinel. Measured:
// making isAlreadyQueuedRejection return false unconditionally left every test in
// ./tx/ and ./miner/ green. A test that constructs its own input proves what the
// code does WITH it, never that the system produces it.
//
// So here the rejection comes from a broadcast that actually fails, and the
// assertion is errors.Is on what the client returns.
//
// THE PAIR IS THE KEY, NOT THE NUMBER. ABCI codes are registered per codespace,
// so a 19 or a 30 arriving from another module means something unrelated -- both
// sentinels say so in their own comments, and until this file existed nothing
// held them to it: dropping the codespace check from either classifier changed
// no test's colour.
func TestCheckTxRejectionClassification(t *testing.T) {
	for _, tt := range []struct {
		name      string
		codespace string
		code      uint32
		rawLog    string
		wantErr   error // nil = must match neither sentinel
	}{
		{
			name:      "19 from the sdk is already-queued",
			codespace: "sdk", code: 19,
			rawLog:  "tx already in mempool",
			wantErr: ErrTxAlreadyQueued,
		},
		{
			name:      "30 from the sdk is window-expired",
			codespace: "sdk", code: 30,
			rawLog:  "block height: 4322, timeout height: 4321: tx timeout height",
			wantErr: ErrTxWindowExpired,
		},
		{
			// The cross pair. x/proof registering a 19 of its own would be an
			// unrelated error, and swallowing it as "already queued" would tell
			// the reconciler to stop trying over something it never sent.
			name:      "19 from another module is NOT already-queued",
			codespace: "proof", code: 19,
			rawLog:  "something else entirely",
			wantErr: nil,
		},
		{
			name:      "30 from another module is NOT window-expired",
			codespace: "proof", code: 30,
			rawLog:  "something else entirely",
			wantErr: nil,
		},
		{
			// A neighbouring code in the RIGHT codespace: 20 is ErrMempoolIsFull,
			// deliberately left unclassified, and it must stay that way. Without
			// this row a classifier that matched "any sdk rejection" would pass
			// every other row in the table.
			name:      "20 from the sdk is neither",
			codespace: "sdk", code: 20,
			rawLog:  "mempool is full",
			wantErr: nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const supplierAddr = "pokt1supplier-classification"
			testServer, tc := newTimeoutHeightTestClient(t, supplierAddr)
			testServer.setBroadcastFailureFrom(tt.codespace, tt.code, tt.rawLog)

			claims := []*prooftypes.MsgCreateClaim{
				generateTestClaim(t, supplierAddr, "session-classification"),
			}
			_, err := tc.CreateClaims(context.Background(), supplierAddr, 4321, claims)
			require.Error(t, err, "a non-zero CheckTx code must surface as an error")

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr,
					"codespace=%q code=%d must classify", tt.codespace, tt.code)
			} else {
				require.NotErrorIs(t, err, ErrTxAlreadyQueued,
					"codespace=%q code=%d must NOT be read as already-queued", tt.codespace, tt.code)
				require.NotErrorIs(t, err, ErrTxWindowExpired,
					"codespace=%q code=%d must NOT be read as window-expired", tt.codespace, tt.code)
			}

			// Whatever it classified as, the datum survives: both sentinels wrap
			// rather than replace, so a caller can still ask what the chain said.
			var rejection *TxRejection
			require.True(t, errors.As(err, &rejection))
			require.Equal(t, tt.code, rejection.ABCICode)
			require.Equal(t, tt.codespace, rejection.Codespace)
		})
	}
}
