package relayer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/pool"
)

// HealthStatus represents the health status of a backend.
type HealthStatus int32

const (
	// HealthStatusUnknown means health has not been checked yet.
	HealthStatusUnknown HealthStatus = iota
	// HealthStatusHealthy means the backend is responding correctly.
	HealthStatusHealthy
	// HealthStatusUnhealthy means the backend is not responding correctly.
	HealthStatusUnhealthy
)

func (s HealthStatus) String() string {
	switch s {
	case HealthStatusUnknown:
		return "unknown"
	case HealthStatusHealthy:
		return "healthy"
	case HealthStatusUnhealthy:
		return "unhealthy"
	default:
		return "invalid"
	}
}

// BackendHealth tracks the health of a single backend endpoint.
// When a pool.BackendEndpoint is associated, health state is delegated to it
// (single source of truth shared with the circuit breaker). Supplementary fields
// like lastCheck and lastError remain on BackendHealth for active health check
// diagnostics.
type BackendHealth struct {
	// ServiceID is the pool key this backend belongs to (e.g., "serviceID:rpcType").
	ServiceID string

	// BackendURL is the URL of the backend.
	BackendURL string

	// endpoint is the pool BackendEndpoint that owns the authoritative health state.
	// When non-nil, IsHealthy() delegates to endpoint.IsHealthy() and
	// recordFailure/recordSuccess operate on endpoint atomics.
	endpoint *pool.BackendEndpoint

	// headers are pool-level headers applied to probe requests.
	headers map[string]string

	// auth is pool-level authentication applied to probe requests.
	auth *AuthenticationConfig

	// basePath is the BackendConfig.BasePath value, used to prepend the
	// backend's path prefix to probe requests (e.g. /ext/bc/C/rpc).
	basePath string

	// Status is the current health status (used only when endpoint is nil, legacy path).
	status atomic.Int32

	// LastCheck is when the last health check was performed.
	lastCheck atomic.Int64

	// LastError is the last error encountered (if unhealthy).
	lastError atomic.Value // stores string

	// ConsecutiveFailures tracks failures (used only when endpoint is nil, legacy path).
	consecutiveFailures atomic.Int32

	// ConsecutiveSuccesses tracks successes for healthy threshold calculation.
	consecutiveSuccesses atomic.Int32
}

// GetStatus returns the current health status.
// When a pool endpoint is associated, derives status from endpoint.IsHealthy().
func (h *BackendHealth) GetStatus() HealthStatus {
	if h.endpoint != nil {
		if h.endpoint.IsHealthy() {
			return HealthStatusHealthy
		}
		return HealthStatusUnhealthy
	}
	return HealthStatus(h.status.Load())
}

// SetStatus sets the health status.
// When a pool endpoint is associated, delegates to endpoint methods.
func (h *BackendHealth) SetStatus(status HealthStatus) {
	if h.endpoint != nil {
		switch status {
		case HealthStatusHealthy:
			h.endpoint.SetHealthy()
		case HealthStatusUnhealthy:
			h.endpoint.SetUnhealthy()
		default:
			// HealthStatusUnknown: no-op for pool endpoint
		}
		return
	}
	h.status.Store(int32(status))
}

// GetLastCheck returns when the last health check was performed.
func (h *BackendHealth) GetLastCheck() time.Time {
	return time.Unix(0, h.lastCheck.Load())
}

// GetLastError returns the last error message (empty if healthy).
func (h *BackendHealth) GetLastError() string {
	if err := h.lastError.Load(); err != nil {
		return err.(string)
	}
	return ""
}

// IsHealthy returns true if the backend is healthy.
// Delegates to pool BackendEndpoint when available (single source of truth).
func (h *BackendHealth) IsHealthy() bool {
	if h.endpoint != nil {
		return h.endpoint.IsHealthy()
	}
	status := h.GetStatus()
	// Unknown is treated as healthy to avoid blocking on startup
	return status == HealthStatusHealthy || status == HealthStatusUnknown
}

