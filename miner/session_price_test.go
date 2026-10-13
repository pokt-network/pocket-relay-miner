//go:build test

package miner

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// perMillionPricer prices a session at one uPOKT per million compute units:
// for tests about which book money goes to, not what it is priced at.
type perMillionPricer struct{}

func (perMillionPricer) Quote(_ context.Context, snap *SessionSnapshot) Upokt {
	return Upokt{Amount: snap.TotalComputeUnits / 1_000_000, OK: true}
}

// heightsAsked records the heights a price was read at.
type heightsAsked struct {
	mu      sync.Mutex
	shared  []int64
	service []int64
}

func (h *heightsAsked) pricer(t *testing.T, cuttm, granularity uint64, sharedErr error) SessionPricer {
	t.Helper()
	shared := &mockSharedQueryClient{paramsAtHeightFn: func(_ context.Context, height int64) (*sharedtypes.Params, error) {
		h.mu.Lock()
		h.shared = append(h.shared, height)
		h.mu.Unlock()
		if sharedErr != nil {
			return nil, sharedErr
		}
		return &sharedtypes.Params{ComputeUnitsToTokensMultiplier: cuttm, ComputeUnitCostGranularity: granularity}, nil
	}}
	difficulty := difficultyAt(func(height int64) {
		h.mu.Lock()
		h.service = append(h.service, height)
		h.mu.Unlock()
	})
	checker := NewProofRequirementChecker(logging.NewLoggerFromConfig(logging.DefaultConfig()), nil, shared, difficulty)
	m := &SupplierManager{config: SupplierManagerConfig{SharedClient: shared, ProofChecker: checker}}
	pricer := m.pricer()
	require.NotNil(t, pricer, "the manager prices with the chain's params when it has both clients")
	return pricer
}

// difficultyAt answers the base relay-mining difficulty for any service and
// reports the height it was asked at.
type difficultyAt func(height int64)

func (d difficultyAt) GetServiceRelayDifficultyAtHeight(_ context.Context, serviceID string, height int64) (servicetypes.RelayMiningDifficulty, error) {
	d(height)
	return servicetypes.RelayMiningDifficulty{ServiceId: serviceID, TargetHash: protocol.BaseRelayDifficultyHashBz}, nil
}

func smstRoot(sum, count uint64) []byte {
	root := make([]byte, SMSTRootLen)
	root[0] = 0xab
	binary.BigEndian.PutUint64(root[SMSTRootLen-16:], sum)
	binary.BigEndian.PutUint64(root[SMSTRootLen-8:], count)
	return root
}

// A session is priced as the chain settles its claim: compute units x CUTTM /
// granularity, floored, under the params and difficulty at its START height,
// on its claimed root when it has one. Each case is the chain's figure worked
// out by hand.
func TestSessionPrice_IsWhatTheChainSettles(t *testing.T) {
	cases := []struct {
		name        string
		cuttm, gran uint64
		cu, relays  uint64
		claimedRoot []byte
		want        uint64
	}{
		// The localnet: 100 uPOKT per compute unit. 3,024 relays of 10 CU each
		// settled 302,400,000 uPOKT; this is one session of them.
		{name: "localnet 100 uPOKT per CU", cuttm: 100, gran: 1, cu: 10_080, relays: 1_008, want: 1_008_000},
		// A fractional price: 123,457 x 42 / 1e6 = 5.185194, floored to 5.
		{name: "fractional price floors as the chain does", cuttm: 42, gran: 1_000_000, cu: 123_457, relays: 7, want: 5},
		// The claimed root is what the chain prices, even when the session's
		// counter says otherwise.
		{name: "the claimed root wins over the counters", cuttm: 100, gran: 1, cu: 10_080, relays: 1_008,
			claimedRoot: smstRoot(2_000, 200), want: 200_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked heightsAsked
			pricer := asked.pricer(t, tc.cuttm, tc.gran, nil)
			snap := &SessionSnapshot{
				SessionID: "s1", SupplierOperatorAddress: "pokt1price", ServiceID: "develop-http",
				SessionStartHeight: 101, SessionEndHeight: 110,
				TotalComputeUnits: tc.cu, RelayCount: int64(tc.relays), ClaimedRootHash: tc.claimedRoot,
			}
			require.Equal(t, Upokt{Amount: tc.want, OK: true}, pricer.Quote(context.Background(), snap))
			require.Equal(t, []int64{101}, asked.shared, "shared params at the session START height, as the chain reads them")
			require.Equal(t, []int64{101}, asked.service, "difficulty at the session START height, as the chain reads it")
		})
	}
}

