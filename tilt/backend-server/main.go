package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	"github.com/pokt-network/pocket-relay-miner/tilt/backend-server/pb"
)

// Config holds backend server configuration.
type Config struct {
	HTTPPort          int     `yaml:"http_port"`
	GRPCPort          int     `yaml:"grpc_port"`
	MetricsPort       int     `yaml:"metrics_port"`
	ErrorRate         float64 `yaml:"error_rate"`          // 0.0-1.0
	ErrorCode         int     `yaml:"error_code"`          // HTTP error code to inject
	DelayMs           int     `yaml:"delay_ms"`            // Delay in milliseconds
	BrokenCompression bool    `yaml:"broken_compression"`  // Compress WITHOUT Content-Encoding header (simulate bug)
}

var (
	// Prometheus metrics
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "demo_requests_total",
			Help: "Total number of requests by protocol and method",
		},
		[]string{"protocol", "method"},
	)

	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "demo_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"protocol"},
	)
)

func init() {
	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(requestDuration)
}

func main() {
	// Load config
	cfg := loadConfig()

	// Start metrics server
	go startMetricsServer(cfg.MetricsPort)

	// Start gRPC server
	go startGRPCServer(cfg)

	// Start HTTP/WebSocket/SSE server
	startHTTPServer(cfg)
}

func loadConfig() *Config {
	cfg := &Config{
		HTTPPort:    8545,
		GRPCPort:    50051,
		MetricsPort: 9095,
		ErrorRate:   0.0,
		ErrorCode:   500,
		DelayMs:     0,
	}

	if data, err := os.ReadFile("config.yaml"); err == nil {
		yaml.Unmarshal(data, cfg)
	}

	return cfg
}

func startMetricsServer(port int) {
	http.Handle("/metrics", promhttp.Handler())
	addr := fmt.Sprintf(":%d", port)
	log.Printf("Metrics server listening on %s", addr)
	http.ListenAndServe(addr, nil)
}

func startHTTPServer(cfg *Config) {
	mux := http.NewServeMux()

	// HTTP JSON-RPC endpoint
	mux.HandleFunc("/", handleJSONRPC(cfg))

	// WebSocket endpoint
	mux.HandleFunc("/ws", handleWebSocket(cfg))

	// SSE streaming endpoint
	mux.HandleFunc("/stream/sse", handleSSE(cfg))

	// NDJSON streaming endpoint
	mux.HandleFunc("/stream/ndjson", handleNDJSON(cfg))

	// Health check
	mux.HandleFunc("/health", handleHealth())

	addr := fmt.Sprintf(":%d", cfg.HTTPPort)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	log.Printf("HTTP server listening on %s", addr)

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down HTTP server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}

func handleJSONRPC(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			requestDuration.WithLabelValues("http").Observe(time.Since(start).Seconds())
		}()

		// Apply delay if configured
		if cfg.DelayMs > 0 {
			time.Sleep(time.Duration(cfg.DelayMs) * time.Millisecond)
		}

		// Inject error if configured
		if shouldInjectError(cfg.ErrorRate) {
			requestsTotal.WithLabelValues("http", "error").Inc()
			http.Error(w, fmt.Sprintf("injected error (code: %d)", cfg.ErrorCode), cfg.ErrorCode)
			return
		}

		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		method, _ := req["method"].(string)
		id, _ := req["id"]

		requestsTotal.WithLabelValues("http", method).Inc()

		// Generic response - echo back method and params
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]interface{}{
				"method": method,
				"params": req["params"],
				"status": "ok",
			},
		}

		// Serialize response to JSON
		respBytes, err := json.Marshal(resp)
		if err != nil {
			http.Error(w, "failed to marshal response", http.StatusInternalServerError)
			return
		}

		// BROKEN COMPRESSION MODE: Simulate backend bug (compress without Content-Encoding header)
		if cfg.BrokenCompression && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			log.Printf("[BROKEN_COMPRESSION_MODE] Client sent Accept-Encoding: gzip - compressing WITHOUT Content-Encoding header")

			// Compress the response
			var buf bytes.Buffer
			gzipWriter := gzip.NewWriter(&buf)
			if _, err := gzipWriter.Write(respBytes); err != nil {
				http.Error(w, "compression failed", http.StatusInternalServerError)
				return
			}
			if err := gzipWriter.Close(); err != nil {
				http.Error(w, "compression close failed", http.StatusInternalServerError)
				return
			}

			// Send compressed response WITHOUT Content-Encoding header (simulate bug)
			w.Header().Set("Content-Type", "application/json")
			// NOTE: Deliberately NOT setting Content-Encoding header to simulate broken backend
			w.Write(buf.Bytes())
			log.Printf("[BROKEN_COMPRESSION_MODE] Sent gzipped response (%d bytes compressed from %d bytes) WITHOUT Content-Encoding header", buf.Len(), len(respBytes))
			return
		}

		// Normal mode: send uncompressed response
		w.Header().Set("Content-Type", "application/json")
		w.Write(respBytes)
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleWebSocket(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade error: %v", err)
			return
		}
		defer conn.Close()

		requestsTotal.WithLabelValues("websocket", "connection").Inc()

		// Echo server: read message and send it back N times
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}

			// Parse message as JSON (works for both TextMessage and BinaryMessage)
			// The relayer sends opaque bytes - we don't care about the WebSocket message type
			var jsonMsg map[string]interface{}
			if err := json.Unmarshal(msg, &jsonMsg); err == nil {
				// Valid JSON message - process it
				{
					method, _ := jsonMsg["method"].(string)
					id, _ := jsonMsg["id"]

					requestsTotal.WithLabelValues("websocket", method).Inc()

					// Check for test instructions (repeat_count and delay_ms)
					repeatCount := 1
					delayMs := 0
					if params, ok := jsonMsg["params"].([]interface{}); ok && len(params) > 0 {
						if testParams, ok := params[0].(map[string]interface{}); ok {
							if rc, ok := testParams["repeat_count"].(float64); ok {
								repeatCount = int(rc)
							}
							if dm, ok := testParams["delay_ms"].(float64); ok {
								delayMs = int(dm)
							}
						}
					}

					// Send multiple responses if instructed (simulates subscriptions)
					for i := 0; i < repeatCount; i++ {
						// Generic response - echo back method and params with sequence number
						resp := map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      id,
							"result": map[string]interface{}{
								"method":   method,
								"params":   jsonMsg["params"],
								"sequence": i + 1,
								"total":    repeatCount,
								"status":   "ok",
							},
						}

						if err := conn.WriteJSON(resp); err != nil {
							log.Printf("Failed to send response %d: %v", i+1, err)
							break
						}

						// Delay before next response (except for last one)
						if i < repeatCount-1 && delayMs > 0 {
							time.Sleep(time.Duration(delayMs) * time.Millisecond)
						}
					}
				}
			}
			// If JSON parsing failed, ignore the message (not a valid JSON-RPC request)
		}
	}
}

