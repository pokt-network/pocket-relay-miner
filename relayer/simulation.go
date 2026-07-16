package relayer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	ringtypes "github.com/pokt-network/go-dleq/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/rings"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Simulation admission sentinel errors. Each distinct rejection reason has its
// own error so callers (and tests) can branch with errors.Is, and so the
// simulated-relay metric can label the outcome precisely.
var (
	ErrSimUnknownKeyID      = errors.New("simulation: unknown key_id")
	ErrSimDisabled          = errors.New("simulation: identity disabled")
	ErrSimExpired           = errors.New("simulation: identity expired (not_after)")
	ErrSimSupplierMissing   = errors.New("simulation: supplier operator address required")
	ErrSimSupplierNotLoaded = errors.New("simulation: supplier signing key not loaded on this relayer")
	ErrSimServiceUnknown    = errors.New("simulation: service not configured on this relayer")
	ErrSimServiceNotAllowed = errors.New("simulation: service not allowed for this identity")
	ErrSimIdentityMismatch  = errors.New("simulation: request application address does not match pinned identity")
	ErrSimBadSessionID      = errors.New("simulation: malformed synthetic session id")
	ErrSimStale             = errors.New("simulation: request timestamp outside freshness window")
	ErrSimReplay            = errors.New("simulation: request already seen (replay)")
	ErrSimDedupUnavailable  = errors.New("simulation: replay dedup store unavailable")
	ErrSimBadSignature      = errors.New("simulation: ring signature verification failed")
)

// simSessionIDPrefix tags the synthetic session id format used ONLY by
// simulated relays: "simv1:<unixSeconds>:<hexNonce>". The timestamp lives
// inside the ring-signed session id (so it is tamper-proof), letting the
// relayer enforce a freshness window against its OWN clock. The CLI builder
// (BuildSimulatedRelayRequest) MUST emit this exact format.
const simSessionIDPrefix = "simv1"

// FormatSimSessionID builds the synthetic session id embedding a caller
// timestamp + nonce. Shared format contract between the signer (CLI) and the
// relayer verifier.
func FormatSimSessionID(ts time.Time, nonceHex string) string {
	return fmt.Sprintf("%s:%d:%s", simSessionIDPrefix, ts.Unix(), nonceHex)
}

// parseSimSessionID extracts the caller unix-seconds timestamp from a synthetic
// session id. Returns ErrSimBadSessionID on any malformed input.
func parseSimSessionID(sessionID string) (int64, error) {
	parts := strings.Split(sessionID, ":")
	if len(parts) != 3 || parts[0] != simSessionIDPrefix || parts[2] == "" {
		return 0, ErrSimBadSessionID
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, ErrSimBadSessionID
	}
	return ts, nil
}

// SimulationDirective is the transport-agnostic simulation signal, extracted
// once at the transport entry. An empty KeyID means the request is a normal
// (non-simulated) relay.
type SimulationDirective struct {
	KeyID string
}

// tokenBucket is a minimal, clock-injectable token bucket used for the
// per-identity request-rate cap (R2). A custom bucket (rather than
// golang.org/x/time/rate) keeps tests deterministic without sleeping: the
// clock is injected.
type tokenBucket struct {
	mu           sync.Mutex
	tokens       float64
	max          float64
	refillPerSec float64
	last         time.Time
	now          func() time.Time
}

func newTokenBucket(rps int, now func() time.Time) *tokenBucket {
	return &tokenBucket{
		tokens:       float64(rps),
		max:          float64(rps),
		refillPerSec: float64(rps),
		last:         now(),
		now:          now,
	}
}

// allow refills based on elapsed time and consumes one token; returns false if
// no token is available.
func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.now()
	elapsed := t.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(b.max, b.tokens+elapsed*b.refillPerSec)
		b.last = t
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// simIdentity is a fully precomputed, in-memory pinned identity. Everything
// needed to admit a simulated relay is resolved at config load, so per-relay
// verification is an in-memory lookup with no file/Redis read for identity
// resolution (the only per-request Redis touch is the replay dedup).
type simIdentity struct {
	keyID           string
	enabled         bool
	notAfter        time.Time // zero => no expiry
	appAddress      string
	ringPoints      map[string]ringtypes.Point
	allowedServices map[string]struct{} // empty => all configured services
	bucket          *tokenBucket
}

