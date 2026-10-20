//go:build test

package miner

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	"github.com/pokt-network/smt"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/tx"
)

// scriptedUpgrades answers AppliedPlanHeight from a script, one answer per
// call; the last answer repeats.
type scriptedUpgrades struct {
	mu      sync.Mutex
	answers []upgradeAnswer
	calls   int
}

type upgradeAnswer struct {
	height int64
	err    error
}

func (s *scriptedUpgrades) AppliedPlanHeight(_ context.Context, name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != weightedProofUpgrade {
		return 0, errors.New("unexpected plan " + name)
	}
	a := s.answers[min(s.calls, len(s.answers)-1)]
	s.calls++
	return a.height, a.err
}

func TestProofShape_FollowsTheAppliedPlan(t *testing.T) {
	ctx := context.Background()
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	errNode := errors.New("node unavailable")

	notApplied := NewProofShape(logger, &scriptedUpgrades{answers: []upgradeAnswer{{height: 0}}})
	assert.False(t, notApplied.Weighted(ctx), "a plan not applied builds closest proofs")

	neverRead := NewProofShape(logger, &scriptedUpgrades{answers: []upgradeAnswer{{err: errNode}}})
	assert.False(t, neverRead.Weighted(ctx), "a plan never read builds closest proofs")

	upgrades := &scriptedUpgrades{answers: []upgradeAnswer{{err: errNode}, {height: 0}, {height: 500}, {err: errNode}, {height: 0}}}
	shape := NewProofShape(logger, upgrades)
	assert.False(t, shape.Weighted(ctx), "read failed, nothing known")
	assert.False(t, shape.Weighted(ctx), "not applied")
	assert.True(t, shape.Weighted(ctx), "applied")
	assert.True(t, shape.Weighted(ctx), "applied stays applied")
	assert.True(t, shape.Weighted(ctx), "applied stays applied")
	assert.Equal(t, 3, upgrades.calls, "once applied the plan is not read again")

	var unwired *ProofShape
	assert.True(t, unwired.Weighted(ctx), "a nil shape builds weighted proofs")
	assert.True(t, NewProofShape(logger, nil).Weighted(ctx), "a shape without a client builds weighted proofs")
}

// closestChainVerifies checks a proof as a chain before v0.1.36 does: the proof
// is for the seed's own path (x/proof validateClosestPath) and verifies as a
// closest proof against root.
func closestChainVerifies(t testing.TB, proofBz, root, seed []byte) bool {
	t.Helper()
	ok, proof := chainVerifies(t, proofBz, root)
	return ok && bytes.Equal(proof.Path, seed)
}

// Each proof passes the check of the chain it is built for and fails the
// other's, from a resident tree and from a compacted one.
func TestProve_EachShapePassesOnlyItsChainsCheck(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	const supplier, sessionID = "pokt1shape", "sess-shape"
	mgr := NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
	root := claimColdTree(t, ctx, mgr, sessionID, coldRelays(3, 300))
	seeds := coldPaths(11, 20)

	check := func(name string, m *RedisSMSTManager) {
		differ := 0
		for i, seed := range seeds {
			closestBz, err := m.ProveClosest(ctx, sessionID, seed)
			require.NoError(t, err)
			weightedBz, err := m.ProveWeighted(ctx, sessionID, seed)
			require.NoError(t, err)

			assert.True(t, closestChainVerifies(t, closestBz, root, seed), "%s seed %d: the closest proof passes the older chain", name, i)
			assert.True(t, weightedVerifies(t, weightedBz, root, seed), "%s seed %d: the weighted proof passes v0.1.36", name, i)
			assert.False(t, closestChainVerifies(t, weightedBz, root, seed), "%s seed %d: the weighted proof fails the older chain", name, i)

			closest, err := decompactWeightedProof(closestBz, protocol.NewSMTSpec())
			require.NoError(t, err)
			weighted, err := decompactWeightedProof(weightedBz, protocol.NewSMTSpec())
			require.NoError(t, err)
			if !bytes.Equal(closest.ClosestPath, weighted.ClosestPath) {
				differ++
				ok, err := smt.VerifyWeightedProof(closest, root, seed, protocol.NewSMTSpec())
				assert.False(t, ok && err == nil, "%s seed %d: the closest proof fails v0.1.36", name, i)
			}
		}
		require.NotZero(t, differ, "%s: control: some seeds select different leaves", name)
	}

	check("resident", mgr)

	result, err := mgr.CompactColdTree(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, coldCompacted, result)
	check("compacted", NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{
		SupplierAddress: supplier, CacheTTL: time.Hour, RebuildAdmission: NewRebuildAdmission(zerolog.Nop()),
	}))
}

// shapeSMST records which proof the cycle asked for.
type shapeSMST struct {
	smstStub
	mu       sync.Mutex
	weighted int
	closest  int
}

func (s *shapeSMST) ProveWeighted(context.Context, string, []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.weighted++
	return []byte("weighted-proof"), nil
}

func (s *shapeSMST) ProveClosest(context.Context, string, []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closest++
	return []byte("closest-proof"), nil
}

func TestOnSessionsNeedProof_BuildsTheShapeTheChainVerifies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		height        int64
		wantWeighted  int
		wantClosest   int
		wantSubmitted int
	}{
		{name: "before the upgrade", height: 0, wantClosest: 1, wantSubmitted: 1},
		{name: "after the upgrade", height: 900, wantWeighted: 1, wantSubmitted: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supplier := &flakySupplier{}
			lc, store := nodeBehindCallback(t, 108, &nodeBehindSupplier{})
			smst := &shapeSMST{}
			lc.smstManager = smst
			lc.supplierClient = supplier
			lc.proofShape = NewProofShape(lc.logger, &scriptedUpgrades{answers: []upgradeAnswer{{height: tc.height}}})
			snapshot := storedSession(t, store, "proof-shape", SessionStateProving)

			_, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snapshot})
			require.NoError(t, err)
			assert.Equal(t, tc.wantWeighted, smst.weighted, "weighted proofs built")
			assert.Equal(t, tc.wantClosest, smst.closest, "closest proofs built")
			assert.Equal(t, tc.wantSubmitted, supplier.calls, "submissions")
		})
	}
}

