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
	// If nil, the default registry is used.
	Registry prometheus.Gatherer
}

// DefaultServerConfig returns sensible defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		MetricsEnabled: true,
		MetricsAddr:    ":9090",
		PprofEnabled:   false,
		PprofAddr:      ":6060",
	}
}

// ReadinessCheck is a function that returns nil if the service is ready,
// or an error describing why it is not ready.
type ReadinessCheck func(ctx context.Context) error

// Server provides observability endpoints (metrics and pprof).
type Server struct {
	logger         logging.Logger
	config         ServerConfig
	metricsServer  *http.Server
	pprofServer    *http.Server
	mu             sync.Mutex
	rm             *RuntimeMetricsCollector
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

	startTime := time.Now()

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
	StartupDurationSeconds.WithLabelValues("observability_server").Set(time.Since(startTime).Seconds())

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

	// Start runtime metrics collector using MinerFactory (metrics go to MinerRegistry).
	// Only start when using default registry - skip for custom registries (tests) to avoid
	// duplicate registration errors since MinerFactory uses the global MinerRegistry.
	if s.config.Registry == nil {
		s.rm = NewRuntimeMetricsCollector(
			s.logger,
			DefaultRuntimeMetricsCollectorConfig(),
			MinerFactory,
		)
		if err := s.rm.Start(ctx); err != nil {
			return fmt.Errorf("failed to start runtime metrics collector: %w", err)
		}
		s.logger.Info().Msg("runtime metrics collector started")
	}

	mux := http.NewServeMux()
	// Use custom registry if provided, otherwise use default
	var metricsHandler http.Handler
	if s.config.Registry != nil {
		metricsHandler = promhttp.HandlerFor(s.config.Registry, promhttp.HandlerOpts{})
	} else {
		metricsHandler = promhttp.Handler()
	}
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		check := s.readinessCheck
		s.mu.Unlock()

		if check != nil {
			if err := check(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "Not Ready: %s", err.Error())
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Ready"))
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
		s.logger.Info().Msg("stopping metrics server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.metricsServer.Shutdown(shutdownCtx)
		if s.rm != nil {
			s.rm.Stop()
		}
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
		s.logger.Info().Msg("stopping pprof server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.pprofServer.Shutdown(shutdownCtx)
	}()

	return nil
}

// Stop gracefully shuts down the observability servers.
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var lastErr error

	if s.metricsServer != nil {
		if err := s.metricsServer.Shutdown(ctx); err != nil {
			s.logger.Error().Err(err).Msg("failed to shutdown metrics server")
			lastErr = err
		}
	}

	if s.rm != nil {
		s.rm.Stop()
	}

	if s.pprofServer != nil {
		if err := s.pprofServer.Shutdown(ctx); err != nil {
			s.logger.Error().Err(err).Msg("failed to shutdown pprof server")
			lastErr = err
		}
	}

	s.running = false
	s.logger.Info().Msg("observability servers stopped")

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
