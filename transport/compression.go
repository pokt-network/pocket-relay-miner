package transport

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// CompressionLevel defines the compression level to use.
// Maps to klauspost/compress/zstd levels.
type CompressionLevel string

const (
	// CompressionLevelNone disables compression entirely.
	CompressionLevelNone CompressionLevel = "none"

	// CompressionLevelFastest provides fastest compression (zstd level 1).
	// ~315 MB/s encode, lowest CPU usage, larger output.
	CompressionLevelFastest CompressionLevel = "fastest"

	// CompressionLevelDefault provides balanced compression (zstd level 3).
	// Good balance of speed and compression ratio. Recommended for most use cases.
	CompressionLevelDefault CompressionLevel = "default"

	// CompressionLevelBetter provides better compression (zstd level 7).
	// Slower but better compression ratio.
	CompressionLevelBetter CompressionLevel = "better"

	// CompressionLevelBest provides best compression (zstd level 11).
	// Slowest but best compression ratio. Use for archival or bandwidth-constrained scenarios.
	CompressionLevelBest CompressionLevel = "best"
)

// CompressionConfig contains configuration for relay compression.
type CompressionConfig struct {
	// Enabled enables compression of relay bytes.
	// Default: true
	Enabled bool `yaml:"enabled"`

	// Level is the compression level to use.
	// Options: "none", "fastest", "default", "better", "best"
	// Default: "default" (zstd level 3)
	Level CompressionLevel `yaml:"level"`

	// MinSize is the minimum size in bytes before compression is applied.
	// Data smaller than this is sent uncompressed (overhead not worth it).
	// Default: 64 bytes
	MinSize int `yaml:"min_size"`
}

// DefaultCompressionConfig returns the default compression configuration.
// Uses "best" compression level by default to maximize memory savings.
func DefaultCompressionConfig() CompressionConfig {
	return CompressionConfig{
		Enabled: true,
		Level:   CompressionLevelBest, // Best ratio - memory savings prioritized
		MinSize: 64,
	}
}

// compressor is a thread-safe compressor instance.
type compressor struct {
	encoder  *zstd.Encoder
	decoder  *zstd.Decoder
	config   CompressionConfig
	mu       sync.RWMutex
	initOnce sync.Once
	initErr  error
}

// globalCompressor is the default compressor used by the package-level functions.
var globalCompressor = &compressor{}

// InitCompression initializes the global compressor with the given configuration.
// Must be called before using CompressRelayBytes/DecompressRelayBytes.
// If not called, defaults are used.
func InitCompression(cfg CompressionConfig) error {
	return globalCompressor.init(cfg)
}

func (c *compressor) init(cfg CompressionConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.config = cfg

	// If disabled, no need to initialize encoder
	if !cfg.Enabled || cfg.Level == CompressionLevelNone {
		return nil
	}

	// Map level to zstd encoder level
	var level zstd.EncoderLevel
	switch cfg.Level {
	case CompressionLevelFastest:
		level = zstd.SpeedFastest
	case CompressionLevelDefault, "": // empty string defaults to default
		level = zstd.SpeedDefault
	case CompressionLevelBetter:
		level = zstd.SpeedBetterCompression
	case CompressionLevelBest:
		level = zstd.SpeedBestCompression
	default:
		return fmt.Errorf("unknown compression level: %s", cfg.Level)
	}

	var err error
	c.encoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
	if err != nil {
		return fmt.Errorf("failed to create zstd encoder: %w", err)
	}

	c.decoder, err = zstd.NewReader(nil)
	if err != nil {
		return fmt.Errorf("failed to create zstd decoder: %w", err)
	}

	return nil
}

func (c *compressor) compress(data []byte) []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check if compression is disabled
	if !c.config.Enabled || c.config.Level == CompressionLevelNone {
		return data
	}

	// Don't compress small data (overhead not worth it)
	if len(data) < c.config.MinSize {
		return data
	}

	// Initialize with defaults if not already initialized
	if c.encoder == nil {
		c.mu.RUnlock()
		c.initOnce.Do(func() {
			c.initErr = c.init(DefaultCompressionConfig())
		})
		c.mu.RLock()
		if c.initErr != nil || c.encoder == nil {
			return data // Fall back to uncompressed
		}
	}

	return c.encoder.EncodeAll(data, make([]byte, 0, len(data)))
}

func (c *compressor) decompress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}

	// Check for zstd magic number (0x28 0xB5 0x2F 0xFD)
	if !isZstdCompressed(data) {
		// Not compressed - return as-is (backwards compatibility)
		return data, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Initialize decoder if needed
	if c.decoder == nil {
		c.mu.RUnlock()
		c.initOnce.Do(func() {
			c.initErr = c.init(DefaultCompressionConfig())
		})
		c.mu.RLock()
		if c.initErr != nil || c.decoder == nil {
			return nil, fmt.Errorf("decompressor not initialized: %w", c.initErr)
		}
	}

	return c.decoder.DecodeAll(data, make([]byte, 0, len(data)*3))
}

// isZstdCompressed checks if data has the zstd magic number.
func isZstdCompressed(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == 0x28 &&
		data[1] == 0xB5 &&
		data[2] == 0x2F &&
		data[3] == 0xFD
}

// CompressRelayBytes compresses relay bytes using the configured compression.
// If compression is disabled or data is too small, returns data as-is.
func CompressRelayBytes(data []byte) []byte {
	return globalCompressor.compress(data)
}

// DecompressRelayBytes decompresses zstd-compressed relay bytes.
// If data is not compressed (legacy messages or compression disabled), returns as-is.
func DecompressRelayBytes(data []byte) ([]byte, error) {
	return globalCompressor.decompress(data)
}

// IsCompressed checks if the data is zstd compressed.
func IsCompressed(data []byte) bool {
	return isZstdCompressed(data)
}

// GetCompressionConfig returns the current compression configuration.
func GetCompressionConfig() CompressionConfig {
	globalCompressor.mu.RLock()
	defer globalCompressor.mu.RUnlock()
	return globalCompressor.config
}
