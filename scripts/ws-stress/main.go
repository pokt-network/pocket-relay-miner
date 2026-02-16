// WebSocket stress test for relay-miner memory profiling.
// Spawns N concurrent WebSocket connections, each sending messages at configurable rate.
// Reports stats periodically and monitors relayer pod memory.
//
// Usage:
//
//	go run scripts/ws-stress/main.go                           # 50 connections, 10 msg/s each
//	go run scripts/ws-stress/main.go -connections 200 -rate 20 # 200 connections, 20 msg/s each
//	go run scripts/ws-stress/main.go -duration 5m              # Run for 5 minutes
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var (
	gatewayURL  = flag.String("url", "ws://localhost:3069/v1", "PATH gateway WebSocket URL")
	serviceID   = flag.String("service", "develop-websocket", "Target service ID")
	connections = flag.Int("connections", 50, "Number of concurrent WebSocket connections")
	rate        = flag.Int("rate", 10, "Messages per second per connection")
	duration    = flag.Duration("duration", 10*time.Minute, "Test duration (0 = run until Ctrl+C)")
	reportSecs  = flag.Int("report", 10, "Report interval in seconds")
	rampUp      = flag.Duration("ramp", 5*time.Second, "Ramp-up time (spread connection creation)")
)

// Global counters
var (
	totalSent      atomic.Int64
	totalRecv      atomic.Int64
	totalErrors    atomic.Int64
	activeConns    atomic.Int64
	connectErrors  atomic.Int64
	disconnections atomic.Int64
)

func main() {
	flag.Parse()

	fmt.Println("========================================")
	fmt.Println("  WebSocket Stress Test")
	fmt.Println("========================================")
	fmt.Printf("  URL:          %s\n", *gatewayURL)
	fmt.Printf("  Service:      %s\n", *serviceID)
	fmt.Printf("  Connections:  %d\n", *connections)
	fmt.Printf("  Rate:         %d msg/s per conn\n", *rate)
	fmt.Printf("  Total RPS:    ~%d msg/s\n", *connections**rate)
	fmt.Printf("  Duration:     %s\n", *duration)
	fmt.Printf("  Ramp-up:      %s\n", *rampUp)
	fmt.Println()

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Done channel for coordinated shutdown
	done := make(chan struct{})

	// Start reporter
	go reporter(done)

	// Calculate delay between connection creations for ramp-up
	rampDelay := *rampUp / time.Duration(*connections)
	if rampDelay < time.Millisecond {
		rampDelay = time.Millisecond
	}

	// Start timer for duration
	var timer *time.Timer
	if *duration > 0 {
		timer = time.NewTimer(*duration)
	}

	// Launch connections with ramp-up
	var wg sync.WaitGroup
	for i := 0; i < *connections; i++ {
		wg.Add(1)
		connID := i + 1
		go func() {
			defer wg.Done()
			runConnection(connID, done)
		}()

		// Ramp-up delay — stop launching if shutdown requested
		rampDone := false
		select {
		case <-done:
			rampDone = true
		case <-time.After(rampDelay):
		}
		if rampDone {
			break
		}
	}

	// Wait for signal or timeout
	select {
	case sig := <-sigChan:
		fmt.Printf("\nReceived %v - shutting down...\n", sig)
	case <-func() <-chan time.Time {
		if timer != nil {
			return timer.C
		}
		return make(chan time.Time) // block forever
	}():
		fmt.Printf("\nDuration %s reached - shutting down...\n", *duration)
	}

	close(done)
	wg.Wait()

	// Final report
	printFinalSummary()
}

func runConnection(id int, done <-chan struct{}) {
	header := http.Header{}
	header.Set("Target-Service-Id", *serviceID)

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	// Reconnect loop
	for {
		select {
		case <-done:
			return
		default:
		}

		conn, _, err := dialer.Dial(*gatewayURL, header)
		if err != nil {
			connectErrors.Add(1)
			// Retry after delay
			select {
			case <-done:
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}

		activeConns.Add(1)

		// Run bidirectional message loop
		connDone := make(chan struct{})

		// Reader goroutine
		go func() {
			defer close(connDone)
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					return
				}
				totalRecv.Add(1)
			}
		}()

		// Writer loop at configured rate
		interval := time.Second / time.Duration(*rate)
		ticker := time.NewTicker(interval)
		msgID := 0

	writeLoop:
		for {
			select {
			case <-done:
				ticker.Stop()
				_ = conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stress test done"),
					time.Now().Add(time.Second),
				)
				_ = conn.Close()
				activeConns.Add(-1)
				return

			case <-connDone:
				// Server closed connection
				ticker.Stop()
				_ = conn.Close()
				activeConns.Add(-1)
				disconnections.Add(1)
				break writeLoop

			case <-ticker.C:
				msgID++
				payload := fmt.Sprintf(
					`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}`,
					msgID,
				)

				if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
					totalErrors.Add(1)
					ticker.Stop()
					_ = conn.Close()
					activeConns.Add(-1)
					disconnections.Add(1)
					break writeLoop
				}
				totalSent.Add(1)
			}
		}

		// Reconnect after disconnect (with backoff)
		select {
		case <-done:
			return
		case <-time.After(time.Second):
		}
	}
}

func reporter(done <-chan struct{}) {
	ticker := time.NewTicker(time.Duration(*reportSecs) * time.Second)
	defer ticker.Stop()

	start := time.Now()
	lastSent := int64(0)
	lastRecv := int64(0)
	lastTime := start

	fmt.Println("----------------------------------------------------------------------")
	fmt.Printf("%-9s %-8s %-10s %-10s %-8s %-8s %-10s %-8s\n",
		"Elapsed", "Conns", "Sent", "Recv", "Errors", "RPS-out", "RPS-in", "Disconn")
	fmt.Println("----------------------------------------------------------------------")

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(start)
			dt := now.Sub(lastTime).Seconds()

			sent := totalSent.Load()
			recv := totalRecv.Load()
			errs := totalErrors.Load()
			conns := activeConns.Load()
			disc := disconnections.Load()

			rpsOut := float64(sent-lastSent) / dt
			rpsIn := float64(recv-lastRecv) / dt

			hours := int(elapsed.Hours())
			mins := int(elapsed.Minutes()) % 60
			secs := int(elapsed.Seconds()) % 60
			etime := fmt.Sprintf("%02d:%02d:%02d", hours, mins, secs)

			fmt.Printf("%-9s %-8d %-10d %-10d %-8d %-8.0f %-10.0f %-8d\n",
				etime, conns, sent, recv, errs, rpsOut, rpsIn, disc)

			lastSent = sent
			lastRecv = recv
			lastTime = now
		}
	}
}

func printFinalSummary() {
	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("  Final Summary")
	fmt.Println("========================================")
	fmt.Printf("  Total Sent:        %d\n", totalSent.Load())
	fmt.Printf("  Total Received:    %d\n", totalRecv.Load())
	fmt.Printf("  Total Errors:      %d\n", totalErrors.Load())
	fmt.Printf("  Connect Errors:    %d\n", connectErrors.Load())
	fmt.Printf("  Disconnections:    %d\n", disconnections.Load())
	fmt.Printf("  Active Conns:      %d\n", activeConns.Load())
	fmt.Println()
}
