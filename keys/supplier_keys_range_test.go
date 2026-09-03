package keys

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A secp256k1 private key is a NUMBER in [1, N-1], not any 32 bytes. Handed
// something outside that range the crypto library reduces it modulo N instead of
// refusing it, so the key on disk is silently not the key that signs. Measured
// 2026-09-03: 1 and N+1 derive the same address, and so do 0 and N.
//
// Two VALID keys never collide, which is why this is not about collisions. It is
// about material that is not a key at all, and the case that actually happens is
// ZERO -- a truncated file, a secret that mounted empty.
const (
	curveOrderN      = "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141"
	curveOrderNMinus = "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364140"
	curveOrderNPlus  = "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364142"
	allZeroKey       = "0000000000000000000000000000000000000000000000000000000000000000"
	allOnesKey       = "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF"
)

func TestValidateHexKeyFormatRejectsMaterialThatIsNotAKey(t *testing.T) {
	tests := []struct {
		name      string
		hexKey    string
		wantErr   bool
		errSubstr string
	}{
		{name: "an ordinary key is accepted", hexKey: testAppHex},
		{name: "the same key with a 0x prefix is accepted", hexKey: "0x" + testAppHex},

		// The boundary, and the reason it is here: N-1 is the LARGEST valid key.
		// A check that merely refused large-looking values would reject it, and
		// would then be rejecting real keys -- worse than the defect.
		{name: "N-1 is the largest valid key and must be accepted", hexKey: curveOrderNMinus},

		{
			name:   "zero is not a key, and it is the case that actually happens",
			hexKey: allZeroKey, wantErr: true, errSubstr: "the value is zero",
		},
		{
			name: "N itself reduces to zero", hexKey: curveOrderN,
			wantErr: true, errSubstr: "curve order N",
		},
		{
			name: "N+1 would sign as 1, at a different address", hexKey: curveOrderNPlus,
			wantErr: true, errSubstr: "DIFFERENT address",
		},
		{
			name: "all ones is above the order", hexKey: allOnesKey,
			wantErr: true, errSubstr: "curve order N",
		},

		// The pre-existing shape checks must keep working: this guard runs after
		// them and must not have swallowed their messages.
		{name: "empty", hexKey: "", wantErr: true, errSubstr: "empty key"},
		{name: "too short", hexKey: "abcd", wantErr: true, errSubstr: "invalid length"},
		{
			name: "not hex", hexKey: strings.Repeat("z", 64),
			wantErr: true, errSubstr: "invalid hex character",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHexKeyFormat(tt.hexKey)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.errSubstr,
				"the message must name what is wrong: the whole point of this check is that "+
					"the operator otherwise reads 'no key for this supplier' and looks in the wrong place")
		})
	}
}

// TestKeyFileValidateRejectsAZeroedFile drives the guard through the entry point
// production actually uses, so the check is known to be REACHED and not merely
// present. A file of zeros is the realistic way unusable key material arrives.
func TestKeyFileValidateRejectsAZeroedFile(t *testing.T) {
	f := &SupplierKeysFile{Keys: []string{testAppHex, allZeroKey}}

	err := f.Validate()

	require.Error(t, err)
	require.Contains(t, err.Error(), "key[1]", "the operator has to be told WHICH entry")
	require.Contains(t, err.Error(), "the value is zero")
}
