//go:build test

package tx

import (
	"testing"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/stretchr/testify/require"
)

// TestExtractUpoktFeeFromTx validates the fee parser used by the economic
// viability check to observe on-chain fees. It must:
//   - return the upokt amount when present and positive
//   - error when the tx is nil, missing authInfo/fee, or missing upokt
//   - tolerate multi-denom fees and pick only the upokt component
//   - reject zero/negative amounts
func TestExtractUpoktFeeFromTx(t *testing.T) {
	mkTx := func(coins sdk.Coins) *txtypes.Tx {
		fee := &txtypes.Fee{Amount: coins}
		return &txtypes.Tx{
			AuthInfo: &txtypes.AuthInfo{Fee: fee},
		}
	}

	tests := []struct {
		name    string
		tx      *txtypes.Tx
		wantErr bool
		wantVal uint64
	}{
		{
			name:    "nil tx errors",
			tx:      nil,
			wantErr: true,
		},
		{
			name:    "missing authinfo/fee errors",
			tx:      &txtypes.Tx{},
			wantErr: true,
		},
		{
			name: "empty fee amount errors",
			tx: func() *txtypes.Tx {
				return mkTx(sdk.NewCoins())
			}(),
			wantErr: true,
		},
		{
			name:    "valid single upokt",
			tx:      mkTx(sdk.NewCoins(sdk.NewCoin("upokt", sdkmath.NewInt(2)))),
			wantErr: false,
			wantVal: 2,
		},
		{
			name:    "valid large upokt",
			tx:      mkTx(sdk.NewCoins(sdk.NewCoin("upokt", sdkmath.NewInt(12345)))),
			wantErr: false,
			wantVal: 12345,
		},
		{
			name: "multi-denom picks upokt",
			tx: mkTx(sdk.NewCoins(
				sdk.NewCoin("atom", sdkmath.NewInt(100)),
				sdk.NewCoin("upokt", sdkmath.NewInt(7)),
			)),
			wantErr: false,
			wantVal: 7,
		},
		{
			name:    "non-upokt only errors",
			tx:      mkTx(sdk.NewCoins(sdk.NewCoin("atom", sdkmath.NewInt(100)))),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractUpoktFeeFromTx(tt.tx, "/test.Msg")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantVal, got)
		})
	}
}
