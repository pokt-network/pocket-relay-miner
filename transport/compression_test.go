package transport

import (
	"bytes"
	"testing"
)

func TestCompressionConfig_Levels(t *testing.T) {
	testData := bytes.Repeat([]byte("test data for compression benchmarking "), 100)

	levels := []CompressionLevel{
		CompressionLevelFastest,
		CompressionLevelDefault,
		CompressionLevelBetter,
		CompressionLevelBest,
	}

	for _, level := range levels {
		t.Run(string(level), func(t *testing.T) {
			// Create a new compressor for this test
			c := &compressor{}
			err := c.init(CompressionConfig{
				Enabled: true,
				Level:   level,
				MinSize: 64,
			})
			if err != nil {
				t.Fatalf("init failed: %v", err)
			}

			// Compress
			compressed := c.compress(testData)
			if len(compressed) == 0 {
				t.Fatal("compressed data is empty")
			}

			// Decompress
			decompressed, err := c.decompress(compressed)
			if err != nil {
				t.Fatalf("decompress failed: %v", err)
			}

			// Verify roundtrip
			if !bytes.Equal(testData, decompressed) {
				t.Errorf("roundtrip failed: original len=%d, decompressed len=%d",
					len(testData), len(decompressed))
			}

			ratio := float64(len(compressed)) / float64(len(testData))
			t.Logf("Level %s: ratio=%.2f (original=%d, compressed=%d)",
				level, ratio, len(testData), len(compressed))
		})
	}
}

func TestCompressionConfig_Disabled(t *testing.T) {
	c := &compressor{}
	err := c.init(CompressionConfig{
		Enabled: false,
		Level:   CompressionLevelDefault,
		MinSize: 64,
	})
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	testData := bytes.Repeat([]byte("test data "), 100)

	// Should return data uncompressed
	result := c.compress(testData)
	if !bytes.Equal(testData, result) {
		t.Error("disabled compression should return data as-is")
	}
}

func TestCompressionConfig_MinSize(t *testing.T) {
	c := &compressor{}
	err := c.init(CompressionConfig{
		Enabled: true,
		Level:   CompressionLevelDefault,
		MinSize: 100, // Only compress data >= 100 bytes
	})
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Small data should not be compressed
	smallData := []byte("small")
	result := c.compress(smallData)
	if !bytes.Equal(smallData, result) {
		t.Error("data below MinSize should not be compressed")
	}

	// Large data should be compressed
	largeData := bytes.Repeat([]byte("large data "), 20)
	compressed := c.compress(largeData)
	if bytes.Equal(largeData, compressed) {
		t.Error("data above MinSize should be compressed")
	}
	if !IsCompressed(compressed) {
		t.Error("compressed data should have zstd magic header")
	}
}

func TestCompressionConfig_None(t *testing.T) {
	c := &compressor{}
	err := c.init(CompressionConfig{
		Enabled: true,
		Level:   CompressionLevelNone,
		MinSize: 64,
	})
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	testData := bytes.Repeat([]byte("test data "), 100)

	// Should return data uncompressed
	result := c.compress(testData)
	if !bytes.Equal(testData, result) {
		t.Error("CompressionLevelNone should return data as-is")
	}
}

func TestDecompressRelayBytes_BackwardsCompatibility(t *testing.T) {
	// Initialize with defaults
	err := InitCompression(DefaultCompressionConfig())
	if err != nil {
		t.Fatalf("InitCompression failed: %v", err)
	}

	// Uncompressed data (no zstd magic header) should be returned as-is
	uncompressed := []byte("this is not compressed data - no magic header")

	result, err := DecompressRelayBytes(uncompressed)
	if err != nil {
		t.Fatalf("DecompressRelayBytes() error = %v", err)
	}

	if !bytes.Equal(uncompressed, result) {
		t.Error("uncompressed data should be returned as-is for backwards compatibility")
	}
}

func TestCompressDecompressRelayBytes_Roundtrip(t *testing.T) {
	// Initialize with defaults
	err := InitCompression(DefaultCompressionConfig())
	if err != nil {
		t.Fatalf("InitCompression failed: %v", err)
	}

	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "empty data",
			data: []byte{},
		},
		{
			name: "small data below MinSize",
			data: []byte("hello"),
		},
		{
			name: "medium data",
			data: bytes.Repeat([]byte("test data for compression "), 100),
		},
		{
			name: "large data with repetition",
			data: bytes.Repeat([]byte("ABCDEFGHIJ"), 1000),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Compress
			compressed := CompressRelayBytes(tt.data)

			// Decompress
			decompressed, err := DecompressRelayBytes(compressed)
			if err != nil {
				t.Fatalf("DecompressRelayBytes() error = %v", err)
			}

			// Verify roundtrip
			if !bytes.Equal(tt.data, decompressed) {
				t.Errorf("roundtrip failed: original len=%d, decompressed len=%d",
					len(tt.data), len(decompressed))
			}
		})
	}
}

func TestIsCompressed(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected bool
	}{
		{
			name:     "empty data",
			data:     []byte{},
			expected: false,
		},
		{
			name:     "short data",
			data:     []byte{0x28, 0xB5, 0x2F},
			expected: false,
		},
		{
			name:     "zstd magic header",
			data:     []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x01, 0x02},
			expected: true,
		},
		{
			name:     "uncompressed data",
			data:     []byte("hello world"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCompressed(tt.data); got != tt.expected {
				t.Errorf("IsCompressed() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestDefaultCompressionConfig(t *testing.T) {
	cfg := DefaultCompressionConfig()

	if !cfg.Enabled {
		t.Error("default config should have compression enabled")
	}
	if cfg.Level != CompressionLevelBest {
		t.Errorf("default level should be %s (best ratio), got %s", CompressionLevelBest, cfg.Level)
	}
	if cfg.MinSize != 64 {
		t.Errorf("default MinSize should be 64, got %d", cfg.MinSize)
	}
}

func BenchmarkCompress_Levels(b *testing.B) {
	// Simulate typical relay data size (~1-2KB)
	data := bytes.Repeat([]byte("relay data content "), 50)

	levels := []CompressionLevel{
		CompressionLevelFastest,
		CompressionLevelDefault,
		CompressionLevelBetter,
		CompressionLevelBest,
	}

	for _, level := range levels {
		b.Run(string(level), func(b *testing.B) {
			c := &compressor{}
			_ = c.init(CompressionConfig{
				Enabled: true,
				Level:   level,
				MinSize: 64,
			})

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = c.compress(data)
			}
		})
	}
}

func BenchmarkDecompress(b *testing.B) {
	data := bytes.Repeat([]byte("relay data content "), 50)

	c := &compressor{}
	_ = c.init(DefaultCompressionConfig())
	compressed := c.compress(data)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = c.decompress(compressed)
	}
}
