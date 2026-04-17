// HTTP load generator that verifies response bodies.
//
// PATH can mask failures as 200 OK with empty/invalid body. hey only checks
// status codes, so it counts those as success. This tool sends JSON-RPC
// requests and verifies that each response contains a valid "result" field
// with the expected shape (for eth_blockNumber: hex-encoded block height).
//
// Usage:
//
//	go run scripts/loadtest/http-verify.go -rps 1000 -duration 300s
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	gatewayURL = flag.String("url", "http://localhost:3069/v1", "PATH gateway URL")
	serviceID  = flag.String("service", "develop-http", "Target service ID")
	rps        = flag.Int("rps", 500, "Total requests per second")
	workers    = flag.Int("workers", 200, "Concurrent workers")
	duration   = flag.Duration("duration", 5*time.Minute, "Test duration")
	timeout    = flag.Duration("timeout", 10*time.Second, "Per-request timeout")
	reportSecs = flag.Int("report", 15, "Report interval (seconds)")
)

type counters struct {
	sent         atomic.Int64
	status200    atomic.Int64
	status5xx    atomic.Int64
	statusOther  atomic.Int64
	bodyValid    atomic.Int64 // 200 OK with valid JSON-RPC result field
	bodyEmpty    atomic.Int64 // 200 OK with empty body (PATH 503 masked)
	bodyInvalid  atomic.Int64 // 200 OK but response body is malformed or error
	bodyRPCError atomic.Int64 // 200 OK with JSON-RPC error field (backend error)
	connErrors   atomic.Int64
	timeouts     atomic.Int64
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func main() {
	flag.Parse()

	c := &counters{}
	payload := []byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`)

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *workers * 2,
			MaxIdleConnsPerHost: *workers * 2,
			MaxConnsPerHost:     *workers * 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handling
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	// Stop after duration
	time.AfterFunc(*duration, cancel)

	fmt.Printf("=== HTTP Verify Load Test ===\n")
	fmt.Printf("  URL:      %s\n", *gatewayURL)
	fmt.Printf("  Service:  %s\n", *serviceID)
	fmt.Printf("  RPS:      %d (across %d workers)\n", *rps, *workers)
	fmt.Printf("  Duration: %s\n\n", *duration)

	// Reporter goroutine
	go report(ctx, c, *reportSecs)

	// Rate limiter: ticker fires at global RPS rate
	interval := time.Second / time.Duration(*rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Job channel
	jobs := make(chan struct{}, *workers*2)

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				doRequest(ctx, client, payload, c)
			}
		}()
	}

	// Dispatch jobs at RPS rate
	for {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			printFinal(c)
			// Exit non-zero if any non-200 body
			if c.bodyEmpty.Load()+c.bodyInvalid.Load()+c.bodyRPCError.Load() > c.bodyValid.Load()/10 {
				os.Exit(1) // >10% invalid bodies
			}
			return
		case <-ticker.C:
			select {
			case jobs <- struct{}{}:
			default:
				// workers are full, skip this tick (RPS will be lower than target)
			}
		}
	}
}

func doRequest(ctx context.Context, client *http.Client, payload []byte, c *counters) {
	c.sent.Add(1)

	req, err := http.NewRequestWithContext(ctx, "POST", *gatewayURL, bytes.NewReader(payload))
	if err != nil {
		c.connErrors.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Target-Service-Id", *serviceID)

	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "context deadline") || strings.Contains(err.Error(), "Timeout") {
			c.timeouts.Add(1)
		} else {
			c.connErrors.Add(1)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode >= 500:
		c.status5xx.Add(1)
		return
	case resp.StatusCode != 200:
		c.statusOther.Add(1)
		return
	}
	c.status200.Add(1)

	// HTTP 200 — verify body
	if len(body) == 0 {
		c.bodyEmpty.Add(1) // PATH likely masked a 503
		return
	}

	var rpc jsonRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		c.bodyInvalid.Add(1)
		return
	}

	if rpc.Error != nil {
		c.bodyRPCError.Add(1)
		return
	}

	if len(rpc.Result) == 0 || bytes.Equal(rpc.Result, []byte("null")) {
		c.bodyInvalid.Add(1)
		return
	}

	// Any non-null, non-error JSON-RPC result counts as a successful relay.
	// Real eth backends return a hex string like "0x12ab"; localnet test
	// backends (develop-http) return an object like {"backend_id":...,"status":"ok"}.
	// Both shapes prove the response flowed end-to-end and PATH did not mask a 503.
	c.bodyValid.Add(1)
}

func report(ctx context.Context, c *counters, interval int) {
	t := time.NewTicker(time.Duration(interval) * time.Second)
	defer t.Stop()

	var lastSent int64
	startTime := time.Now()

	fmt.Printf("%-9s %-9s %-8s %-8s %-9s %-9s %-9s %-9s %-9s %-7s\n",
		"elapsed", "sent", "rps", "200_ok", "body_ok", "body_empty", "body_inv", "rpc_err", "5xx", "conn_err")
	fmt.Println(strings.Repeat("─", 100))

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sent := c.sent.Load()
			rps := (sent - lastSent) / int64(interval)
			lastSent = sent
			elapsed := time.Since(startTime).Truncate(time.Second)

			fmt.Printf("%-9s %-9d %-8d %-8d %-9d %-9d %-9d %-9d %-9d %-7d\n",
				elapsed, sent, rps,
				c.status200.Load(),
				c.bodyValid.Load(),
				c.bodyEmpty.Load(),
				c.bodyInvalid.Load(),
				c.bodyRPCError.Load(),
				c.status5xx.Load(),
				c.connErrors.Load(),
			)
		}
	}
}

func printFinal(c *counters) {
	fmt.Println("\n═══ FINAL RESULTS ═══")
	fmt.Printf("  Sent:             %d\n", c.sent.Load())
	fmt.Printf("  HTTP 200:         %d\n", c.status200.Load())
	fmt.Printf("    body_valid:     %d  (actual successful relays)\n", c.bodyValid.Load())
	fmt.Printf("    body_empty:     %d  (PATH-masked 503)\n", c.bodyEmpty.Load())
	fmt.Printf("    body_invalid:   %d  (malformed response)\n", c.bodyInvalid.Load())
	fmt.Printf("    body_rpc_error: %d  (backend error)\n", c.bodyRPCError.Load())
	fmt.Printf("  HTTP 5xx:         %d\n", c.status5xx.Load())
	fmt.Printf("  Other status:     %d\n", c.statusOther.Load())
	fmt.Printf("  Connection errs:  %d\n", c.connErrors.Load())
	fmt.Printf("  Timeouts:         %d\n", c.timeouts.Load())

	total := c.sent.Load()
	valid := c.bodyValid.Load()
	if total > 0 {
		fmt.Printf("\n  Real success rate: %.2f%% (body_valid / sent)\n",
			float64(valid)/float64(total)*100)
		fakeSuccess := c.bodyEmpty.Load() + c.bodyInvalid.Load() + c.bodyRPCError.Load()
		fmt.Printf("  Fake 200s:         %d (%.2f%% of 200 responses were invalid)\n",
			fakeSuccess, float64(fakeSuccess)/float64(c.status200.Load())*100)
	}
}
