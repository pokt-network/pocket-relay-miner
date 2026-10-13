package miner

import (
	"context"
	"encoding/binary"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"

	"github.com/pokt-network/pocket-relay-miner/query"
)

// Upokt is a session's money as the chain prices it. OK false means it could
// not be priced: the money metrics then count its compute units as unpriced
// instead of adding a guess to the uPOKT series.
type Upokt struct {
	Amount uint64
	OK     bool
}

// SessionPricer prices a session's work in uPOKT.
type SessionPricer interface {
	Quote(ctx context.Context, snap *SessionSnapshot) Upokt
}

// chainPricer prices a session with the chain's own formula: the claimed uPOKT
// of a claim (prooftypes.Claim.GetClaimeduPOKT) under the shared params and the
// relay-mining difficulty effective at the session START height, as the chain
// settles it. Both are immutable at a past height and cached by the query
// layer, so a quote is the same however often, and after a restart.
type chainPricer struct {
	shared     pocktclient.SharedQueryClient
	difficulty query.ServiceDifficultyClient
}

// NewChainPricer returns a SessionPricer over the chain's params; nil when a
// client is missing, which prices nothing.
func NewChainPricer(shared pocktclient.SharedQueryClient, difficulty query.ServiceDifficultyClient) SessionPricer {
	if shared == nil || difficulty == nil {
		return nil
	}
	return &chainPricer{shared: shared, difficulty: difficulty}
}

func (p *chainPricer) Quote(ctx context.Context, snap *SessionSnapshot) Upokt {
	if snap.TotalComputeUnits == 0 {
		return Upokt{OK: true}
	}
	params, err := p.shared.GetParamsAtHeight(ctx, snap.SessionStartHeight)
	if err != nil || params == nil {
		return Upokt{}
	}
	difficulty, err := p.difficulty.GetServiceRelayDifficultyAtHeight(ctx, snap.ServiceID, snap.SessionStartHeight)
	if err != nil {
		return Upokt{}
	}
	claim := prooftypes.Claim{
		SessionHeader: &sessiontypes.SessionHeader{ServiceId: snap.ServiceID},
		RootHash:      priceRoot(snap),
	}
	coin, err := claim.GetClaimeduPOKT(*params, difficulty)
	if err != nil || coin.IsNil() || !coin.Amount.IsUint64() {
		return Upokt{}
	}
	return Upokt{Amount: coin.Amount.Uint64(), OK: true}
}

// priceRoot is the root the session is priced on: its claimed SMST root when it
// has one, which is what the chain prices; otherwise a root carrying its compute
// units and relay count, the only parts of a root the price formula reads.
func priceRoot(snap *SessionSnapshot) []byte {
	if len(snap.ClaimedRootHash) == SMSTRootLen {
		return snap.ClaimedRootHash
	}
	root := make([]byte, SMSTRootLen)
	binary.BigEndian.PutUint64(root[SMSTRootLen-16:], snap.TotalComputeUnits)
	binary.BigEndian.PutUint64(root[SMSTRootLen-8:], uint64(snap.RelayCount))
	return root
}

// quote prices snap with pricer, or reports it unpriced without one.
func quote(ctx context.Context, pricer SessionPricer, snap *SessionSnapshot) Upokt {
	if pricer == nil || snap == nil {
		return Upokt{}
	}
	return pricer.Quote(ctx, snap)
}