func handleSSE(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.WithLabelValues("sse", "stream").Inc()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		sequence := 0
		for range ticker.C {
			sequence++
			event := map[string]interface{}{
				"type":      "update",
				"sequence":  sequence,
				"timestamp": time.Now().Unix(),
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func handleNDJSON(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.WithLabelValues("ndjson", "stream").Inc()

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		sequence := 0
		for range ticker.C {
			sequence++
			event := map[string]interface{}{
				"type":      "update",
				"sequence":  sequence,
				"timestamp": time.Now().Unix(),
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "%s\n", data)
			flusher.Flush()
		}
	}
}

func handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"status":  "healthy",
			"service": "demo-backend",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// gRPC server implementation
type demoServer struct {
	pb.UnimplementedDemoServiceServer
	cfg *Config
}

func (s *demoServer) GetBlockHeight(ctx context.Context, req *pb.Empty) (*pb.BlockHeightResponse, error) {
	requestsTotal.WithLabelValues("grpc", "GetBlockHeight").Inc()
	// Generic response - just echo back a height value
	return &pb.BlockHeightResponse{Height: 1}, nil
}

func (s *demoServer) GetBlock(ctx context.Context, req *pb.BlockRequest) (*pb.BlockResponse, error) {
	requestsTotal.WithLabelValues("grpc", "GetBlock").Inc()
	// Generic response - echo back the request data with timestamp
	return &pb.BlockResponse{
		Number:       req.Number,
		Hash:         fmt.Sprintf("hash-%d", req.Number),
		Timestamp:    time.Now().Unix(),
		Transactions: []string{fmt.Sprintf("tx-%d", req.Number)},
	}, nil
}

func (s *demoServer) StreamBlocks(req *pb.StreamRequest, stream pb.DemoService_StreamBlocksServer) error {
	requestsTotal.WithLabelValues("grpc", "StreamBlocks").Inc()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	sequence := req.StartHeight
	for range ticker.C {
		// Generic streaming response
		block := &pb.BlockResponse{
			Number:       sequence,
			Hash:         fmt.Sprintf("hash-%d", sequence),
			Timestamp:    time.Now().Unix(),
			Transactions: []string{fmt.Sprintf("tx-%d", sequence)},
		}
		if err := stream.Send(block); err != nil {
			return err
		}
		sequence++
	}
	return nil
}

func (s *demoServer) HealthCheck(ctx context.Context, req *pb.Empty) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		Healthy: true,
		Status:  "ok",
	}, nil
}

func startGRPCServer(cfg *Config) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterDemoServiceServer(grpcServer, &demoServer{cfg: cfg})

	log.Printf("gRPC server listening on :%d", cfg.GRPCPort)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("gRPC server error: %v", err)
	}
}

// Helper functions

func shouldInjectError(rate float64) bool {
	return false // Simplified: no random errors for now
}
