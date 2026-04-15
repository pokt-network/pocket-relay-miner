//go:build test

package relayer

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newConfigWithServices is a small helper that returns a base Config with
// TimeoutProfiles auto-populated so the tests can focus on pool logic
// without tripping over unrelated validation.
func newConfigWithServices(services map[string]ServiceConfig) *Config {
	c := &Config{
		HTTPTransport: HTTPTransportConfig{
			MaxConnsPerHost:        500,
			MaxIdleConnsPerHost:    100,
			IdleConnTimeoutSeconds: 90,
		},
		TimeoutProfiles: map[string]TimeoutProfile{
			"fast":      {Name: "fast", RequestTimeoutSeconds: 30},
			"streaming": {Name: "streaming", RequestTimeoutSeconds: 600},
		},
		Services: services,
	}
	return c
}

func TestValidatePoolProfiles_AutoPopulatesDefaults(t *testing.T) {
	c := newConfigWithServices(nil)
	require.NoError(t, c.ValidatePoolProfiles())

	// All three built-in profiles must exist after auto-populate.
	require.Contains(t, c.PoolProfiles, "low")
	require.Contains(t, c.PoolProfiles, "medium")
	require.Contains(t, c.PoolProfiles, "high")

	// Values should match the tuned tiers from the sweep-optimal
	// loadtest (2026-04-14): 10 / 100 / 250.
	assert.Equal(t, 10, c.PoolProfiles["low"].MaxConnsPerHost)
	assert.Equal(t, 100, c.PoolProfiles["medium"].MaxConnsPerHost)
	assert.Equal(t, 250, c.PoolProfiles["high"].MaxConnsPerHost)
}

func TestValidatePoolProfiles_RejectsUnknownProfile(t *testing.T) {
	c := newConfigWithServices(map[string]ServiceConfig{
		"op": {PoolProfile: "ultra-high"},
	})
	err := c.ValidatePoolProfiles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ultra-high")
	assert.Contains(t, err.Error(), "op")
}

func TestValidatePoolProfiles_RejectsNegativeValues(t *testing.T) {
	cases := []struct {
		name    string
		profile PoolProfile
	}{
		{"MaxConnsPerHost < 0", PoolProfile{MaxConnsPerHost: -1}},
		{"MaxIdleConnsPerHost < 0", PoolProfile{MaxIdleConnsPerHost: -1}},
		{"IdleConnTimeoutSeconds < 0", PoolProfile{IdleConnTimeoutSeconds: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newConfigWithServices(nil)
			c.PoolProfiles = map[string]PoolProfile{"broken": tc.profile}
			err := c.ValidatePoolProfiles()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "broken")
		})
	}
}

func TestResolvePoolProfile_UsesServiceProfile(t *testing.T) {
	c := newConfigWithServices(map[string]ServiceConfig{
		"op": {PoolProfile: "high"},
	})
	require.NoError(t, c.ValidatePoolProfiles())

	resolved := c.ResolvePoolProfile("op")
	require.NotNil(t, resolved)
	assert.Equal(t, 250, resolved.MaxConnsPerHost, "op should get the 'high' tier")
	assert.Equal(t, 50, resolved.MaxIdleConnsPerHost)
}

func TestResolvePoolProfile_FallsBackToGlobal(t *testing.T) {
	c := newConfigWithServices(map[string]ServiceConfig{
		"avax": {}, // no PoolProfile set
	})
	require.NoError(t, c.ValidatePoolProfiles())

	resolved := c.ResolvePoolProfile("avax")
	require.NotNil(t, resolved)
	// Without a profile name, the service inherits HTTPTransport defaults.
	assert.Equal(t, 500, resolved.MaxConnsPerHost)
	assert.Equal(t, 100, resolved.MaxIdleConnsPerHost)
}

func TestResolvePoolProfile_PartialOverrideMergesGlobal(t *testing.T) {
	// A profile that only sets MaxConnsPerHost should inherit the other
	// two knobs from HTTPTransport, not silently become zero.
	c := newConfigWithServices(map[string]ServiceConfig{
		"poly": {PoolProfile: "custom"},
	})
	c.PoolProfiles = map[string]PoolProfile{
		"custom": {MaxConnsPerHost: 42},
	}
	require.NoError(t, c.ValidatePoolProfiles())

	resolved := c.ResolvePoolProfile("poly")
	require.NotNil(t, resolved)
	assert.Equal(t, 42, resolved.MaxConnsPerHost, "override applied")
	assert.Equal(t, 100, resolved.MaxIdleConnsPerHost, "global idle per host preserved")
	assert.Equal(t, int64(90), resolved.IdleConnTimeoutSeconds, "global idle timeout preserved")
}