// The chain crosses the upgrade while a closest batch is being retried: the
// batch is not sent again, and its session goes back to claimed so the next
// block builds a weighted proof.
func TestOnSessionsNeedProof_AShapeChangeReturnsTheGroupToClaimed(t *testing.T) {
	supplier := &flakySupplier{failures: 1}
	lc, store := nodeBehindCallback(t, 108, &nodeBehindSupplier{})
	lc.smstManager = &shapeSMST{}
	lc.supplierClient = supplier
	// Build, attempt 1, attempt 2.
	lc.proofShape = NewProofShape(lc.logger, &scriptedUpgrades{answers: []upgradeAnswer{{height: 0}, {height: 0}, {height: 107}}})
	snapshot := storedSession(t, store, "proof-shape-change", SessionStateProving)

	result, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snapshot})
	require.NoError(t, err, "a group returned to the next block is not a failed cycle")
	_, settled := result.Settled["proof-shape-change"]
	assert.False(t, settled)
	assert.Equal(t, 1, supplier.calls, "the closest batch is not sent after the upgrade")
	requireState(t, store, snapshot, SessionStateClaimed)
}

// refusingSupplier refuses its first `refusals` proof submissions with err, then
// accepts.
type refusingSupplier struct {
	flakySupplier
	refusals int
	err      error
}

func (r *refusingSupplier) SubmitProofsReturningHash(_ context.Context, _ int64, _ ...pocktclient.MsgSubmitProof) (string, tx.SignedTxPayload, error) {
	r.calls++
	if r.calls <= r.refusals {
		return "", tx.SignedTxPayload{}, r.err
	}
	hash, signed := fakeSigned("proof")
	return hash, signed, nil
}

// errClosestRefused is the error a v0.1.36 node returned for a closest proof,
// copied from a live run on the Tilt localnet.
var errClosestRefused = errors.New("failed to broadcast proofs: gas simulation failed (gas_limit=0 requires successful simulation): " +
	"simulation failed: rpc error: code = Unknown desc = failed to execute message; message index: 0: " +
	"rpc error: code = FailedPrecondition desc = proof is not a weighted merkle proof: invalid proof " +
	"[cosmos/cosmos-sdk@v0.53.8/baseapp/baseapp.go:1052] with gas used: '169107'")

// A chain that started on v0.1.36 never applies the plan, and a read can fail
// after the upgrade: either way the miner builds closest proofs, and the chain's
// refusal is what tells it. The group goes back to claimed, and the next cycle
// builds weighted proofs, every cycle after it too.
func TestOnSessionsNeedProof_AClosestRefusalTeachesTheShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer upgradeAnswer
	}{
		{name: "plan never applied", answer: upgradeAnswer{height: 0}},
		{name: "plan never read", answer: upgradeAnswer{err: errors.New("node unavailable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			supplier := &refusingSupplier{refusals: 1, err: errClosestRefused}
			lc, store := nodeBehindCallback(t, 108, &nodeBehindSupplier{})
			smst := &shapeSMST{}
			lc.smstManager = smst
			lc.supplierClient = supplier
			lc.proofShape = NewProofShape(lc.logger, &scriptedUpgrades{answers: []upgradeAnswer{tc.answer}})
			snapshot := storedSession(t, store, "proof-refused", SessionStateProving)

			result, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snapshot})
			require.NoError(t, err, "a group returned to the next block is not a failed cycle")
			_, settled := result.Settled["proof-refused"]
			assert.False(t, settled)
			assert.Equal(t, 1, supplier.calls, "the refused closest batch is not sent again")
			assert.Equal(t, 1, smst.closest)
			requireState(t, store, snapshot, SessionStateClaimed)
			require.True(t, lc.proofShape.Weighted(context.Background()), "the refusal settles the shape")

			snapshot = storedSession(t, store, "proof-refused-next", SessionStateProving)
			_, err = lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snapshot})
			require.NoError(t, err)
			assert.Equal(t, 1, smst.weighted, "the next cycle builds a weighted proof")
			assert.Equal(t, 1, smst.closest, "and no closest one")
			assert.Equal(t, 2, supplier.calls, "the weighted batch is submitted")
		})
	}
}

// Control: another invalid-proof refusal says nothing about the shape.
func TestOnSessionsNeedProof_OtherRefusalsKeepTheShape(t *testing.T) {
	supplier := &refusingSupplier{refusals: 1, err: errors.New("rpc error: code = FailedPrecondition desc = relay difficulty below the target: invalid proof")}
	lc, store := nodeBehindCallback(t, 108, &nodeBehindSupplier{})
	smst := &shapeSMST{}
	lc.smstManager = smst
	lc.supplierClient = supplier
	lc.proofShape = NewProofShape(lc.logger, &scriptedUpgrades{answers: []upgradeAnswer{{height: 0}}})
	snapshot := storedSession(t, store, "proof-other-refusal", SessionStateProving)

	_, err := lc.OnSessionsNeedProof(context.Background(), []*SessionSnapshot{snapshot})
	require.NoError(t, err)
	assert.Equal(t, 2, supplier.calls, "an ordinary refusal is retried")
	assert.False(t, lc.proofShape.Weighted(context.Background()), "and teaches nothing")
}