// SimulationVerifier owns the simulation Admission zone: the in-memory pinned
// identity map, the global concurrency semaphore, per-identity rate limiting,
// and per-relay verification (identity binding + freshness + replay dedup +
// pinned-ring signature). It never touches the shared data path.
type SimulationVerifier struct {
	logger     logging.Logger
	redis      *redisutil.Client
	signer     *ResponseSigner
	serviceIDs map[string]struct{} // configured services; nil => skip existence check
	windowSecs int
	now        func() time.Time

	identities atomic.Pointer[map[string]*simIdentity] // swapped atomically on reload
	enabled    atomic.Bool
	globalSem  chan struct{} // sized at construction; max_concurrent change needs restart
}

// NewSimulationVerifier builds the verifier from validated config. serviceIDs
// is the set of configured service IDs (nil => skip the "service configured"
// check and rely on routing). clock defaults to time.Now when nil.
func NewSimulationVerifier(
	logger logging.Logger,
	cfg *SimulationConfig,
	redis *redisutil.Client,
	signer *ResponseSigner,
	serviceIDs map[string]struct{},
	clock func() time.Time,
) (*SimulationVerifier, error) {
	if clock == nil {
		clock = time.Now
	}
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = 32
	}
	window := cfg.FreshnessWindowSeconds
	if window <= 0 {
		window = 30
	}

	v := &SimulationVerifier{
		logger:     logging.ForComponent(logger, "simulation_verifier"),
		redis:      redis,
		signer:     signer,
		serviceIDs: serviceIDs,
		windowSecs: window,
		now:        clock,
		globalSem:  make(chan struct{}, maxConc),
	}

	ids, err := buildSimIdentities(cfg, clock)
	if err != nil {
		return nil, err
	}
	v.identities.Store(&ids)
	v.enabled.Store(cfg.Enabled)
	return v, nil
}

// buildSimIdentities precomputes the in-memory identity map from config. The
// config must already have passed SimulationConfig.Validate (placeholder
// rejected, pubkeys parse), so parse errors here are unexpected but still
// surfaced.
func buildSimIdentities(cfg *SimulationConfig, clock func() time.Time) (map[string]*simIdentity, error) {
	out := make(map[string]*simIdentity, len(cfg.Identities))
	for _, idc := range cfg.Identities {
		appPub, err := rings.PubKeyFromHex(idc.AppPubKeyHex)
		if err != nil {
			return nil, fmt.Errorf("simulation identity %q: app pubkey: %w", idc.KeyID, err)
		}
		// Derive the app address with the ACTIVE SDK bech32 prefix so it matches
		// both SessionHeader.ValidateBasic (which uses sdk.AccAddressFromBech32)
		// and the address the request builder emits. In production the active
		// prefix is "pokt".
		appAddr := cosmostypes.AccAddress(appPub.Address()).String()

		pubKeys := []cryptotypes.PubKey{appPub}
		for _, gwHex := range idc.GatewayPubKeysHex {
			gwPub, err := rings.PubKeyFromHex(gwHex)
			if err != nil {
				return nil, fmt.Errorf("simulation identity %q: gateway pubkey: %w", idc.KeyID, err)
			}
			pubKeys = append(pubKeys, gwPub)
		}
		points, err := rings.PrecomputeRingPoints(pubKeys)
		if err != nil {
			return nil, fmt.Errorf("simulation identity %q: precompute ring points: %w", idc.KeyID, err)
		}

		allowed := make(map[string]struct{}, len(idc.AllowedServices))
		for _, s := range idc.AllowedServices {
			allowed[s] = struct{}{}
		}

		var notAfter time.Time
		if idc.NotAfter != "" {
			notAfter, err = time.Parse(time.RFC3339, idc.NotAfter)
			if err != nil {
				return nil, fmt.Errorf("simulation identity %q: not_after: %w", idc.KeyID, err)
			}
		}

		rps := idc.MaxRPS
		if rps <= 0 {
			rps = 5
		}

		out[idc.KeyID] = &simIdentity{
			keyID:           idc.KeyID,
			enabled:         idc.Enabled,
			notAfter:        notAfter,
			appAddress:      appAddr,
			ringPoints:      points,
			allowedServices: allowed,
			bucket:          newTokenBucket(rps, clock),
		}
	}
	return out, nil
}

// Enabled reports whether simulation serving is on. When false, the transport
// entry MUST ignore the simulation header and serve the normal path (R7).
func (v *SimulationVerifier) Enabled() bool { return v.enabled.Load() }

// AcquireGlobal takes a global concurrency slot BEFORE verification (R2, bounds
// CPU/backend). Returns a release func and ok=false when the fleet-local
// concurrency budget is exhausted. Release must be deferred on every exit path.
func (v *SimulationVerifier) AcquireGlobal() (release func(), ok bool) {
	select {
	case v.globalSem <- struct{}{}:
		return func() { <-v.globalSem }, true
	default:
		return nil, false
	}
}

