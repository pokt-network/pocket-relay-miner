//go:build test

package query

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	accounttypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// accountAnswers answers the auth Account query from a fixed table.
type accountAnswers struct {
	accounttypes.UnimplementedQueryServer
	accounts map[string]*accounttypes.BaseAccount
	errs     map[string]error
}

func (s *accountAnswers) Account(_ context.Context, req *accounttypes.QueryAccountRequest) (*accounttypes.QueryAccountResponse, error) {
	if err, ok := s.errs[req.Address]; ok {
		return nil, err
	}
	acc, ok := s.accounts[req.Address]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "account %s not found", req.Address)
	}
	anyAcc, err := codectypes.NewAnyWithValue(acc)
	if err != nil {
		return nil, err
	}
	return &accounttypes.QueryAccountResponse{Account: anyAcc}, nil
}

func TestGetPubKeyFromAddress_TellsAbsenceFromFailure(t *testing.T) {
	withKey := secp256k1.GenPrivKey().PubKey()
	withKeyAddr := cosmostypes.AccAddress(withKey.Address()).String()
	noKeyAddr := cosmostypes.AccAddress(secp256k1.GenPrivKey().PubKey().Address()).String()
	missingAddr := cosmostypes.AccAddress(secp256k1.GenPrivKey().PubKey().Address()).String()
	failingAddr := cosmostypes.AccAddress(secp256k1.GenPrivKey().PubKey().Address()).String()

	withKeyAcc := accounttypes.NewBaseAccountWithAddress(cosmostypes.MustAccAddressFromBech32(withKeyAddr))
	require.NoError(t, withKeyAcc.SetPubKey(withKey))
	answers := &accountAnswers{
		accounts: map[string]*accounttypes.BaseAccount{
			withKeyAddr: withKeyAcc,
			noKeyAddr:   accounttypes.NewBaseAccountWithAddress(cosmostypes.MustAccAddressFromBech32(noKeyAddr)),
		},
		errs: map[string]error{failingAddr: status.Error(codes.Unavailable, "node unavailable")},
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	accounttypes.RegisterQueryServer(server, answers)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		<-served
	})

	qc, err := NewQueryClients(logging.NewLoggerFromConfig(logging.DefaultConfig()), ClientConfig{
		GRPCEndpoint: listener.Addr().String(),
		QueryTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })
	ctx := context.Background()

	got, err := qc.Account().GetPubKeyFromAddress(ctx, withKeyAddr)
	require.NoError(t, err)
	require.True(t, withKey.Equals(got))

	_, err = qc.Account().GetPubKeyFromAddress(ctx, noKeyAddr)
	require.ErrorIs(t, err, ErrAccountPubKeyNotFound)
	require.False(t, IsEntityNotFound(err))

	_, err = qc.Account().GetPubKeyFromAddress(ctx, missingAddr)
	require.True(t, IsEntityNotFound(err), "got %v", err)
	require.False(t, errors.Is(err, ErrAccountPubKeyNotFound))

	_, err = qc.Account().GetPubKeyFromAddress(ctx, failingAddr)
	require.Error(t, err)
	require.False(t, IsEntityNotFound(err))
	require.False(t, errors.Is(err, ErrAccountPubKeyNotFound))
	require.Equal(t, codes.Unavailable, status.Code(errors.Unwrap(err)))
}
