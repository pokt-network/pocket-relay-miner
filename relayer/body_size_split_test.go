//go:build test

package relayer

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheFirstStageBoundCoversEveryServiceBound is the defect.
//
// An HTTP relay body is bounded twice: once before the service is known
// (proxy.go, the io.LimitReader that lets the body be read at all) and once
// after (the service's own limit). The first stage USED the global default, so a
// service configured to allow MORE than the default never saw its own limit: its
// relays were rejected in the first stage, labelled unknown/unknown because the
// service ID does not exist yet, which is also why nobody could tell from the
// metrics which service was losing traffic.
//
// Stated as the invariant it is: the first stage must be at least as permissive
// as every service, or that service rejects its own traffic by its own config.
//
// LINK: first-stage-covers-every-service
func TestTheFirstStageBoundCoversEveryServiceBound(t *testing.T) {
	c := &Config{
		DefaultMaxBodySizeBytes: 1 << 20, // 1 MiB
		Services: map[string]ServiceConfig{
			"small":      {},
			"big":        {MaxRequestBodySizeBytes: 4 << 20}, // 4 MiB
			"legacy-big": {MaxBodySizeBytes: 8 << 20},        // 8 MiB, the pre-split key
		},
	}

	firstStage := c.MaxRequestBodySizeAcrossServices()

	for serviceID := range c.Services {
		own := c.GetServiceMaxRequestBodySize(serviceID)
		require.GreaterOrEqual(t, firstStage, own,
			"LINK first-stage-covers-every-service: service %q allows %d B but the pre-parse read stops at %d B, so its relays are rejected as unknown/unknown before its own limit is ever consulted",
			serviceID, own, firstStage)
	}

	require.Equal(t, int64(8<<20), firstStage,
		"LINK first-stage-covers-every-service: the first stage is the largest bound any service allows, not the default")
}

// TestRequestBoundResolvesMostSpecificFirst pins the whole fallback chain, which
// is the part an operator cannot see: the new key falls back through the
// pre-split per-service key, then the new default, then the pre-split default.
// Each step exists so a config written before the split keeps its behaviour.
//
// LINK: request-bound-resolution-order
func TestRequestBoundResolvesMostSpecificFirst(t *testing.T) {
	c := &Config{
		DefaultMaxBodySizeBytes:        1 << 20,
		DefaultMaxRequestBodySizeBytes: 2 << 20,
		Services: map[string]ServiceConfig{
			"both":        {MaxRequestBodySizeBytes: 9 << 20, MaxBodySizeBytes: 5 << 20},
			"legacy-only": {MaxBodySizeBytes: 5 << 20},
			"neither":     {},
		},
	}

	for _, tc := range []struct {
		serviceID string
		want      int64
		source    BodySizeSource
	}{
		{"both", 9 << 20, BodySizeFromServiceRequestOverride},
		{"legacy-only", 5 << 20, BodySizeFromServiceLegacy},
		{"neither", 2 << 20, BodySizeFromDefaultRequest},
		{"absent", 2 << 20, BodySizeFromDefaultRequest},
	} {
		size, source := c.ResolveMaxRequestBodySize(tc.serviceID)
		require.Equal(t, tc.want, size,
			"LINK request-bound-resolution-order: %q resolves to the most specific key that is set", tc.serviceID)
		require.Equal(t, tc.source, source,
			"LINK request-bound-resolution-order: %q must NAME the key it used -- an operator who writes a key and cannot confirm it applied is in the same position as one whose key was ignored", tc.serviceID)
	}

	// With no new default at all, the pre-split key is what answers.
	c.DefaultMaxRequestBodySizeBytes = 0
	size, source := c.ResolveMaxRequestBodySize("neither")
	require.Equal(t, int64(1<<20), size)
	require.Equal(t, BodySizeFromLegacyDefault, source,
		"LINK request-bound-resolution-order: a config that names only the pre-split key resolves through it, and says so")
}

