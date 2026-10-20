package miner

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
)

// weightedProofUpgrade is the upgrade plan from which the chain verifies
// weighted proofs; before it, it verifies closest proofs.
const weightedProofUpgrade = "v0.1.36"

// weightedProofRequiredText is how a v0.1.36 chain refuses a closest proof at
// submission (x/proof ensureWeightedProofForm). The refusal reaches the miner
// as the text of a failed simulation, without its error code.
const weightedProofRequiredText = "not a weighted merkle proof"

// ProofShape tells the proof builder which proof the chain verifies: weighted
// once the weightedProofUpgrade plan is applied, closest before. Proofs are
// validated at the height that includes them, and the chain halts at the
// upgrade height, so the plan's state now is the state the next proof meets.
//
// Once seen applied it stays applied. A failed read keeps the last answer, and
// with none yet the answer is closest: a closest proof is refused at
// submission by a chain that wants weighted ones, while a weighted proof is
// accepted by an older chain and only fails when the block is validated.
//
// The plan is not the only way to know: a chain that started on v0.1.36 never
// applies it. That refusal is the other: only a chain that verifies weighted
// proofs sends it, so seeing it settles the answer the same way.
type ProofShape struct {
	logger   logging.Logger
	client   query.UpgradeQueryClient
	weighted atomic.Bool
}

// NewProofShape returns a ProofShape that reads the plan through client.
func NewProofShape(logger logging.Logger, client query.UpgradeQueryClient) *ProofShape {
	return &ProofShape{
		logger: logging.ForComponent(logger, logging.ComponentLifecycleCallback),
		client: client,
	}
}

// Weighted reports whether the next proof must be a weighted one. A nil
// ProofShape, or one without a client, answers true.
func (s *ProofShape) Weighted(ctx context.Context) bool {
	if s == nil || s.client == nil {
		return true
	}
	if s.weighted.Load() {
		return true
	}
	height, err := s.client.AppliedPlanHeight(ctx, weightedProofUpgrade)
	if err != nil {
		s.logger.Warn().Err(err).
			Str("upgrade", weightedProofUpgrade).
			Msg("could not read the upgrade plan; building closest proofs until it is read")
		return false
	}
	if height <= 0 {
		return false
	}
	if s.weighted.CompareAndSwap(false, true) {
		s.logger.Info().
			Str("upgrade", weightedProofUpgrade).
			Int64("applied_height", height).
			Msg("upgrade applied: building weighted proofs from now on")
	}
	return true
}

// LearnFromRefusal reports whether err is the chain refusing a proof for not
// being weighted, and if so makes every later Weighted answer true. A nil
// ProofShape learns nothing: it already answers weighted.
func (s *ProofShape) LearnFromRefusal(err error) bool {
	if s == nil || err == nil || !strings.Contains(err.Error(), weightedProofRequiredText) {
		return false
	}
	if s.weighted.CompareAndSwap(false, true) {
		s.logger.Info().Err(err).
			Msg("the chain refused a closest proof: building weighted proofs from now on")
	}
	return true
}