func TestResolvePoolProfile_UnknownServiceReturnsGlobal(t *testing.T) {
	c := newConfigWithServices(nil)
	require.NoError(t, c.ValidatePoolProfiles())

	// A service that isn't in the config at all still gets a non-nil,
	// sensible profile so callers don't segfault.
	resolved := c.ResolvePoolProfile("never-seen")
	require.NotNil(t, resolved)
	assert.Equal(t, 500, resolved.MaxConnsPerHost)
}

func TestBuildClientPool_OneClientPerService(t *testing.T) {
	c := newConfigWithServices(map[string]ServiceConfig{
		"op":   {TimeoutProfile: "fast", PoolProfile: "high"},
		"avax": {TimeoutProfile: "fast", PoolProfile: "low"},
		"sui":  {TimeoutProfile: "fast"}, // no pool profile, uses default
	})
	require.NoError(t, c.ValidatePoolProfiles())

	clients, fallback := buildClientPool(c, &c.HTTPTransport)
	require.NotNil(t, fallback, "fallback client must always be built")
	require.Len(t, clients, 3, "one client per service")

	// The three services must get DIFFERENT *http.Client instances so
	// their transports (and therefore their connection pools) are
	// isolated. Sharing pointers would defeat the whole point.
	assert.NotSame(t, clients["op"], clients["avax"],
		"op and avax must have distinct clients")
	assert.NotSame(t, clients["op"], clients["sui"],
		"op and sui must have distinct clients")
	assert.NotSame(t, clients["op"], fallback,
		"op and fallback must be distinct")
}

func TestBuildClientPool_AppliesPoolProfileToTransport(t *testing.T) {
	// End-to-end check: each service's transport must carry the pool
	// knobs resolved for its pool_profile, not the global default.
	c := newConfigWithServices(map[string]ServiceConfig{
		"op":   {TimeoutProfile: "fast", PoolProfile: "high"},
		"avax": {TimeoutProfile: "fast", PoolProfile: "low"},
		"sui":  {TimeoutProfile: "fast"}, // falls back to global
	})
	require.NoError(t, c.ValidatePoolProfiles())

	clients, _ := buildClientPool(c, &c.HTTPTransport)

	// Expected pool knob values per service after resolution. Checking all
	// three fields per service (not just MaxConnsPerHost) catches bugs
	// where the merge logic silently drops IdleConnsPerHost or timeout.
	type want struct {
		maxConnsPerHost     int
		maxIdleConnsPerHost int
		idleTimeoutSeconds  int
	}
	cases := map[string]want{
		"op":   {maxConnsPerHost: 250, maxIdleConnsPerHost: 50, idleTimeoutSeconds: 90},
		"avax": {maxConnsPerHost: 10, maxIdleConnsPerHost: 5, idleTimeoutSeconds: 90},
		"sui":  {maxConnsPerHost: 500, maxIdleConnsPerHost: 100, idleTimeoutSeconds: 90},
	}
	for svc, w := range cases {
		t.Run(svc, func(t *testing.T) {
			transport, ok := clients[svc].Transport.(*http.Transport)
			require.True(t, ok, "must use *http.Transport")
			assert.Equal(t, w.maxConnsPerHost, transport.MaxConnsPerHost, "MaxConnsPerHost")
			assert.Equal(t, w.maxIdleConnsPerHost, transport.MaxIdleConnsPerHost, "MaxIdleConnsPerHost")
			assert.Equal(t, w.idleTimeoutSeconds, int(transport.IdleConnTimeout.Seconds()), "IdleConnTimeout")
		})
	}
}

// TestBuildClientPool_EmptyServices covers the edge case where Services is
// nil. Startup must not panic; the fallback client must still be usable.
func TestBuildClientPool_EmptyServices(t *testing.T) {
	c := newConfigWithServices(nil)
	require.NoError(t, c.ValidatePoolProfiles())

	clients, fallback := buildClientPool(c, &c.HTTPTransport)
	assert.Empty(t, clients)
	require.NotNil(t, fallback, "fallback must exist even with zero services")

	transport, ok := fallback.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, 500, transport.MaxConnsPerHost,
		"fallback inherits HTTPTransport global defaults")
}