// ConsecutiveFailureCount returns the current consecutive failure count.
// Delegates to pool BackendEndpoint when available.
func (h *BackendHealth) ConsecutiveFailureCount() int32 {
	if h.endpoint != nil {
		return h.endpoint.ConsecutiveFailures()
	}
	return h.consecutiveFailures.Load()
}

// HealthChecker manages health checks for all backends.
type HealthChecker struct {
	logger     logging.Logger
	httpClient *http.Client

	// backends maps poolKey -> []*BackendHealth (one entry per endpoint in the pool)
	backends   map[string][]*BackendHealth
	backendsMu sync.RWMutex

	// Configuration per pool
	configs   map[string]*BackendHealthCheckConfig
	configsMu sync.RWMutex

	// Default thresholds
	defaultUnhealthyThreshold int
	defaultHealthyThreshold   int

	// Lifecycle
	mu       sync.Mutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewHealthChecker creates a new health checker.
func NewHealthChecker(logger logging.Logger) *HealthChecker {
	return &HealthChecker{
		logger: logging.ForComponent(logger, logging.ComponentHealthChecker),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		backends:                  make(map[string][]*BackendHealth),
		configs:                   make(map[string]*BackendHealthCheckConfig),
		defaultUnhealthyThreshold: 3,
		defaultHealthyThreshold:   2,
	}
}

// RegisterPool registers all endpoints in a pool for health checking.
// Each endpoint gets its own BackendHealth entry linked to the pool's BackendEndpoint.
// The config, headers, auth and basePath are shared across all endpoints in the pool.
// basePath, when set, is prepended to the probe path via the same mergeBackendPath
// rules used for real traffic, so probes hit the correct backend endpoint.
func (hc *HealthChecker) RegisterPool(poolKey string, endpoints []*pool.BackendEndpoint, config *BackendHealthCheckConfig, headers map[string]string, auth *AuthenticationConfig, basePath string) {
	backends := make([]*BackendHealth, 0, len(endpoints))
	for _, ep := range endpoints {
		backends = append(backends, &BackendHealth{
			ServiceID:  poolKey,
			BackendURL: ep.RawURL,
			endpoint:   ep,
			headers:    headers,
			auth:       auth,
			basePath:   basePath,
		})
	}

	hc.backendsMu.Lock()
	hc.backends[poolKey] = backends
	hc.backendsMu.Unlock()

	if config != nil {
		hc.configsMu.Lock()
		hc.configs[poolKey] = config
		hc.configsMu.Unlock()
	}

	hc.logger.Info().
		Str(logging.FieldServiceID, poolKey).
		Int("endpoint_count", len(endpoints)).
		Bool("health_check_enabled", config != nil && config.Enabled).
		Msg("registered pool for health checking")
}

// GetHealth returns the health status for the first endpoint in a pool.
// For per-endpoint health, use GetAllHealth.
func (hc *HealthChecker) GetHealth(poolKey string) *BackendHealth {
	hc.backendsMu.RLock()
	defer hc.backendsMu.RUnlock()
	backends := hc.backends[poolKey]
	if len(backends) == 0 {
		return nil
	}
	return backends[0]
}

// IsHealthy returns true if any backend in the pool is healthy.
func (hc *HealthChecker) IsHealthy(poolKey string) bool {
	hc.backendsMu.RLock()
	defer hc.backendsMu.RUnlock()
	backends := hc.backends[poolKey]
	if len(backends) == 0 {
		// Unknown pool - assume healthy
		return true
	}
	for _, b := range backends {
		if b.IsHealthy() {
			return true
		}
	}
	return false
}

// GetAllHealth returns health status for all backends across all pools.
func (hc *HealthChecker) GetAllHealth() map[string]*BackendHealth {
	hc.backendsMu.RLock()
	defer hc.backendsMu.RUnlock()

	result := make(map[string]*BackendHealth)
	for poolKey, backends := range hc.backends {
		if len(backends) == 1 {
			result[poolKey] = backends[0]
		} else {
			for i, b := range backends {
				key := fmt.Sprintf("%s#%d", poolKey, i)
				result[key] = b
			}
		}
	}
	return result
}

// Start begins health checking for all registered backends.
func (hc *HealthChecker) Start(ctx context.Context) error {
	hc.mu.Lock()
	if hc.closed {
		hc.mu.Unlock()
		return fmt.Errorf("health checker is closed")
	}

	ctx, hc.cancelFn = context.WithCancel(ctx)
	hc.mu.Unlock()

	// Start health check loops for each configured pool
	hc.configsMu.RLock()
	for poolKey, config := range hc.configs {
		if config.Enabled {
			hc.wg.Add(1)
			go hc.healthCheckLoop(ctx, poolKey, config)
		}
	}
	hc.configsMu.RUnlock()

	hc.logger.Info().Msg("health checker started")
	return nil
}

// healthCheckLoop runs periodic health checks for all endpoints in a pool.
func (hc *HealthChecker) healthCheckLoop(ctx context.Context, poolKey string, config *BackendHealthCheckConfig) {
	defer hc.wg.Done()

	interval := time.Duration(config.IntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run initial check immediately
	hc.checkPool(ctx, poolKey, config)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.checkPool(ctx, poolKey, config)
		}
	}
}

