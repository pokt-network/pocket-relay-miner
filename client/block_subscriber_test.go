//go:build test

package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestNewBlockSubscriber_ValidConfig tests creating a subscriber with valid configuration.
func TestNewBlockSubscriber_ValidConfig(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
		UseTLS:      false,
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err, "NewBlockSubscriber should succeed with valid config")
	require.NotNil(t, subscriber, "Subscriber should not be nil")
	require.NotNil(t, subscriber.cometClient, "CometBFT client should be initialized")
}

// TestNewBlockSubscriber_EmptyEndpoint tests that empty RPC endpoint is rejected.
func TestNewBlockSubscriber_EmptyEndpoint(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.Error(t, err, "NewBlockSubscriber should fail with empty endpoint")
	require.Nil(t, subscriber, "Subscriber should be nil on error")
	require.Contains(t, err.Error(), "RPC endpoint is required")
}

// TestNewBlockSubscriber_WithTLS tests creating a subscriber with TLS enabled.
func TestNewBlockSubscriber_WithTLS(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "https://localhost:26657",
		UseTLS:      true,
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err, "NewBlockSubscriber should succeed with TLS config")
	require.NotNil(t, subscriber, "Subscriber should not be nil")
}

// TestBlockSubscriber_LastBlock_NoBlock tests LastBlock when no block has been received.
func TestBlockSubscriber_LastBlock_NoBlock(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	ctx := context.Background()
	block := subscriber.LastBlock(ctx)
	require.NotNil(t, block, "LastBlock should return a block (possibly zero)")
}

// TestBlockSubscriber_GetChainVersion_Uninitialized tests getting chain version before initialization.
func TestBlockSubscriber_GetChainVersion_Uninitialized(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	version := subscriber.GetChainVersion()
	require.Nil(t, version, "Chain version should be nil before initialization")
}

// TestBlockSubscriber_Close_NotStarted tests closing a subscriber that was never started.
func TestBlockSubscriber_Close_NotStarted(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// Close should not panic
	subscriber.Close()
	require.True(t, subscriber.closed, "Subscriber should be marked as closed")
}

// TestBlockSubscriber_Close_MultipleCalls tests that multiple Close calls are safe.
func TestBlockSubscriber_Close_MultipleCalls(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// First close
	subscriber.Close()
	require.True(t, subscriber.closed)

	// Second close should not panic
	subscriber.Close()
	require.True(t, subscriber.closed)
}

// TestBlockSubscriber_CommittedBlocksSequence tests that CommittedBlocksSequence returns nil.
func TestBlockSubscriber_CommittedBlocksSequence(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	ctx := context.Background()
	observable := subscriber.CommittedBlocksSequence(ctx)
	require.Nil(t, observable, "CommittedBlocksSequence should return nil (not implemented)")
}

// TestBlockSubscriber_IncreaseBackoff tests exponential backoff logic.
func TestBlockSubscriber_IncreaseBackoff(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// Test exponential backoff
	delay1 := subscriber.increaseBackoff(1 * time.Second)
	require.Equal(t, 2*time.Second, delay1, "Backoff should double")

	delay2 := subscriber.increaseBackoff(delay1)
	require.Equal(t, 4*time.Second, delay2, "Backoff should continue doubling")

	// Test max backoff cap
	delay3 := subscriber.increaseBackoff(20 * time.Second)
	require.Equal(t, reconnectMaxDelay, delay3, "Backoff should cap at max delay")

	delay4 := subscriber.increaseBackoff(50 * time.Second)
	require.Equal(t, reconnectMaxDelay, delay4, "Backoff should stay at max delay")
}

// TestDefaultBlockSubscriberConfig tests default configuration.
func TestDefaultBlockSubscriberConfig(t *testing.T) {
	config := DefaultBlockSubscriberConfig()
	require.Empty(t, config.RPCEndpoint, "Default config should have empty endpoint")
	require.False(t, config.UseTLS, "Default config should have TLS disabled")
}

// TestSimpleBlock_Interface tests the simpleBlock implementation.
func TestSimpleBlock_Interface(t *testing.T) {
	block := &simpleBlock{
		height: 12345,
		hash:   []byte{0x01, 0x02, 0x03, 0x04},
	}

	require.Equal(t, int64(12345), block.Height(), "Height should match")
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, block.Hash(), "Hash should match")
}

