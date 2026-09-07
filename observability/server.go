package observability

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// ServerConfig contains configuration for the observability server.
type ServerConfig struct {
	// MetricsEnabled enables the metrics server.
	MetricsEnabled bool

	// MetricsAddr is the address for the metrics server (e.g., ":9090").
	MetricsAddr string

	// PprofEnabled enables the pprof server.
	PprofEnabled bool

	// PprofAddr is the address for the pprof server (e.g., ":6060").
	PprofAddr string

	// Registry is the Prometheus registry to serve metrics from.
	// Required when MetricsEnabled: Start fails if nil — both binaries pass
	// their combined registry.
	Registry prometheus.Gatherer
}

// ReadinessCheck is a function that returns nil if the service is ready,
// or an error describing why it is not ready.
type ReadinessCheck func(ctx context.Context) error

// Server provides observability endpoints (metrics and pprof).
type Server struct {
	logger        logging.Logger
	config        ServerConfig
	metricsServer *http.Server
	pprofServer   *http.Server
	mu            sync.Mutex

	running        bool
	readinessCheck ReadinessCheck
}

// NewServer creates a new observability server.
func NewServer(logger logging.Logger, config ServerConfig) *Server {
	// Default pprof addr to :6060 for security if not specified
	if config.PprofAddr == "" {
		config.PprofAddr = ":6060"
	}

	return &Server{
		logger: logging.ForComponent(logger, logging.ComponentObservability),
		config: config,
	}
}

// Start begins serving metrics and pprof endpoints.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	if s.config.MetricsEnabled {
		// Start runtime metrics collector
		if err := s.startMetricsServer(ctx); err != nil {
			return err
		}
	}

	if s.config.PprofEnabled {
		if err := s.startPprofServer(ctx); err != nil {
			return err
		}
	}

	s.running = true

	return nil
}

// startMetricsServer starts the Prometheus metrics server.
func (s *Server) startMetricsServer(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.config.MetricsAddr)
	if err != nil {
		s.logger.Error().Err(err).Str("addr", s.config.MetricsAddr).Msg("failed to listen for metrics server")
		return err
	}
	defer func() {
		// Close only if we haven't passed ownership to http.Server
		if s.metricsServer == nil {
			err := ln.Close()
			if err != nil {
				s.logger.Error().Err(err).Msg("failed to close metrics listener")
				return
			}
		}
	}()

	mux := http.NewServeMux()
	// Registry is required: both binaries pass their combined registry and
	// start their own runtime metrics collector. The old nil branch (default
	// promhttp handler + a second collector) was unreachable in production
	// and would have double-registered runtime metrics if it ever ran.
	if s.config.Registry == nil {
		return fmt.Errorf("observability server requires a Registry")
	}
	metricsHandler := promhttp.HandlerFor(s.config.Registry, promhttp.HandlerOpts{})
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK")) //nolint:errcheck // the status code already went out (WriteHeader above), so a failed body write means the client is gone: nothing left to act on
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		check := s.readinessCheck
		s.mu.Unlock()

		if check != nil {
			if err := check(r.Context()); err != nil {
				s.logger.Warn().Err(err).Msg("readiness check failed")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, "Not Ready")
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Ready")) //nolint:errcheck // the status code already went out (WriteHeader above), so a failed body write means the client is gone: nothing left to act on
	})

	s.metricsServer = &http.Server{
		Handler: mux,
	}

	go func() {
		s.logger.Info().Str("addr", s.config.MetricsAddr).Msg("serving metrics")
		if err := s.metricsServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error().Err(err).Msg("metrics server failed")
		}
	}()

	go func() {
		<-ctx.Done()
		// Delegate instead of shutting the server down directly. This used to be
		// a second, parallel shutdown path that discarded its error, so a failure
		// here left no trace while the same failure through Stop() was logged --
		// twins with one of them wired. Going through Stop() removes the second
		// path rather than making it report: it brings the mutex, the `running`
		// guard and the lastErr accumulation with it, and it makes a double
		// Shutdown impossible when a context cancellation and an explicit Stop()
		// race, which they do on every normal shutdown.
		//
		// Observable change: "observability servers stopped" now also appears
		// when the shutdown arrives by context, which today it does not.
		_ = s.Stop() //nolint:errcheck // Stop reports every shutdown failure at Error before returning it; there is no caller here to hand it to
	}()

	return nil
}