// TestResponseBoundIsPerServiceToo mirrors the request side.
//
// The shared BufferPool was read for a while as proof that the response bound
// had to be fleet-wide -- one pool, one number. It is not: the pool recycles
// BUFFERS, which start at DefaultInitialBufferSize and grow as they read
// (buffer_pool.go), while the bound is a limit passed per read. One pool, many
// limits, and no memory cost for the override.
//
// LINK: response-bound-is-per-service
func TestResponseBoundIsPerServiceToo(t *testing.T) {
	c := &Config{
		DefaultMaxBodySizeBytes:         1 << 20,
		DefaultMaxResponseBodySizeBytes: 2 << 20,
		Services: map[string]ServiceConfig{
			"override": {MaxResponseBodySizeBytes: 9 << 20, MaxBodySizeBytes: 5 << 20},
			"legacy":   {MaxBodySizeBytes: 5 << 20},
			"default":  {},
		},
	}

	for _, tc := range []struct {
		serviceID string
		want      int64
		source    BodySizeSource
	}{
		{"override", 9 << 20, BodySizeFromServiceResponseOverride},
		{"legacy", 5 << 20, BodySizeFromServiceLegacy},
		{"default", 2 << 20, BodySizeFromDefaultResponse},
	} {
		size, source := c.ResolveMaxResponseBodySize(tc.serviceID)
		require.Equal(t, tc.want, size,
			"LINK response-bound-is-per-service: %q resolves to its own bound", tc.serviceID)
		require.Equal(t, tc.source, source,
			"LINK response-bound-is-per-service: %q must name the key it used", tc.serviceID)
	}

	require.Equal(t, int64(9<<20), c.MaxResponseBodySizeAcrossServices(),
		"LINK response-bound-is-per-service: the pool's own fallback bound covers the widest service, so a read that names no service is never cut short")
}

// TestABodyOfExactlyTheLimitIsAccepted pins an off-by-one that the split made
// worth fixing: the read stopped at exactly `limit` bytes and called that an
// overflow, so a response of precisely the configured size was refused. An
// operator who writes 10 MiB means 10 MiB is allowed, and the request side has
// always read it that way.
//
// LINK: exactly-the-limit-is-allowed
func TestABodyOfExactlyTheLimitIsAccepted(t *testing.T) {
	pool := NewBufferPool(1 << 20)

	got, err := pool.ReadWithBufferLimit(bytes.NewReader(make([]byte, 100)), 100)
	require.NoError(t, err,
		"LINK exactly-the-limit-is-allowed: a body of exactly the limit is within it")
	require.Len(t, got, 100)

	_, err = pool.ReadWithBufferLimit(bytes.NewReader(make([]byte, 101)), 100)
	require.Error(t, err,
		"LINK exactly-the-limit-is-allowed: one byte over is still over")
}

// TestTheLimitIsPerReadNotPerPool is the finding that unblocked the per-service
// response override: the pool and the bound are independent.
//
// LINK: limit-is-per-read
func TestTheLimitIsPerReadNotPerPool(t *testing.T) {
	pool := NewBufferPool(10)

	got, err := pool.ReadWithBufferLimit(bytes.NewReader(make([]byte, 5000)), 8192)
	require.NoError(t, err,
		"LINK limit-is-per-read: a caller's larger limit governs its own read, so one pool can serve services with different bounds")
	require.Len(t, got, 5000)

	// A read that names no limit, which is what a caller with no service
	// resolved passes. The fallback lives in ReadWithBufferLimit itself
	// (`limit <= 0`), so this is the same guarantee read at its own door: the
	// wrapper that used to spell it out had no production caller and was
	// removed rather than kept alive by this line.
	_, err = pool.ReadWithBufferLimit(bytes.NewReader(make([]byte, 5000)), 0)
	require.Error(t, err,
		"LINK limit-is-per-read: a read that names no service still falls back to the pool's own bound")
}

// TestQueueFloorMultipliesTheRequestBound is the arithmetic the split fixes.
//
// A queued relay retains the request body twice and one response. While both
// terms came from one field the floor was right by coincidence; with the
// directions set apart, a floor that multiplies the response bound -- or adds
// the request one -- is off by the difference, and the queue either admits more
// than it can hold or refuses what it was configured to accept.
//
// LINK: queue-floor-multiplies-request
func TestQueueFloorMultipliesTheRequestBound(t *testing.T) {
	c := &Config{
		DefaultMaxBodySizeBytes:         1 << 20,
		DefaultMaxResponseBodySizeBytes: 16 << 20,
		Services: map[string]ServiceConfig{
			"svc": {MaxRequestBodySizeBytes: 2 << 20},
		},
	}

	c.Services["svc"] = ServiceConfig{MaxRequestBodySizeBytes: 2 << 20, MaxResponseBodySizeBytes: 16 << 20}
	require.Equal(t, int64(2*(2<<20)+(16<<20)), c.ValidationQueueFloorBytes("svc"),
		"LINK queue-floor-multiplies-request: the 2x belongs to the REQUEST bound (body plus the copy the RelayRequest carries); the response is added once and comes from the fleet-wide bound")
}