// TestBuildClientPool_TimeoutProfilePropagation verifies that the timeout
// profile's response_header_timeout ends up on the service's transport.
// Without this check, a bug that dropped the timeout would be invisible —
// the request would just block longer than intended.
func TestBuildClientPool_TimeoutProfilePropagation(t *testing.T) {
	c := newConfigWithServices(map[string]ServiceConfig{
		"fast-svc":   {TimeoutProfile: "fast"},
		"stream-svc": {TimeoutProfile: "streaming"},
	})
	// Differentiate the two profiles with response_header_timeout values.
	c.TimeoutProfiles["fast"] = TimeoutProfile{
		Name: "fast", RequestTimeoutSeconds: 6,
		ResponseHeaderTimeoutSeconds: 4,
		DialTimeoutSeconds:           2,
	}
	c.TimeoutProfiles["streaming"] = TimeoutProfile{
		Name: "streaming", RequestTimeoutSeconds: 600,
		ResponseHeaderTimeoutSeconds: 0, // streaming = no header timeout
		DialTimeoutSeconds:           10,
	}
	require.NoError(t, c.ValidatePoolProfiles())

	clients, _ := buildClientPool(c, &c.HTTPTransport)

	fastTransport := clients["fast-svc"].Transport.(*http.Transport)
	streamTransport := clients["stream-svc"].Transport.(*http.Transport)

	assert.Equal(t, 4*time.Second, fastTransport.ResponseHeaderTimeout,
		"fast-svc inherits fast profile's 4s header timeout")
	assert.Equal(t, time.Duration(0), streamTransport.ResponseHeaderTimeout,
		"stream-svc has no header timeout (streaming profile)")
	// DialTimeoutSeconds propagates through buildHTTPClient into the
	// dialer, not into a Transport field — assert on the resolved knobs
	// we CAN read. The streaming profile's 10s dial must not leak into
	// the fast profile's client.
	assert.NotSame(t, fastTransport, streamTransport,
		"fast and streaming profiles must yield distinct transports")
}

// TestClassifyBackendOutcome exhaustively covers every branch of the
// classification helper. A bug here would corrupt every metric that uses
// the outcome label (backendLatency, backendRequests, relaysRejected), so
// every single path matters.
func TestClassifyBackendOutcome(t *testing.T) {
	// Helper to build an error that contains one of the reject reason
	// substrings, matching the production wrapping pattern at proxy.go:
	// `fmt.Errorf("%s: %w", rejectReasonX, cause)`.
	errWith := func(reason string) error {
		return fmt.Errorf("%s: underlying cause", reason)
	}

	cases := []struct {
		name       string
		err        error
		respStatus int
		wantOut    string
	}{
		// Success paths — error is nil.
		{"2xx success", nil, 200, "success"},
		{"3xx redirect is success", nil, 301, "success"},
		{"4xx client error is success", nil, 404, "success"},
		{"499 edge is success", nil, 499, "success"},
		{"500 is backend_5xx", nil, 500, rejectReasonBackend5xx},
		{"503 is backend_5xx", nil, 503, rejectReasonBackend5xx},
		{"599 is backend_5xx", nil, 599, rejectReasonBackend5xx},
		// Error paths — precedence matters: client_disconnected wins over
		// backend_timeout which wins over generic network error.
		{"client_disconnected error", errWith(rejectReasonClientDisconnected), 0, rejectReasonClientDisconnected},
		{"backend_timeout error", errWith(rejectReasonBackendTimeout), 0, rejectReasonBackendTimeout},
		{"generic network error", fmt.Errorf("connection refused"), 0, rejectReasonBackendNetworkError},
		{"DNS error falls into network_error", fmt.Errorf("no such host"), 0, rejectReasonBackendNetworkError},
		{"TLS handshake error falls into network_error", fmt.Errorf("x509: certificate"), 0, rejectReasonBackendNetworkError},
		// Edge case: err is set AND respStatus is 500. Error classification
		// wins because we never actually read the body — there's no
		// guarantee the status is meaningful.
		{"error takes precedence over 500", errWith(rejectReasonBackendTimeout), 500, rejectReasonBackendTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyBackendOutcome(tc.err, tc.respStatus)
			assert.Equal(t, tc.wantOut, got)
		})
	}
}

