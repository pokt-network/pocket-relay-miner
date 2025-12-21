// Test WebSocket relay through PATH gateway with session rollover handling
// Usage: go run scripts/ws-test/main.go [service-id] [message-count]
//
// Examples:
//
//	go run scripts/ws-test/main.go                       # 5 messages to develop-websocket
//	go run scripts/ws-test/main.go develop-websocket 10  # 10 messages
//	go run scripts/ws-test/main.go develop-websocket 0   # infinite with auto-reconnect on session rollover
//
// Close Codes:
//
//	1000 - Normal closure
//	1001 - Going away
//	1012 - Service restart
//	4000 - Session expired (Pocket custom - triggers reconnect)
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Custom Pocket close codes
const (
	CloseSessionExpired = 4000
)

type wsClient struct {
	url          string
	serviceID    string
	messageCount int
	header       http.Header
	dialer       websocket.Dialer

	conn      *websocket.Conn
	done      chan struct{}
	closeCode int
	closeText string

	sentCount int
	recvCount int
	mu        sync.Mutex

	reconnectCount int
}

func main() {
	serviceID := "develop-websocket"
	messageCount := 5 // default: send 5 messages

	if len(os.Args) > 1 {
		serviceID = os.Args[1]
	}
	if len(os.Args) > 2 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil {
			messageCount = n
		}
	}

	client := &wsClient{
		url:          "ws://localhost:3069/v1",
		serviceID:    serviceID,
		messageCount: messageCount,
		header:       http.Header{},
		dialer: websocket.Dialer{
			HandshakeTimeout: 5 * time.Second,
		},
	}
	client.header.Set("Target-Service-Id", serviceID)

	fmt.Printf("=== WebSocket Session Rollover Test ===\n")
	fmt.Printf("URL: %s\n", client.url)
	fmt.Printf("Service: %s\n", serviceID)
	if messageCount == 0 {
		fmt.Printf("Mode: Persistent (auto-reconnect on session expiry)\n")
	} else {
		fmt.Printf("Messages: %d\n", messageCount)
	}
	fmt.Println()

	// Handle Ctrl+C gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Main connection loop - reconnects on session expiry
	for {
		if err := client.connect(); err != nil {
			fmt.Printf("[%s] ❌ CONNECTION FAILED: %v\n", timestamp(), err)
			if client.messageCount == 0 {
				fmt.Printf("[%s] Retrying in 3 seconds...\n", timestamp())
				select {
				case <-sigChan:
					fmt.Printf("\n[%s] Interrupted - exiting\n", timestamp())
					client.printSummary()
					return
				case <-time.After(3 * time.Second):
					continue
				}
			}
			os.Exit(1)
		}

		// Run the message loop
		exitReason := client.run(sigChan)

		switch exitReason {
		case "interrupted":
			fmt.Printf("\n[%s] 🛑 Interrupted by user\n", timestamp())
			client.close()
			client.printSummary()
			return

		case "session_expired":
			fmt.Printf("[%s] 🔄 SESSION EXPIRED - Reconnecting...\n", timestamp())
			client.reconnectCount++
			fmt.Printf("[%s] Reconnect #%d in 1 second...\n", timestamp(), client.reconnectCount)
			time.Sleep(1 * time.Second)
			continue // Reconnect

		case "done":
			client.close()
			client.printSummary()
			return

		case "error":
			if client.messageCount == 0 {
				fmt.Printf("[%s] ⚠️ Connection error - Reconnecting in 3 seconds...\n", timestamp())
				client.reconnectCount++
				time.Sleep(3 * time.Second)
				continue
			}
			client.printSummary()
			return
		}
	}
}

func (c *wsClient) connect() error {
	fmt.Printf("[%s] 🔌 Connecting to %s...\n", timestamp(), c.url)

	conn, resp, err := c.dialer.Dial(c.url, c.header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("%v (HTTP %d)", err, resp.StatusCode)
		}
		return err
	}

	c.conn = conn
	c.done = make(chan struct{})
	c.closeCode = 0
	c.closeText = ""

	fmt.Printf("[%s] ✅ CONNECTED\n", timestamp())
	return nil
}

