package rings

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	ring_secp256k1 "github.com/pokt-network/go-dleq/secp256k1"
	ringtypes "github.com/pokt-network/go-dleq/types"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	protocolrings "github.com/pokt-network/poktroll/pkg/crypto/rings"
	"github.com/pokt-network/ring-go"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/pokt-network/poktroll/pkg/client"
	"github.com/pokt-network/poktroll/pkg/crypto"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	"github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

var (
	_ crypto.RingClient = (*ringClient)(nil)

	// ringCurve is the elliptic curve used for the ring signatures.
	// It is used to deserialize the ring signatures.
	// DEV_NOTE: This is initialized as a package-level variable to avoid re-initializing for every RelayRequest verification.
	ringCurve = ring_secp256k1.NewCurve()
)

// ringPointsCacheKey is the key for caching ring points by app address and session.
// Ring points are cached because:
// - Converting public keys to curve points (DecodeToPoint) is cryptographically expensive
// - Ring composition only changes at session boundaries (delegation changes)
// - At 1000 RPS, this prevents 2000-4000 DecodeToPoint calls/sec
type ringPointsCacheKey struct {
	appAddress       string
	sessionEndHeight int64
}

// ringPointsCacheEntry is a cached ring-points map plus the session end height it
// was keyed at (for window-bounded eviction) and the time it was computed (for the
// TTL safety floor). numMembers is the ring's member count before dedup: a ring
// can list a key more than once (the application key twice when it has no
// gateway), so it can be larger than len(points).
type ringPointsCacheEntry struct {
	points           map[string]ringtypes.Point
	numMembers       int
	sessionEndHeight int64
	cachedAt         time.Time
}

// ringPointsCacheTTL is the max-age floor on a cached ring-points entry. Ring
// composition is fixed for a given (app, sessionEndHeight) by the protocol
// (delegations are session-boundary effective), so this floor is long — it does
// NOT exist to follow mid-session delegation changes (there are none). It exists
// so a one-time bad compute (e.g. the entity cache served a stale delegation set
// that has since self-healed) cannot be frozen for the process lifetime: worst
// case it lives one TTL, then is recomputed from fresh state. var (not const) so
// tests can shrink it without sleeping.
var ringPointsCacheTTL = 30 * time.Minute

// ringPointsCacheKeepHeights bounds the cache's height window. Without it the
// sync.Map grows one entry per distinct (app, sessionEndHeight) for the process
// lifetime — a slow but unbounded leak (one entry per app per session, forever).
// Storing prunes entries whose session end height is more than this many blocks
// below the newest seen. The window is generous so ring verification during the
// claim/proof windows of recent sessions still hits cache.
const ringPointsCacheKeepHeights = 600

// ringClient is an implementation of the RingClient interface that uses the
// client.ApplicationQueryClient to get application's delegation information
// needed to construct the ring for signing relay requests.
//
// Ring Points Caching: We cache ring points (map[string]ringtypes.Point) by (appAddress, sessionEndHeight) because:
// - Converting public keys to curve points (DecodeToPoint) is cryptographically expensive
// - Ring composition only changes at session boundaries (delegation changes)
// - At 1000 RPS, this prevents 2000-4000 DecodeToPoint calls/sec (N keys per ring)
type ringClient struct {
	logger logging.Logger

	// applicationQuerier is the querier for the application module, and is
	// used to get the gateways an application is delegated to.
	applicationQuerier client.ApplicationQueryClient

	// accountQuerier is used to fetch accounts for a given an account address.
	accountQuerier client.AccountQueryClient

	// sharedQuerier is used to fetch the shared module's parameters.
	sharedQuerier client.SharedQueryClient

	// ringPointsCache caches ring points by (appAddress, sessionEndHeight).
	// This ensures expensive DecodeToPoint operations are done once per session,
	// while allowing new ring points when sessions change (delegations may differ).
	// Bounded by ringPointsCacheKeepHeights (window eviction on store) and
	// ringPointsCacheTTL (max-age floor on read) so it never grows or freezes
	// for the process lifetime.
	ringPointsCache *xsync.Map[ringPointsCacheKey, ringPointsCacheEntry]

	// gatewaysLeftOut is the gateways already reported as left out of a ring:
	// the ring is rebuilt every session, and a gateway without a usable key
	// stays without one, so it is a Warn once per process and Debug after.
	gatewaysLeftOut *xsync.Map[string, struct{}]
}

