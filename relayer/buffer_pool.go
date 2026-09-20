// Buffer Pool for High-Concurrency HTTP Response Processing
// ===========================================================
//
// This buffer pool manages reusable byte buffers to optimize memory allocation
// for high-throughput HTTP response processing. When handling thousands of
// concurrent HTTP requests with large response bodies (blockchain data can
// range from 10KB to 200MB+), naive allocation patterns create significant
// performance issues.
//
// Memory Allocation Patterns:
//   - Without pooling: Each request allocates new []byte buffers (can cause OOM)
//   - With pooling: Buffers are reused across requests via sync.Pool
//
// Benefits:
//   - Reduces garbage collection pressure
//   - Provides predictable memory usage under load
//   - Maintains consistent performance during traffic spikes
//   - Size limits prevent memory bloat from oversized responses
//
// The pool automatically grows buffer capacity as needed while preventing
// oversized buffers from being returned to avoid memory waste.
package relayer

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

const (
	// DefaultInitialBufferSize is the initial size of pooled buffers.
	// Start with 256KB buffers - can grow as needed for larger responses
	DefaultInitialBufferSize = 256 * 1024

	// DefaultMaxBufferSize is the maximum size allowed in the pool.
	// Buffers larger than this won't be returned to the pool to prevent memory bloat.
	// Set to 4MB as a reasonable upper bound for pooled buffers.
	DefaultMaxBufferSize = 4 * 1024 * 1024

	// DefaultMaxResponseSize is the absolute maximum response size we'll read.
	// This prevents reading unbounded data that could cause OOM.
	// Set to 200MB to handle large blockchain responses.
	DefaultMaxResponseSize = 200 * 1024 * 1024
)

// BufferPool manages reusable byte buffers to reduce GC pressure.
// Uses sync.Pool for efficient buffer recycling with size limits.
type BufferPool struct {
	pool          sync.Pool
	maxReaderSize int64
}

// NewBufferPool creates a new buffer pool with the specified max reader size.
// maxReaderSize limits how much data can be read from a single io.Reader.
func NewBufferPool(maxReaderSize int64) *BufferPool {
	if maxReaderSize <= 0 {
		maxReaderSize = DefaultMaxResponseSize
	}

	return &BufferPool{
		pool: sync.Pool{
			New: func() interface{} {
				return bytes.NewBuffer(make([]byte, 0, DefaultInitialBufferSize))
			},
		},
		maxReaderSize: maxReaderSize,
	}
}

// getBuffer retrieves a buffer from the pool.
func (bp *BufferPool) getBuffer() *bytes.Buffer {
	poolObj := bp.pool.Get()
	buf, ok := poolObj.(*bytes.Buffer)
	if !ok {
		// This should never happen since New always returns *bytes.Buffer,
		// but handle gracefully to prevent panics.
		return bytes.NewBuffer(make([]byte, 0, DefaultInitialBufferSize))
	}
	buf.Reset() // Always reset to ensure clean state
	return buf
}

// putBuffer returns a buffer to the pool.
// Buffers larger than DefaultMaxBufferSize are not returned to avoid memory bloat.
func (bp *BufferPool) putBuffer(buf *bytes.Buffer) {
	// Skip pooling oversized buffers to prevent memory bloat
	// This prevents 200MB responses from being pooled and consuming memory indefinitely
	if buf.Cap() > DefaultMaxBufferSize {
		return
	}
	bp.pool.Put(buf)
}

// ReadWithBufferLimit reads from an io.Reader using a pooled buffer, bounded by
// the caller's limit.
//
// The limit is a PARAMETER and not a property of the pool, because the two are
// unrelated: pooled buffers start at DefaultInitialBufferSize and grow as they
// read, and are dropped above DefaultMaxBufferSize -- none of which involves the
// limit. It lived on the pool only because there was one bound for everyone, and
// that is what made a per-service response limit look like it needed a pool per
// service. It does not: one pool, many limits.
//
// The returned byte slice is an independent copy safe for use after the function
// returns.
func (bp *BufferPool) ReadWithBufferLimit(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = bp.maxReaderSize
	}

	buf := bp.getBuffer()
	defer bp.putBuffer(buf)

	// Read one byte past the limit so "exactly at the limit" and "over it" can be
	// told apart. Reading exactly `limit` and calling that an overflow rejects a
	// body of precisely the configured size -- an operator who sets 10 MiB means
	// 10 MiB is allowed. This is the same idiom the request side already uses.
	limitedReader := io.LimitReader(r, limit+1)
	n, err := buf.ReadFrom(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read from reader: %w", err)
	}

	if n > limit {
		return nil, fmt.Errorf("response size exceeded limit of %d bytes", limit)
	}

	// Return independent copy to avoid data races
	// The buffer will be returned to the pool, so we need our own copy
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}