// checkPool performs health checks for all endpoints in a pool.
func (hc *HealthChecker) checkPool(ctx context.Context, poolKey string, config *BackendHealthCheckConfig) {
	hc.backendsMu.RLock()
	backends := hc.backends[poolKey]
	hc.backendsMu.RUnlock()

	for _, backend := range backends {
		hc.checkEndpoint(ctx, backend, config)
	}
}

// checkEndpoint performs a single health check for a backend endpoint.
func (hc *HealthChecker) checkEndpoint(ctx context.Context, backend *BackendHealth, config *BackendHealthCheckConfig) {
	// Build health check URL with proper path joining. When a base_path is
	// set on the BackendConfig we feed it through the same mergeBackendPath
	// rules used for real traffic, so probes hit the correct endpoint even
	// when operators configured the URL without a path component.
	probePath := mergeBackendPath("", backend.basePath, config.Endpoint)
	healthURL, err := joinURLPath(backend.BackendURL, probePath)
	if err != nil {
		hc.recordFailure(backend, config, fmt.Sprintf("invalid backend URL: %v", err))
		return
	}

	// Create request with timeout
	timeout := time.Duration(config.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := buildProbeRequest(reqCtx, healthURL, config, backend)
	if err != nil {
		hc.recordFailure(backend, config, fmt.Sprintf("failed to create request: %v", err))
		return
	}

	// Perform health check
	resp, err := hc.httpClient.Do(req)
	if err != nil {
		hc.recordFailure(backend, config, fmt.Sprintf("request failed: %v", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Always read the full response body to prevent connection leaks
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		hc.recordFailure(backend, config, fmt.Sprintf("failed to read response body: %v", err))
		return
	}

	// Validate response
	if err := validateResponse(resp.StatusCode, body, config); err != nil {
		hc.recordFailure(backend, config, err.Error())
		return
	}

	hc.recordSuccess(backend, config)
}

// buildProbeRequest creates an HTTP request for a health check probe.
// Applies custom method, body, content-type, and pool-level auth/headers.
func buildProbeRequest(ctx context.Context, healthURL string, config *BackendHealthCheckConfig, backend *BackendHealth) (*http.Request, error) {
	method := http.MethodGet
	if config.Method != "" {
		method = strings.ToUpper(config.Method)
	}

	var bodyReader io.Reader
	if config.RequestBody != "" {
		bodyReader = strings.NewReader(config.RequestBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, healthURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// Set Content-Type
	if config.ContentType != "" {
		req.Header.Set("Content-Type", config.ContentType)
	} else if config.RequestBody != "" {
		trimmed := strings.TrimSpace(config.RequestBody)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	// Apply pool-level headers
	if backend.headers != nil {
		for k, v := range backend.headers {
			req.Header.Set(k, v)
		}
	}

	// Apply pool-level authentication
	if backend.auth != nil {
		if backend.auth.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+backend.auth.BearerToken)
		} else if backend.auth.PlainToken != "" {
			req.Header.Set("Authorization", backend.auth.PlainToken)
		} else if backend.auth.Username != "" && backend.auth.Password != "" {
			req.SetBasicAuth(backend.auth.Username, backend.auth.Password)
		}
	}

	return req, nil
}

// validateResponse checks the HTTP response against the configured expectations.
// Returns nil if the response is considered healthy, or an error describing the mismatch.
func validateResponse(statusCode int, body []byte, config *BackendHealthCheckConfig) error {
	// Check status code
	if len(config.ExpectedStatus) > 0 {
		matched := false
		for _, expected := range config.ExpectedStatus {
			if statusCode == expected {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("unexpected status code: %d (expected one of %v)", statusCode, config.ExpectedStatus)
		}
	} else {
		// Default: accept any 2xx status
		if statusCode < 200 || statusCode >= 300 {
			return fmt.Errorf("unhealthy status code: %d", statusCode)
		}
	}

	// Check expected body substring
	if config.ExpectedBody != "" {
		if !strings.Contains(string(body), config.ExpectedBody) {
			return fmt.Errorf("expected body substring %q not found in response", config.ExpectedBody)
		}
	}

	return nil
}

// recordFailure records a health check failure.
// When a pool endpoint is associated, uses endpoint atomics for failure tracking.
func (hc *HealthChecker) recordFailure(backend *BackendHealth, config *BackendHealthCheckConfig, errMsg string) {
	backend.lastCheck.Store(time.Now().UnixNano())
	backend.lastError.Store(errMsg)
	backend.consecutiveSuccesses.Store(0)

	endpointLabel := backend.BackendURL
	if backend.endpoint != nil && backend.endpoint.Name != "" {
		endpointLabel = backend.endpoint.Name
	}

	unhealthyThreshold := hc.defaultUnhealthyThreshold
	if config.UnhealthyThreshold > 0 {
		unhealthyThreshold = config.UnhealthyThreshold
	}

	if backend.endpoint != nil {
		// Delegate to pool endpoint atomics (shared with circuit breaker)
		failures := backend.endpoint.IncrementFailures()
		if int(failures) >= unhealthyThreshold {
			wasHealthy := backend.endpoint.IsHealthy()
			backend.endpoint.SetUnhealthy()
			if wasHealthy {
				hc.logger.Warn().
					Str(logging.FieldServiceID, backend.ServiceID).
					Str("backend_url", backend.BackendURL).
					Str("endpoint", endpointLabel).
					Str("error", errMsg).
					Int32("consecutive_failures", failures).
					Msg("backend became unhealthy (active health check)")
				backendHealthStatus.WithLabelValues(backend.ServiceID, endpointLabel).Set(0)
			}
		}
	} else {
		// Legacy path: use local atomics
		failures := backend.consecutiveFailures.Add(1)
		if int(failures) >= unhealthyThreshold {
			oldStatus := backend.GetStatus()
			backend.SetStatus(HealthStatusUnhealthy)
			if oldStatus != HealthStatusUnhealthy {
				hc.logger.Warn().
					Str(logging.FieldServiceID, backend.ServiceID).
					Str("backend_url", backend.BackendURL).
					Str("endpoint", endpointLabel).
					Str("error", errMsg).
					Int32("consecutive_failures", failures).
					Msg("backend became unhealthy")
				backendHealthStatus.WithLabelValues(backend.ServiceID, endpointLabel).Set(0)
			}
		}
	}

	healthCheckFailures.WithLabelValues(backend.ServiceID, endpointLabel).Inc()
}

// recordSuccess records a health check success.
// When a pool endpoint is associated, uses endpoint atomics for counter reset.
// On recovery (unhealthy -> healthy transition), performs a full reset of both
// consecutiveFailures and consecutiveSuccesses for a clean slate.
func (hc *HealthChecker) recordSuccess(backend *BackendHealth, config *BackendHealthCheckConfig) {
	backend.lastCheck.Store(time.Now().UnixNano())
	backend.lastError.Store("")

	endpointLabel := backend.BackendURL
	if backend.endpoint != nil && backend.endpoint.Name != "" {
		endpointLabel = backend.endpoint.Name
	}

	healthyThreshold := hc.defaultHealthyThreshold
	if config.HealthyThreshold > 0 {
		healthyThreshold = config.HealthyThreshold
	}

	if backend.endpoint != nil {
		// Delegate to pool endpoint atomics (shared with circuit breaker)
		backend.endpoint.ResetFailures()
		successes := backend.consecutiveSuccesses.Add(1)
		if int(successes) >= healthyThreshold {
			wasUnhealthy := !backend.endpoint.IsHealthy()
			backend.endpoint.SetHealthy()
			if wasUnhealthy {
				// Full reset on recovery: clean slate for the backend
				backend.endpoint.ResetFailures()
				backend.consecutiveSuccesses.Store(0)
				hc.logger.Info().
					Str(logging.FieldServiceID, backend.ServiceID).
					Str("backend_url", backend.BackendURL).
					Str("endpoint", endpointLabel).
					Int32("consecutive_successes", successes).
					Msg("backend became healthy (active health check)")
				backendHealthStatus.WithLabelValues(backend.ServiceID, endpointLabel).Set(1)
			}
		}
	} else {
		// Legacy path: use local atomics
		backend.consecutiveFailures.Store(0)
		successes := backend.consecutiveSuccesses.Add(1)
		if int(successes) >= healthyThreshold {
			oldStatus := backend.GetStatus()
			backend.SetStatus(HealthStatusHealthy)
			if oldStatus != HealthStatusHealthy {
				// Full reset on recovery
				backend.consecutiveSuccesses.Store(0)
				hc.logger.Info().
					Str(logging.FieldServiceID, backend.ServiceID).
					Str("backend_url", backend.BackendURL).
					Str("endpoint", endpointLabel).
					Int32("consecutive_successes", successes).
					Msg("backend became healthy")
				backendHealthStatus.WithLabelValues(backend.ServiceID, endpointLabel).Set(1)
			}
		}
	}

	healthCheckSuccesses.WithLabelValues(backend.ServiceID, endpointLabel).Inc()
}

// CheckNow performs an immediate health check for all endpoints in a pool.
func (hc *HealthChecker) CheckNow(ctx context.Context, poolKey string) error {
	hc.configsMu.RLock()
	config, ok := hc.configs[poolKey]
	hc.configsMu.RUnlock()

	if !ok {
		return fmt.Errorf("no health check config for pool %s", poolKey)
	}

	hc.checkPool(ctx, poolKey, config)
	return nil
}

// Close gracefully shuts down the health checker.
func (hc *HealthChecker) Close() error {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if hc.closed {
		return nil
	}

	hc.closed = true

	if hc.cancelFn != nil {
		hc.cancelFn()
	}

	hc.wg.Wait()

	hc.logger.Info().Msg("health checker closed")
	return nil
}

// joinURLPath properly joins a base URL with a path, handling edge cases like
// trailing slashes and leading slashes to avoid double slashes.
func joinURLPath(baseURL, pathToJoin string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	// Clean up paths to avoid double slashes
	basePath := strings.TrimSuffix(parsed.Path, "/")
	joinPath := pathToJoin
	if !strings.HasPrefix(joinPath, "/") {
		joinPath = "/" + joinPath
	}

	parsed.Path = basePath + joinPath
	return parsed.String(), nil
}