// NewRingClient creates a new RingClient with the provided query clients.
// This is the HA RelayMiner's version that doesn't require depinject.
func NewRingClient(
	logger logging.Logger,
	applicationQuerier client.ApplicationQueryClient,
	accountQuerier client.AccountQueryClient,
	sharedQuerier client.SharedQueryClient,
) crypto.RingClient {
	return &ringClient{
		logger:             logger,
		ringPointsCache:    xsync.NewMap[ringPointsCacheKey, ringPointsCacheEntry](),
		gatewaysLeftOut:    xsync.NewMap[string, struct{}](),
		applicationQuerier: applicationQuerier,
		accountQuerier:     accountQuerier,
		sharedQuerier:      sharedQuerier,
	}
}

// gatewayLeftOutEvent is Warn the first time this process leaves gatewayAddress
// out of a ring, and Debug after.
func (rc *ringClient) gatewayLeftOutEvent(gatewayAddress string) *zerolog.Event {
	if rc.gatewaysLeftOut == nil {
		return rc.logger.Warn()
	}
	if _, seen := rc.gatewaysLeftOut.LoadOrStore(gatewayAddress, struct{}{}); seen {
		return rc.logger.Debug()
	}
	return rc.logger.Warn()
}

// GetRingForAddressAtHeight returns the ring for the address and block height provided.
// The height provided is used to determine the appropriate delegated gateways
// to use at that height since signature verification may be performed for
// delegations that are no longer active.
// The height provided will be rounded up to the session end height to ensure
// the ring is constructed from the correct past delegations since they become
// effective at the next session's start height.
// The ring is created by querying for the application's and its delegated
// gateways public keys. These keys are converted to secp256k1 curve points
// before forming the ring.
func (rc *ringClient) GetRingForAddressAtHeight(
	ctx context.Context,
	appAddress string,
	blockHeight int64,
) (*ring.Ring, error) {
	// TODO_MAINNET(#543): We don't really want to have to query the params for every method call.
	// Once `ModuleParamsClient` is implemented, use its replay observable's `#Last` method
	// to get the most recently (asynchronously) observed (and cached) value.
	sharedParams, err := rc.sharedQuerier.GetParams(ctx)
	if err != nil {
		return nil, err
	}
	sessionEndHeight := sharedtypes.GetSessionEndHeight(sharedParams, blockHeight)

	ringPubKeys, err := rc.getRingPubKeysForAddress(ctx, appAddress, uint64(sessionEndHeight))
	if err != nil {
		return nil, err
	}

	return GetRingFromPubKeys(ringPubKeys)
}

// MaxRingSignatureMembers is the most members a relay request ring signature
// may have. It is a fixed bound, checked before any ring lookup; the expected
// ring's member count bounds the signature again once the ring is known. It is
// the chain's own bound: a relay signed by a larger ring fails the chain's
// proof validation, so serving it would be unpaid work.
const MaxRingSignatureMembers = protocolrings.MaxRingSignatureMembers

// validateRingSignatureSize checks a serialized ring signature against the
// ring-go v0.2.0 layout: a 4-byte big-endian member count n, the challenge
// scalar, the key image point, then n (scalar, point) pairs. n must be between
// 2 and maxMembers.
func validateRingSignatureSize(signature []byte, maxMembers uint64) error {
	const (
		sizeLen   = 4
		scalarLen = 32 // ring-go's fixed scalar length for secp256k1
	)
	if len(signature) < sizeLen {
		return ErrRingClientInvalidRelayRequestSignature.Wrapf(
			"ring signature is %d bytes, too short", len(signature),
		)
	}

	numRingMembers := uint64(binary.BigEndian.Uint32(signature[:sizeLen]))
	if numRingMembers < 2 || numRingMembers > maxMembers {
		return ErrRingClientInvalidRelayRequestSignature.Wrapf(
			"ring signature has %d members, expected between 2 and %d", numRingMembers, maxMembers,
		)
	}

	pointLen := uint64(ringCurve.CompressedPointSize())
	expectedLen := sizeLen + scalarLen + pointLen + numRingMembers*(scalarLen+pointLen)
	if uint64(len(signature)) != expectedLen {
		return ErrRingClientInvalidRelayRequestSignature.Wrapf(
			"ring signature is %d bytes, expected %d for %d members",
			len(signature), expectedLen, numRingMembers,
		)
	}
	return nil
}