// TestSimpleBlock_ZeroValues tests simpleBlock with zero values.
func TestSimpleBlock_ZeroValues(t *testing.T) {
	block := &simpleBlock{
		height: 0,
		hash:   nil,
	}

	require.Equal(t, int64(0), block.Height(), "Zero height should be valid")
	require.Nil(t, block.Hash(), "Nil hash should be valid")
}

// TestBlockSubscriber_LastBlock_WithStoredBlock tests LastBlock after storing a block.
func TestBlockSubscriber_LastBlock_WithStoredBlock(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// Manually store a block
	testBlock := &simpleBlock{
		height: 12345,
		hash:   []byte{0x01, 0x02, 0x03},
	}
	subscriber.lastBlock.Store(testBlock)

	ctx := context.Background()
	block := subscriber.LastBlock(ctx)
	require.NotNil(t, block)
	require.Equal(t, int64(12345), block.Height())
	require.Equal(t, []byte{0x01, 0x02, 0x03}, block.Hash())
}

// TestBlockSubscriber_Configuration tests various configuration options.
func TestBlockSubscriber_Configuration(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	tests := []struct {
		name        string
		config      BlockSubscriberConfig
		expectError bool
	}{
		{
			name: "valid HTTP endpoint",
			config: BlockSubscriberConfig{
				RPCEndpoint: "http://localhost:26657",
				UseTLS:      false,
			},
			expectError: false,
		},
		{
			name: "valid HTTPS endpoint",
			config: BlockSubscriberConfig{
				RPCEndpoint: "https://localhost:26657",
				UseTLS:      true,
			},
			expectError: false,
		},
		{
			name: "empty endpoint",
			config: BlockSubscriberConfig{
				RPCEndpoint: "",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subscriber, err := NewBlockSubscriber(logger, tt.config)
			if tt.expectError {
				require.Error(t, err)
				require.Nil(t, subscriber)
			} else {
				require.NoError(t, err)
				require.NotNil(t, subscriber)
			}
		})
	}
}

// TestBlockSubscriber_AtomicOperations tests atomic operations on lastBlock.
func TestBlockSubscriber_AtomicOperations(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// Store blocks atomically in goroutines
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func(height int64) {
			block := &simpleBlock{
				height: height,
				hash:   []byte{byte(height)},
			}
			subscriber.lastBlock.Store(block)
			done <- true
		}(int64(i))
	}

	// Wait for all goroutines
	for i := 0; i < 100; i++ {
		<-done
	}

	// Verify we have a valid block (any of the 100)
	ctx := context.Background()
	block := subscriber.LastBlock(ctx)
	require.NotNil(t, block)
	require.Greater(t, block.Height(), int64(-1))
}

// TestBlockSubscriber_ChainVersionAccess tests concurrent access to chain version.
func TestBlockSubscriber_ChainVersionAccess(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// Read chain version concurrently
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			version := subscriber.GetChainVersion()
			_ = version // May be nil, that's ok
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestBlockSubscriber_MultipleClose tests multiple Close calls don't cause issues.
func TestBlockSubscriber_MultipleClose(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// Close multiple times
	for i := 0; i < 5; i++ {
		subscriber.Close()
	}

	require.True(t, subscriber.closed)
}

// TestBlockSubscriber_CloseNotStarted tests closing before starting.
func TestBlockSubscriber_CloseNotStarted(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	config := BlockSubscriberConfig{
		RPCEndpoint: "http://localhost:26657",
	}

	subscriber, err := NewBlockSubscriber(logger, config)
	require.NoError(t, err)

	// Close without ever calling Start
	subscriber.Close()
	require.True(t, subscriber.closed)
}

// TestConstants tests package constants are defined correctly.
func TestConstants(t *testing.T) {
	require.Equal(t, "tm.event='NewBlockHeader'", newBlockHeaderQuery)
	require.Equal(t, "ha-block-subscriber", subscriptionClientID)
	require.Equal(t, 1*time.Second, reconnectBaseDelay)
	require.Equal(t, 30*time.Second, reconnectMaxDelay)
	require.Equal(t, 2, reconnectBackoffFactor)
}
