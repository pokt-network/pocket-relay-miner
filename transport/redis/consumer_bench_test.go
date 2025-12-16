package redis

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// parseMessageJSON is the old JSON-based implementation (causes memory leak).
func parseMessageJSON(message redis.XMessage, streamName string) (*transport.StreamMessage, error) {
	data, ok := message.Values["data"]
	if !ok {
		return nil, fmt.Errorf("message missing 'data' field")
	}

	dataStr, ok := data.(string)
	if !ok {
		return nil, fmt.Errorf("message 'data' field is not a string")
	}

	var minedRelay transport.MinedRelayMessage
	// OLD: json.Unmarshal - caused 1.4GB literalStore accumulation with 1000 suppliers
	if err := json.Unmarshal([]byte(dataStr), &minedRelay); err != nil {
		return nil, fmt.Errorf("failed to unmarshal message: %w", err)
	}

	return &transport.StreamMessage{
		ID:         message.ID,
		StreamName: streamName,
		Message:    &minedRelay,
	}, nil
}

// parseMessageProtobuf is the new protobuf-based implementation (memory leak fix).
func parseMessageProtobuf(message redis.XMessage, streamName string) (*transport.StreamMessage, error) {
	data, ok := message.Values["data"]
	if !ok {
		return nil, fmt.Errorf("message missing 'data' field")
	}

	dataStr, ok := data.(string)
	if !ok {
		return nil, fmt.Errorf("message 'data' field is not a string")
	}

	var minedRelay transport.MinedRelayMessage
	// NEW: protobuf.Unmarshal - eliminates JSON literalStore memory overhead
	if err := minedRelay.Unmarshal([]byte(dataStr)); err != nil {
		return nil, fmt.Errorf("failed to unmarshal message: %w", err)
	}

	return &transport.StreamMessage{
		ID:         message.ID,
		StreamName: streamName,
		Message:    &minedRelay,
	}, nil
}

// createTestMessageJSON creates a sample Redis Stream message with JSON serialization.
func createTestMessageJSON(b *testing.B) redis.XMessage {
	sampleRelay := &transport.MinedRelayMessage{
		RelayHash:               make([]byte, 32),
		RelayBytes:              make([]byte, 1024),
		ComputeUnitsPerRelay:    100,
		SessionId:               "session_abc123_height_12345_app_pokt1abc_svc_develop",
		SessionEndHeight:        12345,
		SupplierOperatorAddress: "pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj",
		ServiceId:               "develop",
		PublishedAtUnixNano:     1234567890,
	}

	dataBytes, err := json.Marshal(sampleRelay)
	if err != nil {
		b.Fatalf("failed to marshal sample relay: %v", err)
	}
	dataStr := string(dataBytes)

	return redis.XMessage{
		ID: "1234567890-0",
		Values: map[string]interface{}{
			"data": dataStr,
		},
	}
}

// createTestMessageProtobuf creates a sample Redis Stream message with protobuf serialization.
func createTestMessageProtobuf(b *testing.B) redis.XMessage {
	sampleRelay := &transport.MinedRelayMessage{
		RelayHash:               make([]byte, 32),
		RelayBytes:              make([]byte, 1024),
		ComputeUnitsPerRelay:    100,
		SessionId:               "session_abc123_height_12345_app_pokt1abc_svc_develop",
		SessionEndHeight:        12345,
		SupplierOperatorAddress: "pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj",
		ServiceId:               "develop",
		PublishedAtUnixNano:     1234567890,
	}

	dataBytes, err := sampleRelay.Marshal()
	if err != nil {
		b.Fatalf("failed to marshal sample relay: %v", err)
	}
	dataStr := string(dataBytes)

	return redis.XMessage{
		ID: "1234567890-0",
		Values: map[string]interface{}{
			"data": dataStr,
		},
	}
}

// BenchmarkParseMessage_JSON benchmarks the old JSON implementation.
// This is the baseline that caused the memory leak (1.4GB literalStore accumulation).
//
// Expected results (1KB relay):
// - ~8000-9000 ns/op
// - ~3500-4000 B/op (JSON string allocations)
// - ~12 allocs/op
func BenchmarkParseMessage_JSON(b *testing.B) {
	message := createTestMessageJSON(b)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := parseMessageJSON(message, "ha:relays:pokt1abc:session123")
		if err != nil {
			b.Fatalf("parse failed: %v", err)
		}
	}
}

// BenchmarkParseMessage_Protobuf benchmarks the new protobuf implementation.
// This should eliminate literalStore memory overhead and reduce allocations.
//
// Expected results (1KB relay):
// - ~2000-4000 ns/op (2-3× faster than JSON)
// - ~1500-2000 B/op (3-5× smaller than JSON)
// - ~8-10 allocs/op (fewer than JSON)
//
// Key improvements:
// 1. Eliminates JSON literalStore accumulation (1.4GB → 0)
// 2. 2-3× faster parsing (protobuf binary vs JSON text)
// 3. Smaller message size (better Redis memory usage)
func BenchmarkParseMessage_Protobuf(b *testing.B) {
	message := createTestMessageProtobuf(b)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := parseMessageProtobuf(message, "ha:relays:pokt1abc:session123")
		if err != nil {
			b.Fatalf("parse failed: %v", err)
		}
	}
}

// BenchmarkParseMessage_LargeRelay benchmarks with a larger relay (10KB) to simulate
// complex requests like LLM streaming with large payloads.
func BenchmarkParseMessage_LargeRelay(b *testing.B) {
	// Create a large relay message (10KB)
	sampleRelay := &transport.MinedRelayMessage{
		RelayHash:               make([]byte, 32),
		RelayBytes:              make([]byte, 10*1024), // 10KB relay
		ComputeUnitsPerRelay:    1000,
		SessionId:               "session_abc123_height_12345_app_pokt1abc_svc_develop",
		SessionEndHeight:        12345,
		SupplierOperatorAddress: "pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj",
		ServiceId:               "develop",
		PublishedAtUnixNano:     1234567890,
	}

	// JSON message
	dataJSONBytes, err := json.Marshal(sampleRelay)
	if err != nil {
		b.Fatalf("failed to marshal JSON: %v", err)
	}
	messageJSON := redis.XMessage{
		ID: "1234567890-0",
		Values: map[string]interface{}{
			"data": string(dataJSONBytes),
		},
	}

	// Protobuf message
	dataProtoBytes, err := sampleRelay.Marshal()
	if err != nil {
		b.Fatalf("failed to marshal protobuf: %v", err)
	}
	messageProto := redis.XMessage{
		ID: "1234567890-0",
		Values: map[string]interface{}{
			"data": string(dataProtoBytes),
		},
	}

	b.Run("JSON", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := parseMessageJSON(messageJSON, "ha:relays:pokt1abc:session123")
			if err != nil {
				b.Fatalf("parse failed: %v", err)
			}
		}
	})

	b.Run("Protobuf", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, err := parseMessageProtobuf(messageProto, "ha:relays:pokt1abc:session123")
			if err != nil {
				b.Fatalf("parse failed: %v", err)
			}
		}
	})
}
