package miner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	nodeservice "github.com/cosmos/cosmos-sdk/client/grpc/node"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"google.golang.org/grpc"
)

// startupChainReadTimeout bounds each chain read the miner makes before it may
// start consuming.
const startupChainReadTimeout = 10 * time.Second

var (
	// errNodeOnAnotherChain is returned when the node reports a network other
	// than the chain ID this miner signs its transactions for.
	errNodeOnAnotherChain = errors.New("the node is on another chain")
	// errNoChainHeight is returned when the node answers with no committed height.
	errNoChainHeight = errors.New("the node reported no committed height")
)

// startupChainReaders are the chain reads the miner makes before it may start.
type startupChainReaders struct {
	network func(context.Context) (string, error)
	params  func(context.Context) (*sharedtypes.Params, error)
	height  func(context.Context) (int64, error)
}

// readStartupChainState reads what the miner must know about the chain before it
// consumes a single relay, and returns an error instead of a warning for any of
// it, because the caller does not start without all three:
//
//   - the node's network, which must be chainID. A node of another network hands
//     out that network's heights, params and sessions: the miner would judge
//     claim windows against them and only fail when its claims are refused, a
//     whole session later. It is read first, since nothing after it means
//     anything if it does not match.
//   - the shared params.
//   - the committed height, where the block adapter starts.
func readStartupChainState(
	ctx context.Context,
	chainID string,
	read startupChainReaders,
) (*sharedtypes.Params, int64, error) {
	networkCtx, cancelNetwork := context.WithTimeout(ctx, startupChainReadTimeout)
	network, err := read.network(networkCtx)
	cancelNetwork()
	if err != nil {
		return nil, 0, fmt.Errorf("cannot start without the node's network: %w", err)
	}
	if network != chainID {
		return nil, 0, fmt.Errorf("cannot start: %w: the node reports network %q and this miner is configured for chain %q",
			errNodeOnAnotherChain, network, chainID)
	}

	paramsCtx, cancelParams := context.WithTimeout(ctx, startupChainReadTimeout)
	params, err := read.params(paramsCtx)
	cancelParams()
	if err != nil {
		return nil, 0, fmt.Errorf("cannot start without the chain's shared params: %w", err)
	}

	heightCtx, cancelHeight := context.WithTimeout(ctx, startupChainReadTimeout)
	height, err := read.height(heightCtx)
	cancelHeight()
	if err != nil {
		return nil, 0, fmt.Errorf("cannot start without the chain's committed height: %w", err)
	}
	if height <= 0 {
		return nil, 0, fmt.Errorf("cannot start without the chain's committed height: %w (got %d)", errNoChainHeight, height)
	}
	return params, height, nil
}

// nodeNetworkReader reads the network the node reports
// (cosmos.base.tendermint.v1beta1.Service/GetNodeInfo), the value CometBFT
// calls the chain ID.
func nodeNetworkReader(conn *grpc.ClientConn) func(context.Context) (string, error) {
	node := cmtservice.NewServiceClient(conn)
	return func(ctx context.Context) (string, error) {
		res, err := node.GetNodeInfo(ctx, &cmtservice.GetNodeInfoRequest{})
		if err != nil {
			return "", err
		}
		return res.GetDefaultNodeInfo().GetNetwork(), nil
	}
}

// committedHeightReader reads the height of the node's last committed state
// (cosmos.base.node.v1beta1.Service/Status), which is the state every query is
// answered from. The node's latest block is not that height: while a block is
// being committed it is already one ahead.
func committedHeightReader(conn *grpc.ClientConn) func(context.Context) (int64, error) {
	node := nodeservice.NewServiceClient(conn)
	return func(ctx context.Context) (int64, error) {
		res, err := node.Status(ctx, &nodeservice.StatusRequest{})
		if err != nil {
			return 0, err
		}
		if res.GetHeight() > math.MaxInt64 {
			return 0, fmt.Errorf("the node reported height %d, beyond int64", res.GetHeight())
		}
		return int64(res.GetHeight()), nil
	}
}
