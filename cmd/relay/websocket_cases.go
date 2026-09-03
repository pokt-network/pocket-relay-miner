package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"

	"github.com/pokt-network/pocket-relay-miner/client/relay_client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/relayer"
)

// WebSocket adversarial cases.
//
// The load test proves the happy path: a pool of connections, one signed
// RelayRequest in and one signed RelayResponse out, sixty times. Everything a
// WebSocket does BESIDES that -- a client that connects and says nothing, one
// that sends bytes that are not a relay, one that swaps supplier mid-connection,
// one that drops without a close frame, and a subscription where one request
// produces many billable pushes -- had no coverage at any level, which is how
// four teardown defects and a connection leak lived in this path at once.
//
// Each case asserts what the RELAYER did, from the client's side, and fails the
// process on a mismatch. That is what makes them usable as live gate cells
// rather than something a human reads.

// wsCases is the closed set, used both to dispatch and to report an unknown
// name with the real list rather than a guess.
var wsCases = []string{
	"subscribe",
	"hang",
	"garbage",
	"abrupt-disconnect",
	"supplier-change",
	"no-session-header",
	"oversized",
}

// RunWebSocketCase runs one adversarial case against a live relayer.
func RunWebSocketCase(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient) error {
	name := strings.ToLower(strings.TrimSpace(RelayWSCase))

	logger.Info().
		Str("case", name).
		Str("handshake", RelayWSHandshake).
		Str("service", RelayServiceID).
		Msg("running websocket adversarial case")

	switch name {
	case "subscribe":
		return wsCaseSubscribe(ctx, logger, client)
	case "hang":
		return wsCaseHang(logger)
	case "garbage":
		return wsCaseGarbage(logger)
	case "abrupt-disconnect":
		return wsCaseAbruptDisconnect(ctx, logger, client)
	case "supplier-change":
		return wsCaseSupplierChange(ctx, logger, client)
	case "no-session-header":
		return wsCaseNoSessionHeader(ctx, logger, client)
	case "oversized":
		return wsCaseOversized(logger)
	default:
		return fmt.Errorf("unknown --ws-case %q; valid cases: %s", RelayWSCase, strings.Join(wsCases, ", "))
	}
}

// dialCase opens a connection in the shape --ws-handshake selects.
func dialCase() (*websocket.Conn, error) {
	return connectWebSocket(RelayRelayerURL, RelayServiceID, RelaySupplierAddr)
}

// expectClose reads until the connection ends and requires it to have ended with
// a specific close code.
//
// It asserts the CODE and not merely "an error", because the code is the whole
// message: 4001 says the relayer judged the CLIENT, 1013 says try again, 1009
// says the frame was too big, and a read timeout says the relayer did nothing at
// all -- which is the failure most of these cases exist to catch.
func expectClose(conn *websocket.Conn, wantCode int, within time.Duration, what string) error {
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return fmt.Errorf("%s: could not set read deadline: %w", what, err)
	}
	for {
		_, data, err := conn.ReadMessage()
		if err == nil {
			// A data frame before the close: keep reading, but say so, because
			// "it served us something first" changes what the case proved.
			if len(data) > 0 {
				continue
			}
			continue
		}
		if websocket.IsCloseError(err, wantCode) {
			return nil
		}
		return fmt.Errorf("%s: expected close %d (%s), got %v",
			what, wantCode, closeCodeLabel(wantCode), err)
	}
}

func closeCodeLabel(code int) string {
	switch code {
	case relayer.CloseValidationFailed:
		return "validation failed"
	case relayer.CloseTryAgainLater:
		return "try again later"
	case relayer.CloseMessageTooBig:
		return "message too big"
	case relayer.CloseStakeLimitExceeded:
		return "stake limit exceeded"
	default:
		return "unnamed"
	}
}

// wsCaseSubscribe is the case a WebSocket exists FOR, and the one the load test
// cannot reach: one RelayRequest, many backend pushes, and EVERY push is a
// billable relay paired with the same request.
//
// The localnet backend produces it from a repeat_count parameter
// (tilt/backend-server/main.go); against another backend, pass a real
// subscription payload with --payload.
func wsCaseSubscribe(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient) error {
	const pushes = 5

	payload := RelayPayloadJSON
	if payload == "" {
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "eth_subscribe",
			"params":  []any{map[string]any{"repeat_count": pushes}},
		})
		if err != nil {
			return fmt.Errorf("subscribe: could not build payload: %w", err)
		}
		payload = string(body)
	}

	_, relayBz, err := buildRelayRequest(ctx, client, RelayServiceID, RelaySupplierAddr, []byte(payload))
	if err != nil {
		return fmt.Errorf("subscribe: could not build relay: %w", err)
	}

	conn, err := dialCase()
	if err != nil {
		return fmt.Errorf("subscribe: dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(websocket.BinaryMessage, relayBz); err != nil {
		return fmt.Errorf("subscribe: write failed: %w", err)
	}

	for i := 1; i <= pushes; i++ {
		if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return err
		}
		_, respBz, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("subscribe: push %d/%d never arrived: %w", i, pushes, err)
		}
		var resp servicetypes.RelayResponse
		if err := resp.Unmarshal(respBz); err != nil {
			return fmt.Errorf("subscribe: push %d is not a RelayResponse: %w", i, err)
		}
		if len(resp.Meta.SupplierOperatorSignature) == 0 {
			return fmt.Errorf("subscribe: push %d came back UNSIGNED", i)
		}
		logger.Info().Int("push", i).Int("of", pushes).Msg("signed push received")
	}

	logger.Info().
		Int("pushes", pushes).
		Msg("PASS subscribe: one request, every push signed -- check the miner billed the same number")
	return nil
}