func TestSessionPrice_UnreadableParamsLeaveItUnpriced(t *testing.T) {
	var asked heightsAsked
	pricer := asked.pricer(t, 100, 1, errors.New("node unreachable"))
	snap := &SessionSnapshot{ServiceID: "develop-http", SessionStartHeight: 5, TotalComputeUnits: 10, RelayCount: 1}
	require.Equal(t, Upokt{}, pricer.Quote(context.Background(), snap))

	require.Equal(t, Upokt{OK: true}, pricer.Quote(context.Background(), &SessionSnapshot{ServiceID: "develop-http"}),
		"a session with no work is worth 0 uPOKT without asking the chain")
	require.Equal(t, Upokt{}, quote(context.Background(), nil, snap), "no pricer prices nothing")

	m := &SupplierManager{}
	require.Nil(t, m.pricer(), "without the chain's clients there is no price")
}

// The money series carry the price; a session that could not be priced adds
// its compute units to unpriced_compute_units_total and nothing to the uPOKT
// series.
func TestMoneySeries_CarryThePriceOrCountTheWorkUnpriced(t *testing.T) {
	const supplier, service = "pokt1price_series", "develop-http"
	claimed := func() float64 { return testutil.ToFloat64(upoktClaimedTotal.WithLabelValues(supplier, service)) }
	unpriced := func(book string) float64 {
		return testutil.ToFloat64(unpricedComputeUnitsTotal.WithLabelValues(supplier, service, book))
	}
	lost := func() float64 {
		return testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, "proof_window_closed"))
	}

	claimedBefore, lostBefore, unpricedBefore := claimed(), lost(), unpriced("claimed")
	RecordRevenueClaimed(supplier, service, 10_080, 1_008, Upokt{Amount: 1_008_000, OK: true})
	require.Equal(t, claimedBefore+1_008_000, claimed(), "uPOKT as the chain settles it, not compute units / 1e6")
	RecordProofWindowClosed(supplier, service, "", 1_008, 10_080, Upokt{Amount: 1_008_000, OK: true})
	require.Equal(t, lostBefore+1_008_000, lost())

	RecordRevenueClaimed(supplier, service, 77, 7, Upokt{})
	require.Equal(t, claimedBefore+1_008_000, claimed(), "an unpriced session adds no guess to the uPOKT series")
	require.Equal(t, unpricedBefore+77, unpriced("claimed"), "its compute units are counted unpriced, under the book they miss")
}

// The lifecycle records a session's money at the chain's price: a proof window
// that closes on a claimed session loses what the chain would have paid.
func TestLifecycle_RecordsMoneyAtTheChainsPrice(t *testing.T) {
	const supplier, service = "pokt1price_lifecycle", "develop-http"
	f := newHandlerTestFixture(t, supplier)
	var asked heightsAsked
	lc := &LifecycleCallback{
		logger:      logging.NewLoggerFromConfig(logging.DefaultConfig()),
		smstManager: f.smstMgr,
		pricer:      asked.pricer(t, 100, 1, nil),
	}
	lost := func() float64 {
		return testutil.ToFloat64(upoktLostTotal.WithLabelValues(supplier, service, "proof_window_closed"))
	}
	before := lost()

	require.NoError(t, lc.OnProofWindowClosed(context.Background(), &SessionSnapshot{
		SessionID: "sess-price-lifecycle", SupplierOperatorAddress: supplier, ServiceID: service,
		SessionStartHeight: 101, SessionEndHeight: 110, RelayCount: 1_008, TotalComputeUnits: 10_080,
	}))
	require.Equal(t, before+1_008_000, lost(), "10,080 compute units at 100 uPOKT each")
	require.Equal(t, []int64{101}, asked.shared)
}