// AllowKey charges the per-identity token bucket AFTER a request verifies (R2).
// Charging post-verify means the public, non-secret key_id cannot be used by a
// keyless attacker to starve a legitimate identity's budget. Unknown key_id
// returns false.
func (v *SimulationVerifier) AllowKey(keyID string) bool {
	m := *v.identities.Load()
	id, ok := m[keyID]
	if !ok {
		return false
	}
	return id.bucket.allow()
}

// Verify performs simulation admission: identity lookup, request ValidateBasic,
// supplier presence + loaded signing key, identity binding (app address +
// allowed service), freshness (timestamp window + shared replay dedup), and the
// pinned-ring signature check. All checks are adjudicated against relayer-side
// state; every caller-supplied field is untrusted input. Returns nil on
// admission, or one of the Err Sim* sentinels.
func (v *SimulationVerifier) Verify(ctx context.Context, keyID string, rr *servicetypes.RelayRequest) error {
	m := *v.identities.Load()
	id, ok := m[keyID]
	if !ok {
		return ErrSimUnknownKeyID
	}
	if !id.enabled {
		return ErrSimDisabled
	}
	if !id.notAfter.IsZero() && v.now().After(id.notAfter) {
		return ErrSimExpired
	}

	meta := rr.GetMeta()

	// Supplier presence + loaded signing key checked first so the rejection is
	// precise (rather than a generic ValidateBasic wrap).
	supplier := meta.GetSupplierOperatorAddress()
	if supplier == "" {
		return ErrSimSupplierMissing
	}
	if !v.signer.HasSigner(supplier) {
		return ErrSimSupplierNotLoaded
	}

	if err := rr.ValidateBasic(); err != nil {
		return fmt.Errorf("%w: %v", ErrSimBadSignature, err)
	}

	sh := meta.GetSessionHeader()

	// R4 — bind the request to the pinned identity.
	if sh.GetApplicationAddress() != id.appAddress {
		return ErrSimIdentityMismatch
	}
	svc := sh.GetServiceId()
	if v.serviceIDs != nil {
		if _, ok := v.serviceIDs[svc]; !ok {
			return ErrSimServiceUnknown
		}
	}
	if len(id.allowedServices) > 0 {
		if _, ok := id.allowedServices[svc]; !ok {
			return ErrSimServiceNotAllowed
		}
	}

	// R3 — freshness: reject stale/future timestamps against the relayer clock,
	// then dedup the signature in shared Redis for the window (fleet-wide).
	ts, err := parseSimSessionID(sh.GetSessionId())
	if err != nil {
		return err
	}
	nowUnix := v.now().Unix()
	delta := nowUnix - ts
	if delta < 0 {
		delta = -delta
	}
	if delta >= int64(v.windowSecs) {
		return ErrSimStale
	}
	if err := v.checkReplay(ctx, meta.GetSignature()); err != nil {
		return err
	}

	// R5 — pinned-ring signature (identical crypto to the real path, anchored to
	// config-pinned points instead of on-chain).
	if err := rings.VerifySignatureAgainstPoints(rr, id.ringPoints); err != nil {
		return fmt.Errorf("%w: %v", ErrSimBadSignature, err)
	}
	return nil
}

// checkReplay atomically records the signature hash in shared Redis with a TTL
// equal to the freshness window. First-seen => admit; already-present =>
// ErrSimReplay. A Redis error fails CLOSED (ErrSimDedupUnavailable): within the
// window, replay protection depends on this shared state, so a degraded store
// must not silently allow replays.
func (v *SimulationVerifier) checkReplay(ctx context.Context, signature []byte) error {
	sum := sha256.Sum256(signature)
	key := v.redis.KB().SimulationReplayKey(hex.EncodeToString(sum[:]))
	ttl := time.Duration(v.windowSecs) * time.Second
	set, err := v.redis.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSimDedupUnavailable, err)
	}
	if !set {
		return ErrSimReplay
	}
	return nil
}

// Reload atomically swaps the identity map and the enabled flag from new
// config. The global concurrency semaphore is fixed at construction; changing
// max_concurrent requires a restart.
func (v *SimulationVerifier) Reload(cfg *SimulationConfig) error {
	ids, err := buildSimIdentities(cfg, v.now)
	if err != nil {
		return err
	}
	v.identities.Store(&ids)
	v.enabled.Store(cfg.Enabled)
	v.logger.Info().Int("identities", len(ids)).Bool("enabled", cfg.Enabled).Msg("simulation config reloaded")
	return nil
}