// VerifyRelayRequestSignature verifies the signature of the relay request
// provided against the corresponding ring for the application address in
// the same request.
func (rc *ringClient) VerifyRelayRequestSignature(
	ctx context.Context,
	relayRequest *types.RelayRequest,
) error {
	relayRequestMeta := relayRequest.GetMeta()

	sessionHeader := relayRequestMeta.GetSessionHeader()
	// ValidateBasic dereferences the header in poktroll v0.1.35.
	if sessionHeader == nil {
		return ErrRingClientInvalidRelayRequest.Wrap("invalid session header: missing")
	}
	if err := sessionHeader.ValidateBasic(); err != nil {
		return ErrRingClientInvalidRelayRequest.Wrapf("invalid session header: %v", err)
	}

	rc.logger.Debug().
		Fields(map[string]any{
			"session_id":          sessionHeader.GetSessionId(),
			"application_address": sessionHeader.GetApplicationAddress(),
			"service_id":          sessionHeader.GetServiceId(),
		}).
		Msg("verifying relay request signature")

	// Extract the relay request's ring signature.
	signature := relayRequestMeta.GetSignature()
	if signature == nil {
		return ErrRingClientInvalidRelayRequest.Wrap("missing signature from relay request")
	}

	// Check the signature's size before deserializing it: the member count must
	// be within bounds, and the length must match the member count exactly.
	if err := validateRingSignatureSize(signature, MaxRingSignatureMembers); err != nil {
		return err
	}

	// Get the ring for the application address of the relay request, at the
	// end height of the request's session.
	sessionEndHeight := sessionHeader.GetSessionEndBlockHeight()
	appAddress := sessionHeader.GetApplicationAddress()
	expectedRelayRingPointsForApp, numExpectedRingMembers, err := rc.getRingPointsForAddressAtSessionEndHeight(
		ctx,
		appAddress,
		sessionEndHeight,
	)
	if err != nil {
		return ErrRingClientInvalidRelayRequest.Wrapf(
			"error getting ring for application address %s: %v", appAddress, err,
		)
	}

	// The signature cannot have more members than the expected ring. Signers
	// build the same list of ring members, which can name a key more than once
	// (e.g. the application key twice when it has no gateways), so the bound is
	// the member count, not the number of distinct keys.
	if err := validateRingSignatureSize(signature, uint64(numExpectedRingMembers)); err != nil {
		return err
	}
	return verifyRingSignature(signature, expectedRelayRingPointsForApp, relayRequest, appAddress)
}

// verifyRingSignature deserializes the signature and verifies it against the
// relay request, using only keys of the expected ring. A panic while handling
// the signature bytes is returned as an invalid signature.
func verifyRingSignature(
	signature []byte,
	expectedRelayRingPointsForApp map[string]ringtypes.Point,
	relayRequest *types.RelayRequest,
	appAddress string,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ErrRingClientInvalidRelayRequestSignature.Wrapf("panic verifying ring signature: %v", r)
		}
	}()

	// Deserialize the request signature bytes back into a ring signature.
	relayRequestRingSig := new(ring.RingSig)
	if err = relayRequestRingSig.Deserialize(ringCurve, signature); err != nil {
		return ErrRingClientInvalidRelayRequestSignature.Wrapf(
			"error deserializing ring signature: %s", err,
		)
	}

	// Check that the expected ring signature points map contains the public keys
	// in the relay request's ring signature.
	if !ringPointsContain(expectedRelayRingPointsForApp, relayRequestRingSig) {
		return ErrRingClientInvalidRelayRequestSignature.Wrapf(
			"ring signature in the relay request does not match the expected one for the app %s", appAddress,
		)
	}

	// Get and hash the signable bytes of the relay request.
	requestSignableBz, err := relayRequest.GetSignableBytesHash()
	if err != nil {
		return ErrRingClientInvalidRelayRequest.Wrapf("error getting relay request signable bytes: %v", err)
	}

	// Verify the relay request's signature.
	if valid := relayRequestRingSig.Verify(requestSignableBz); !valid {
		return ErrRingClientInvalidRelayRequestSignature.Wrapf("invalid relay request signature or bytes")
	}

	return nil
}

