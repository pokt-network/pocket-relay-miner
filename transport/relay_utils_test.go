package transport

import (
	"bytes"
	"testing"
)

func TestComputeRelayHash(t *testing.T) {
	data := []byte("test relay data")
	hash := ComputeRelayHash(data)

	// SHA256 produces 32 bytes
	if len(hash) != 32 {
		t.Errorf("expected 32 byte hash, got %d", len(hash))
	}

	// Same data should produce same hash
	hash2 := ComputeRelayHash(data)
	if !bytes.Equal(hash, hash2) {
		t.Error("same data should produce same hash")
	}

	// Different data should produce different hash
	hash3 := ComputeRelayHash([]byte("different data"))
	if bytes.Equal(hash, hash3) {
		t.Error("different data should produce different hash")
	}
}

func BenchmarkComputeRelayHash(b *testing.B) {
	data := bytes.Repeat([]byte("relay data content "), 50)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ComputeRelayHash(data)
	}
}