// TestTheShippedExampleKeepsItsBehaviour is the compatibility half. Every
// service in the config this repository ships must resolve, after the split, to
// exactly what the single knob gave before it -- otherwise the split is a
// silent config change for every operator who upgrades.
//
// LINK: split-changes-no-existing-config
func TestTheShippedExampleKeepsItsBehaviour(t *testing.T) {
	c, err := LoadConfig("../config.relayer.example.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, c.Services, "the example must define services, or this test proves nothing")

	for serviceID := range c.Services {
		require.Equal(t, c.GetServiceMaxBodySize(serviceID), c.GetServiceMaxRequestBodySize(serviceID),
			"LINK split-changes-no-existing-config: service %q must keep the request bound it had before the split", serviceID)
	}

	for serviceID := range c.Services {
		require.Equal(t, c.GetServiceMaxBodySize(serviceID), c.GetServiceMaxResponseBodySize(serviceID),
			"LINK split-changes-no-existing-config: service %q must keep the response bound it had before the split", serviceID)
	}
	// The pre-split bound, computed here rather than kept in production: the
	// function that computed it had this assertion as its only caller, and a
	// function alive only so a test can compare against it is dead code that
	// reads as current (deadcode gate, item 401).
	//
	// GetServiceMaxBodySize is the pre-split API and still resolves the way it
	// did, so this is the value the pool was built with before the split.
	legacyPoolBound := c.DefaultMaxBodySizeBytes
	for serviceID := range c.Services {
		if size := c.GetServiceMaxBodySize(serviceID); size > legacyPoolBound {
			legacyPoolBound = size
		}
	}
	require.Equal(t, legacyPoolBound, c.MaxResponseBodySizeAcrossServices(),
		"LINK split-changes-no-existing-config: the pool must keep the bound it was built with before the split")

	// Every service in the shipped example sets max_body_size_bytes to exactly
	// the default, so the loop above passes whether or not the per-service key is
	// read at all -- it cannot reach that branch. Measured: an injection removing
	// the per-service fallback left it green. So the file is also used the one
	// way that DOES reach it: one service moved off the default, on a config that
	// came from the real file rather than a literal.
	var moved string
	for serviceID := range c.Services {
		moved = serviceID
		break
	}
	svc := c.Services[moved]
	svc.MaxBodySizeBytes = c.DefaultMaxBodySizeBytes * 3
	c.Services[moved] = svc

	require.Equal(t, c.DefaultMaxBodySizeBytes*3, c.GetServiceMaxRequestBodySize(moved),
		"LINK split-changes-no-existing-config: an operator whose only per-service key is the pre-split max_body_size_bytes must still have it govern the request bound")
	require.Equal(t, c.DefaultMaxBodySizeBytes*3, c.MaxRequestBodySizeAcrossServices(),
		"LINK split-changes-no-existing-config: and the pre-parse read must grow with it, or that service rejects its own relays")
}

// TestAProxyBuiltAsAStructLiteralStillBoundsTheBody pins the trap the fix cost
// a full red suite to find.
//
// Thirty test fixtures in this package build ProxyServer as a struct literal, so
// anything the constructor alone resolves is zero on all of them. A zero body
// bound is the worst kind: it does not panic or refuse to start, it truncates
// every request to nothing and answers 413 to traffic that is fine. Production
// would show the same if the field were ever left unset.
//
// LINK: body-bound-survives-a-literal-proxy
func TestAProxyBuiltAsAStructLiteralStillBoundsTheBody(t *testing.T) {
	p := &ProxyServer{
		config: &Config{
			DefaultMaxBodySizeBytes: 1 << 20,
			Services:                map[string]ServiceConfig{"big": {MaxRequestBodySizeBytes: 6 << 20}},
		},
	}

	require.Equal(t, int64(6<<20), p.maxRequestBodySize(),
		"LINK body-bound-survives-a-literal-proxy: with the constructor bypassed the bound must still come from the config, not from the zero value")
}