// TestStatusCodeLabel covers the Prometheus label rendering. status_code is
// a bounded-cardinality label: it must never contain dynamic user data.
func TestStatusCodeLabel(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		respStatus int
		want       string
	}{
		{"nil err + 200", nil, 200, "200"},
		{"nil err + 404", nil, 404, "404"},
		{"nil err + 500", nil, 500, "500"},
		{"nil err + 0 status is none", nil, 0, "none"},
		{"non-nil err forces none", fmt.Errorf("boom"), 200, "none"},
		{"non-nil err + 0 status is none", fmt.Errorf("boom"), 0, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, statusCodeLabel(tc.respStatus, tc.err))
		})
	}
}

// TestGetClientForService covers the lookup paths: registered service,
// unknown service falls back, and the defensive-nil branch (which should
// never happen in production but must not segfault).
func TestGetClientForService(t *testing.T) {
	c := newConfigWithServices(map[string]ServiceConfig{
		"op": {TimeoutProfile: "fast", PoolProfile: "high"},
	})
	require.NoError(t, c.ValidatePoolProfiles())

	clients, fallback := buildClientPool(c, &c.HTTPTransport)
	p := &ProxyServer{
		logger:             testLogger(),
		config:             c,
		clientPool:         clients,
		clientPoolFallback: fallback,
	}

	t.Run("registered service returns its client", func(t *testing.T) {
		got := p.getClientForService("op")
		require.NotNil(t, got)
		assert.Same(t, clients["op"], got)
	})

	t.Run("unknown service returns fallback", func(t *testing.T) {
		got := p.getClientForService("never-registered")
		require.NotNil(t, got)
		assert.Same(t, fallback, got)
	})

	t.Run("nil fallback with unknown service returns nil not panic", func(t *testing.T) {
		p2 := &ProxyServer{
			logger:     testLogger(),
			config:     c,
			clientPool: clients,
			// intentionally leave clientPoolFallback nil
		}
		got := p2.getClientForService("never-registered")
		assert.Nil(t, got, "explicit nil signals error state, must not panic")
	})
}

// TestDefaultPoolProfiles_Values locks in the tier values so an accidental
// edit shifts them loudly. These are the three numbers the operator sees
// in production by default; any change must be deliberate.
func TestDefaultPoolProfiles_Values(t *testing.T) {
	p := defaultPoolProfiles()
	require.Len(t, p, 3)

	low := p["low"]
	assert.Equal(t, "low", low.Name)
	assert.Equal(t, 10, low.MaxConnsPerHost)
	assert.Equal(t, 5, low.MaxIdleConnsPerHost)
	assert.Equal(t, int64(90), low.IdleConnTimeoutSeconds)

	med := p["medium"]
	assert.Equal(t, 100, med.MaxConnsPerHost)
	assert.Equal(t, 20, med.MaxIdleConnsPerHost)

	high := p["high"]
	assert.Equal(t, 250, high.MaxConnsPerHost)
	assert.Equal(t, 50, high.MaxIdleConnsPerHost)
}

// TestConfigValidate_IncludesPoolProfiles ensures Validate() wires pool
// profile checks into the Config.Validate() chain. A missing hook would
// let an invalid pool_profile reference into production.
func TestConfigValidate_IncludesPoolProfiles(t *testing.T) {
	// Minimal valid config that references a bogus pool profile on one
	// service. Validate() must reject it.
	c := &Config{
		ListenAddr:            "0.0.0.0:8080",
		Redis:                 RedisConfig{URL: "redis://localhost:6379"},
		PocketNode:            PocketNodeConfig{QueryNodeRPCUrl: "http://x", QueryNodeGRPCUrl: "x:9090"},
		DefaultValidationMode: ValidationModeOptimistic,
		Services: map[string]ServiceConfig{
			"op": {
				PoolProfile:    "does-not-exist",
				DefaultBackend: "jsonrpc",
				Backends: map[string]BackendConfig{
					"jsonrpc": {URL: "http://backend:8545"},
				},
			},
		},
	}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}