// getRingPubKeysForAddress returns the public keys corresponding to a ring.
// It returns a slice consisting of the application's public key and the public
// keys of the gateways to which the application delegated the authority to sign
// relay requests on its behalf in the session ending at the given height.
//
// The application's key is required. A gateway whose account has no public
// key, or whose key is not a secp256k1 key, is left out of the ring: nobody can
// sign with it. Any other error getting a gateway's key is returned rather than
// building a smaller ring, which would refuse that gateway's relays for as long
// as the ring stays cached.
func (rc *ringClient) getRingPubKeysForAddress(
	ctx context.Context,
	appAddress string,
	sessionEndHeight uint64,
) ([]cryptotypes.PubKey, error) {
	// Get the application's on chain state.
	app, err := rc.applicationQuerier.GetApplication(ctx, appAddress)
	if err != nil {
		return nil, err
	}

	appPubKey, err := rc.accountQuerier.GetPubKeyFromAddress(ctx, appAddress)
	if err != nil {
		return nil, err
	}

	// Create a slice of addresses for the ring, and the public key of each.
	ringAddresses := []string{appAddress}
	pubKeysByAddress := map[string]cryptotypes.PubKey{appAddress: appPubKey}

	// Reconstruct the delegatee gateway addresses at the given session end
	// height and add those that can be ring members.
	for _, gatewayAddress := range GetRingAddressesAtSessionEndHeight(&app, sessionEndHeight) {
		gatewayPubKey, err := rc.accountQuerier.GetPubKeyFromAddress(ctx, gatewayAddress)
		if query.IsEntityNotFound(err) || errors.Is(err, query.ErrAccountPubKeyNotFound) {
			rc.gatewayLeftOutEvent(gatewayAddress).Err(err).
				Str("gateway_address", gatewayAddress).
				Msg("leaving gateway out of the ring: it has no public key")
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, ok := gatewayPubKey.(*secp256k1.PubKey); !ok {
			rc.gatewayLeftOutEvent(gatewayAddress).
				Str("gateway_address", gatewayAddress).
				Str("key_type", fmt.Sprintf("%T", gatewayPubKey)).
				Msg("leaving gateway out of the ring: its key is not a secp256k1 key")
			continue
		}
		ringAddresses = append(ringAddresses, gatewayAddress)
		pubKeysByAddress[gatewayAddress] = gatewayPubKey
	}

	// Ring signatures require at least two pubKeys.
	// If the Application has no gateway in the ring, add its own address again.
	if len(ringAddresses) == 1 {
		ringAddresses = append(ringAddresses, appAddress)
	}

	// Sort the ring addresses so the ring is built the same way every time.
	// Verification checks that every key in the signature's ring is in this set
	// (see ringPointsContain); it does not require the rings to be equal.
	slices.Sort(ringAddresses)

	rc.logger.Debug().
		Int("num_addresses", len(ringAddresses)).
		Msg("converting addresses to points")

	pubKeys := make([]cryptotypes.PubKey, len(ringAddresses))
	for i, addr := range ringAddresses {
		pubKeys[i] = pubKeysByAddress[addr]
	}
	return pubKeys, nil
}

// getRingPointsForAddressAtSessionEndHeight returns a map of the ring points
// for the given application in the session ending at the given height, and the
// number of ring members, counting a key listed more than once each time. It
// takes into account the application itself as well as all the addresses it
// delegated to. It returns a map of encoded ring points to Point objects (i.e.
// public keys).
//
// Ring points are cached by (appAddress, sessionEndHeight) since:
// - Converting public keys to curve points (DecodeToPoint) is cryptographically expensive
// - Ring composition only changes at session boundaries (delegation changes)
// - At 1000 RPS, this prevents 2000-4000 DecodeToPoint calls/sec
func (rc *ringClient) getRingPointsForAddressAtSessionEndHeight(
	ctx context.Context,
	appAddress string,
	sessionEndHeight int64,
) (map[string]ringtypes.Point, int, error) {
	cacheKey := ringPointsCacheKey{
		appAddress:       appAddress,
		sessionEndHeight: sessionEndHeight,
	}

	// Check cache first. Treat an entry past its TTL floor as a miss so a stale
	// one-time compute self-heals instead of being frozen for the process lifetime.
	if entry, ok := rc.ringPointsCache.Load(cacheKey); ok {
		if time.Since(entry.cachedAt) < ringPointsCacheTTL {
			return entry.points, entry.numMembers, nil
		}
	}

	// Cache miss - compute ring points
	ringPubKeys, err := rc.getRingPubKeysForAddress(ctx, appAddress, uint64(sessionEndHeight))
	if err != nil {
		return nil, 0, err
	}

	// Get the points on the secp256k1 curve for the public keys in the ring.
	points, err := pointsFromPublicKeys(ringPubKeys...)
	if err != nil {
		return nil, 0, err
	}

	ringPoints := make(map[string]ringtypes.Point, len(points))
	for _, point := range points {
		// Use the point's encoded bytes as the key in the map to identify it and
		// avoid nested loops when checking for its existence.
		// Since it's not possible to use bytes slices as keys in a map, we convert
		// the point to a string before using it as a key.
		keyFromPoint := string(point.Encode())
		ringPoints[keyFromPoint] = point
	}

	// Cache the ring points and prune entries far below the newest height to
	// keep the sync.Map bounded. Overwrite (Store, not LoadOrStore) so a TTL
	// refresh replaces the stale entry; concurrent first-misses recompute the
	// same deterministic value, so last-write-wins is safe.
	rc.storeRingPoints(cacheKey, ringPoints, len(points))
	return ringPoints, len(points), nil
}

// storeRingPoints caches a ring-points entry and prunes entries whose session end
// height is far below the newest, bounding the cache the same way the session
// cache bounds its L1 (cache/session_cache.go storeSession).
func (rc *ringClient) storeRingPoints(key ringPointsCacheKey, points map[string]ringtypes.Point, numMembers int) {
	rc.ringPointsCache.Store(key, ringPointsCacheEntry{
		points:           points,
		numMembers:       numMembers,
		sessionEndHeight: key.sessionEndHeight,
		cachedAt:         time.Now(),
	})

	cutoff := key.sessionEndHeight - ringPointsCacheKeepHeights
	if cutoff <= 0 {
		return
	}
	rc.ringPointsCache.Range(func(k ringPointsCacheKey, e ringPointsCacheEntry) bool {
		if e.sessionEndHeight < cutoff {
			rc.ringPointsCache.Delete(k)
		}
		return true
	})
}

// GetRingAddressesAtBlock returns the active gateway addresses that need to be
// used to construct the ring in order to validate that the given app should pay for.
// It takes into account both active delegations and pending undelegations that
// should still be part of the ring at the given block height.
// The ring addresses slice is reconstructed by adding back the past delegated
// gateways that have been undelegated after the target session end height.
func GetRingAddressesAtBlock(
	sharedParams *sharedtypes.Params,
	app *apptypes.Application,
	blockHeight int64,
) []string {
	// Get the target session end height at which we want to get the active delegations.
	targetSessionEndHeight := uint64(sharedtypes.GetSessionEndHeight(sharedParams, blockHeight))

	return GetRingAddressesAtSessionEndHeight(app, targetSessionEndHeight)
}

// GetRingAddressesAtSessionEndHeight returns the active gateway addresses that need to be
// used to construct the ring in order to validate that the given app should pay for relays.
// It takes into account both active delegations and pending undelegations that
// should still be part of the ring at the given session end height.
// The ring addresses slice is reconstructed by adding back the past delegated
// gateways that have been undelegated after the target session end height.
func GetRingAddressesAtSessionEndHeight(
	app *apptypes.Application,
	targetSessionEndHeight uint64,
) []string {
	// Get the current active delegations for the application and use them as a
	// base. Clone them: appending below must not write into the application's
	// slice, which callers may share.
	activeDelegationsAtHeight := slices.Clone(app.DelegateeGatewayAddresses)

	// Use a map to keep track of the gateways addresses that have been added to
	// the active delegations slice to avoid duplicates.
	addedDelegations := make(map[string]struct{})

	// Iterate over the pending undelegations recorded at their respective block
	// height and check whether to add them back as active delegations.
	for pendingUndelegationHeight, undelegatedGateways := range app.PendingUndelegations {
		// If the pending undelegation happened BEFORE the target session end height, skip it.
		// The gateway is pending undelegation and simply has not been pruned yet.
		// It will be pruned in the near future.
		if pendingUndelegationHeight < targetSessionEndHeight {
			continue
		}
		// Add back any gateway address  that was undelegated after the target session
		// end height, as we consider it not happening yet relative to the target height.
		for _, gatewayAddress := range undelegatedGateways.GatewayAddresses {
			if _, ok := addedDelegations[gatewayAddress]; ok {
				continue
			}

			activeDelegationsAtHeight = append(activeDelegationsAtHeight, gatewayAddress)
			// Mark the gateway address as added to avoid duplicates.
			addedDelegations[gatewayAddress] = struct{}{}
		}

	}

	return activeDelegationsAtHeight
}

// ringPointsContain checks if the given ring points map contains the public keys
// in the given ring signature.
func ringPointsContain(
	ringPoints map[string]ringtypes.Point,
	ringSig *ring.RingSig,
) bool {
	for _, publicKey := range ringSig.PublicKeys() {
		// Use the keyFromPoint's encoded bytes as the key in the map to identify it and
		// avoid nested loops when checking for its existence.
		// Since it's not possible to use bytes slices as keys in a map, we convert
		// the point to a string before using it as a key.
		if _, ok := ringPoints[string(publicKey.Encode())]; !ok {
			return false
		}
	}

	return true
}