// startPprofServer starts the pprof debug server.
func (s *Server) startPprofServer(ctx context.Context) error {
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
	pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// Additional pprof handlers for specific profiles
	pprofMux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	pprofMux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	pprofMux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	pprofMux.Handle("/debug/pprof/block", pprof.Handler("block"))
	pprofMux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	pprofMux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))

	s.pprofServer = &http.Server{
		Addr:    s.config.PprofAddr,
		Handler: pprofMux,
	}

	go func() {
		s.logger.Info().Str("addr", s.config.PprofAddr).Msg("serving pprof")
		if err := s.pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error().Err(err).Msg("pprof server failed")
		}
	}()

	go func() {
		<-ctx.Done()
		// Delegate instead of shutting the server down directly. This used to be
		// a second, parallel shutdown path that discarded its error, so a failure
		// here left no trace while the same failure through Stop() was logged --
		// twins with one of them wired. Going through Stop() removes the second
		// path rather than making it report: it brings the mutex, the `running`
		// guard and the lastErr accumulation with it, and it makes a double
		// Shutdown impossible when a context cancellation and an explicit Stop()
		// race, which they do on every normal shutdown.
		//
		// Observable change: "observability servers stopped" now also appears
		// when the shutdown arrives by context, which today it does not.
		_ = s.Stop() //nolint:errcheck // Stop reports every shutdown failure at Error before returning it; there is no caller here to hand it to
	}()

	return nil
}

// Stop gracefully shuts down the observability servers.
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The guard means "nothing is listening", and that has to stay true or the
	// ctx.Done goroutines below delegate their shutdown into a no-op. TODAY it
	// holds for two reasons, neither of them stated where they live: startPprof
	// cannot fail (its only return is nil -- a bind error surfaces inside its
	// goroutine, not to the caller), and startMetrics closes its listener in a
	// defer whenever it did not hand it to an http.Server. So a Start that
	// returns an error has nothing left up. Give either of those a synchronous
	// failure path and this guard starts lying; the test for it is
	// TestServer_FailedStartLeavesNothingListening.
	if !s.running {
		return nil
	}

	// Whoever gets past the guard shuts BOTH servers down, so this line is never
	// the misleading half of a pair -- unlike the per-server "stopping X server"
	// lines this replaced, which a delegating goroutine would emit without
	// stopping anything. It is kept because Shutdown waits for connections to go
	// idle, up to the timeout below: without a line at the START, a shutdown that
	// hangs leaves the operator with no evidence it even began.
	s.logger.Info().Msg("stopping observability servers")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lastErr := s.shutdownServers(ctx)

	s.running = false
	s.logger.Info().Msg("observability servers stopped")

	return lastErr
}

// shutdownServers stops whichever servers are up and reports every failure,
// returning the last one. Callers hold s.mu.
func (s *Server) shutdownServers(ctx context.Context) error {
	var lastErr error

	if s.metricsServer != nil {
		if err := s.metricsServer.Shutdown(ctx); err != nil {
			s.logger.Error().Err(err).Msg("failed to shutdown metrics server")
			lastErr = err
		}
	}

	if s.pprofServer != nil {
		if err := s.pprofServer.Shutdown(ctx); err != nil {
			s.logger.Error().Err(err).Msg("failed to shutdown pprof server")
			lastErr = err
		}
	}

	return lastErr
}

// SetReadinessCheck sets a readiness check function that the /ready endpoint
// will call. If the check returns an error, /ready returns HTTP 503.
// This can be called after Start() to set checks that depend on components
// initialized later (e.g., Redis client).
func (s *Server) SetReadinessCheck(check ReadinessCheck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readinessCheck = check
}

// IsRunning returns true if the server is running.
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