func (c *wsClient) close() {
	if c.conn != nil {
		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client closing")
		_ = c.conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(time.Second))
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *wsClient) run(sigChan chan os.Signal) string {
	// Start reader goroutine
	go c.readLoop()

	msgID := 0
	sendTicker := time.NewTicker(1 * time.Second)
	defer sendTicker.Stop()

	fmt.Printf("[%s] 📤 Sending messages (1 per second)...\n", timestamp())
	fmt.Println()

	for {
		select {
		case <-sigChan:
			return "interrupted"

		case <-c.done:
			// Connection closed - check why
			return c.handleClose()

		case <-sendTicker.C:
			msgID++

			// Check if we've sent enough (if not infinite mode)
			if c.messageCount > 0 && msgID > c.messageCount {
				fmt.Printf("\n[%s] ✅ Sent all %d messages\n", timestamp(), c.messageCount)
				return "done"
			}

			// Alternate between different JSON-RPC methods
			var payload string
			switch msgID % 3 {
			case 1:
				payload = fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":%d}`, msgID)
			case 2:
				payload = fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":%d}`, msgID)
			case 0:
				payload = fmt.Sprintf(`{"jsonrpc":"2.0","method":"net_version","params":[],"id":%d}`, msgID)
			}

			fmt.Printf("[%s] SEND #%d: %s\n", timestamp(), msgID, truncate(payload, 70))

			if err := c.conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				fmt.Printf("[%s] ❌ WRITE ERROR: %v\n", timestamp(), err)
				return "error"
			}

			c.mu.Lock()
			c.sentCount++
			c.mu.Unlock()
		}
	}
}

func (c *wsClient) readLoop() {
	defer close(c.done)

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			// Extract close code if available
			if closeErr, ok := err.(*websocket.CloseError); ok {
				c.closeCode = closeErr.Code
				c.closeText = closeErr.Text
				fmt.Printf("[%s] 🔌 CONNECTION CLOSED: code=%d (%s) text=%q\n",
					timestamp(), closeErr.Code, closeCodeName(closeErr.Code), closeErr.Text)
			} else {
				fmt.Printf("[%s] ❌ READ ERROR: %v\n", timestamp(), err)
			}
			return
		}

		c.mu.Lock()
		c.recvCount++
		count := c.recvCount
		c.mu.Unlock()

		fmt.Printf("[%s] RECV #%d: %s\n", timestamp(), count, truncate(string(message), 80))
	}
}

func (c *wsClient) handleClose() string {
	switch c.closeCode {
	case CloseSessionExpired:
		// Session expired - should reconnect
		return "session_expired"

	case websocket.CloseNormalClosure:
		fmt.Printf("[%s] Connection closed normally\n", timestamp())
		return "done"

	case websocket.CloseGoingAway:
		fmt.Printf("[%s] Server going away\n", timestamp())
		if c.messageCount == 0 {
			return "session_expired" // Treat as reconnectable
		}
		return "done"

	case websocket.CloseServiceRestart:
		fmt.Printf("[%s] Service restarting - will reconnect\n", timestamp())
		if c.messageCount == 0 {
			return "session_expired" // Treat as reconnectable
		}
		return "done"

	default:
		if c.closeCode != 0 {
			fmt.Printf("[%s] Unexpected close code: %d\n", timestamp(), c.closeCode)
		}
		return "error"
	}
}

func (c *wsClient) printSummary() {
	fmt.Println()
	fmt.Printf("=== Summary ===\n")
	fmt.Printf("Total Sent: %d messages\n", c.sentCount)
	fmt.Printf("Total Received: %d messages\n", c.recvCount)
	fmt.Printf("Reconnects: %d\n", c.reconnectCount)
}

func timestamp() string {
	return time.Now().Format("15:04:05.000")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func closeCodeName(code int) string {
	switch code {
	case 1000:
		return "NormalClosure"
	case 1001:
		return "GoingAway"
	case 1002:
		return "ProtocolError"
	case 1003:
		return "UnsupportedData"
	case 1005:
		return "NoStatusReceived"
	case 1006:
		return "AbnormalClosure"
	case 1007:
		return "InvalidPayload"
	case 1008:
		return "PolicyViolation"
	case 1009:
		return "MessageTooBig"
	case 1010:
		return "MandatoryExtension"
	case 1011:
		return "InternalError"
	case 1012:
		return "ServiceRestart"
	case 1013:
		return "TryAgainLater"
	case 1015:
		return "TLSHandshake"
	case 4000:
		return "SessionExpired"
	case 4001:
		return "ValidationFailed"
	case 4002:
		return "StakeLimitExceeded"
	default:
		return "Unknown"
	}
}