// wsCaseHang connects and says nothing. The relayer must close it on the
// first-frame deadline: ping/pong cannot reclaim this connection, because the
// peer is alive and answering.
func wsCaseHang(logger logging.Logger) error {
	conn, err := dialCase()
	if err != nil {
		return fmt.Errorf("hang: dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Answer pings like a well-behaved gateway with nothing to say yet. This is
	// what makes the case interesting: under a plain read deadline this
	// connection would live forever.
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	wait := time.Duration(RelayTimeout) * time.Second
	if wait <= 0 {
		wait = 3 * time.Minute
	}
	logger.Info().Dur("waiting_up_to", wait).Msg("connected and deliberately silent")

	if err := expectClose(conn, relayer.CloseTryAgainLater, wait, "hang"); err != nil {
		return err
	}
	logger.Info().Msg("PASS hang: the relayer closed a connection that never sent a frame")
	return nil
}

// wsCaseGarbage sends bytes that cannot be a RelayRequest before any relay has
// established the connection. Nothing may reach the operator's backend.
func wsCaseGarbage(logger logging.Logger) error {
	conn, err := dialCase()
	if err != nil {
		return fmt.Errorf("garbage: dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// 0xFF cannot decode as protobuf: it is an unending varint continuation.
	if err := conn.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{0xFF}, 64)); err != nil {
		return fmt.Errorf("garbage: write failed: %w", err)
	}

	if err := expectClose(conn, relayer.CloseValidationFailed, 30*time.Second, "garbage"); err != nil {
		return err
	}
	logger.Info().Msg("PASS garbage: a frame that is not a relay closed the connection")
	return nil
}

// wsCaseSupplierChange serves one relay and then names a DIFFERENT supplier on
// the same connection. A bridge has one owner: the second frame must close it.
//
// Without that rule the connection would keep signing as the first supplier
// while metering against the second.
func wsCaseSupplierChange(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient) error {
	payloadBz, err := buildWebSocketPayload()
	if err != nil {
		return err
	}
	_, firstBz, err := buildRelayRequest(ctx, client, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("supplier-change: could not build the first relay: %w", err)
	}

	conn, err := dialCase()
	if err != nil {
		return fmt.Errorf("supplier-change: dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(websocket.BinaryMessage, firstBz); err != nil {
		return fmt.Errorf("supplier-change: write failed: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		return fmt.Errorf("supplier-change: the establishing relay was not served: %w", err)
	}

	// Same frame, different supplier. The signature no longer matches, which is
	// fine: the owner gate runs before validation, and THAT is what is under
	// test -- a connection may not change hands.
	var second servicetypes.RelayRequest
	if err := second.Unmarshal(firstBz); err != nil {
		return err
	}
	second.Meta.SupplierOperatorAddress = flipAddress(second.Meta.SupplierOperatorAddress)
	secondBz, err := second.Marshal()
	if err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, secondBz); err != nil {
		return fmt.Errorf("supplier-change: second write failed: %w", err)
	}

	if err := expectClose(conn, relayer.CloseValidationFailed, 30*time.Second, "supplier-change"); err != nil {
		return err
	}
	logger.Info().Msg("PASS supplier-change: a frame naming another supplier closed the connection")
	return nil
}

// wsCaseNoSessionHeader sends a relay with no session header. It used to panic
// the handler goroutine one line past the owner gate.
func wsCaseNoSessionHeader(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient) error {
	payloadBz, err := buildWebSocketPayload()
	if err != nil {
		return err
	}
	_, relayBz, err := buildRelayRequest(ctx, client, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("no-session-header: could not build relay: %w", err)
	}

	var req servicetypes.RelayRequest
	if err := req.Unmarshal(relayBz); err != nil {
		return err
	}
	req.Meta.SessionHeader = nil
	headerless, err := req.Marshal()
	if err != nil {
		return err
	}

	conn, err := dialCase()
	if err != nil {
		return fmt.Errorf("no-session-header: dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(websocket.BinaryMessage, headerless); err != nil {
		return fmt.Errorf("no-session-header: write failed: %w", err)
	}

	if err := expectClose(conn, relayer.CloseValidationFailed, 30*time.Second, "no-session-header"); err != nil {
		return err
	}
	logger.Info().Msg("PASS no-session-header: refused with a close code instead of a panic")
	return nil
}

// wsCaseOversized sends a frame past the inbound cap. The relayer must refuse it
// rather than buffer it whole: this is the pre-auth memory path.
func wsCaseOversized(logger logging.Logger) error {
	conn, err := dialCase()
	if err != nil {
		return fmt.Errorf("oversized: dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// One byte over the relayer's 15MiB cap.
	oversized := bytes.Repeat([]byte{0xFF}, 15*1024*1024+1)
	_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	// The write may fail: the relayer stops reading and closes as soon as the
	// limit is passed, so a broken pipe mid-write is the SAME verdict. Only the
	// close code is asserted.
	_ = conn.WriteMessage(websocket.BinaryMessage, oversized)

	if err := expectClose(conn, relayer.CloseMessageTooBig, 60*time.Second, "oversized"); err != nil {
		return err
	}
	logger.Info().Msg("PASS oversized: a frame past the cap was refused")
	return nil
}

// wsCaseAbruptDisconnect drops a served connection without a close frame and
// then proves the relayer is still healthy by serving a fresh one.
//
// NOT GATE-READY: measured 2026-09-03, this case FLAKES about one run in five.
// The failure is the follow-up connection reading close 1006 (abnormal, no close
// frame) -- while the relayer's own log for that same connection says it emitted
// a relay on it, so the relayer served it and something raced afterwards. The
// cause is NOT diagnosed; do not add this to scripts/gates/live.sh until it is,
// because a flaky gate cell is worse than an absent one. The other six cases ran
// clean, individually and in sequence.
//
// It asserts from the client because that is what an operator can see. A bridge
// left wedged by an abandoned connection holds a socket, its goroutines and a
// slot in the active-connections gauge, and the only thing a client can observe
// about that is whether the NEXT connection still works.
func wsCaseAbruptDisconnect(ctx context.Context, logger logging.Logger, client *relay_client.RelayClient) error {
	payloadBz, err := buildWebSocketPayload()
	if err != nil {
		return err
	}
	_, relayBz, err := buildRelayRequest(ctx, client, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("abrupt-disconnect: could not build relay: %w", err)
	}

	conn, err := dialCase()
	if err != nil {
		return fmt.Errorf("abrupt-disconnect: dial failed: %w", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, relayBz); err != nil {
		return fmt.Errorf("abrupt-disconnect: write failed: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		return fmt.Errorf("abrupt-disconnect: the relay was not served: %w", err)
	}

	// No close frame, no WebSocket goodbye: the TCP connection just goes.
	if err := conn.NetConn().Close(); err != nil {
		return fmt.Errorf("abrupt-disconnect: could not drop the socket: %w", err)
	}
	logger.Info().Msg("dropped the socket without a close frame")

	// A relayer that wedged on the abandoned bridge cannot serve this.
	_, secondBz, err := buildRelayRequest(ctx, client, RelayServiceID, RelaySupplierAddr, payloadBz)
	if err != nil {
		return fmt.Errorf("abrupt-disconnect: could not build the follow-up relay: %w", err)
	}
	next, err := dialCase()
	if err != nil {
		return fmt.Errorf("abrupt-disconnect: the relayer refused a new connection afterwards: %w", err)
	}
	defer func() { _ = next.Close() }()
	if err := next.WriteMessage(websocket.BinaryMessage, secondBz); err != nil {
		return fmt.Errorf("abrupt-disconnect: follow-up write failed: %w", err)
	}
	if err := next.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	respBz, err := readSignedResponse(next)
	if err != nil {
		return fmt.Errorf("abrupt-disconnect: the relayer did not serve a fresh connection afterwards: %w", err)
	}
	logger.Info().
		Int("response_bytes", len(respBz)).
		Msg("PASS abrupt-disconnect: the relayer served a fresh connection after a socket was dropped")
	return nil
}

// readSignedResponse reads one RelayResponse and requires it to be signed.
func readSignedResponse(conn *websocket.Conn) ([]byte, error) {
	_, respBz, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var resp servicetypes.RelayResponse
	if err := resp.Unmarshal(respBz); err != nil {
		return nil, fmt.Errorf("not a RelayResponse: %w", err)
	}
	if len(resp.Meta.SupplierOperatorSignature) == 0 {
		return nil, fmt.Errorf("the response came back UNSIGNED")
	}
	return respBz, nil
}

// flipAddress returns a DIFFERENT bech32-shaped address, by changing the last
// data character. It only has to differ from the original: the owner gate
// compares strings and runs before any signature or key check.
func flipAddress(addr string) string {
	if addr == "" {
		return "pokt1someoneelse"
	}
	last := addr[len(addr)-1]
	repl := byte('q')
	if last == 'q' {
		repl = 'p'
	}
	return addr[:len(addr)-1] + string(repl)
}
