package relayer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
)

// streamDelimiter is a custom delimiter used to separate signed batches in streaming responses.
// This allows clients to parse individual signed relay response chunks from the stream.
// The delimiter is chosen to avoid collision with arbitrary payloads.
const streamDelimiter = "||POKT_STREAM||"

// Batch streaming configuration constants
const (
	// batchTimeThreshold is the maximum time to wait before flushing a batch (100ms)
	// This ensures low-latency streaming while reducing signing overhead.
	batchTimeThreshold = 100 * time.Millisecond

	// batchSizeThreshold is the maximum payload size before flushing a batch (100KB)
	// Prevents memory accumulation for high-throughput streams.
	batchSizeThreshold = 100 * 1024 // 100KB in bytes

	// batchChunksThreshold is the maximum number of chunks before flushing a batch
	// Limits the number of chunks per signed batch for predictable signing overhead.
	batchChunksThreshold = 100

	// maxScanTokenSize is the maximum size for a single stream chunk (256KB)
	// LLM responses can have large chunks, so we increase the default scanner buffer.
	maxScanTokenSize = 256 * 1024
)

// chunkBatch accumulates multiple chunks for batch signing and writing.
// This reduces signing overhead by signing multiple chunks together while
// maintaining low-latency streaming.
type chunkBatch struct {
	chunks      [][]byte  // individual chunk bodies
	totalSize   int64     // cumulative size of all chunks
	totalChunks int64     // number of chunks
	startTime   time.Time // when the batch was created
}

// newChunkBatch creates a new chunk batch with pre-allocated capacity.
func newChunkBatch() *chunkBatch {
	return &chunkBatch{
		chunks:      make([][]byte, 0, batchChunksThreshold),
		totalSize:   0,
		totalChunks: 0,
		startTime:   time.Now(),
	}
}

// shouldFlushBatch determines if the current batch should be flushed based on
// time, size, and chunk count thresholds.
//
// Flush conditions:
//   - forceFlush is true AND batch has at least one chunk
//   - Time threshold: 100ms has elapsed since batch creation
//   - Size threshold: batch size >= 100KB
//   - Chunk threshold: chunk count >= 100
func shouldFlushBatch(batch *chunkBatch, forceFlush bool) bool {
	if batch.totalChunks == 0 {
		return false
	}

	if forceFlush {
		return true
	}

	// Check time threshold
	if time.Since(batch.startTime) >= batchTimeThreshold {
		return true
	}

	// Check size threshold
	if batch.totalSize >= batchSizeThreshold {
		return true
	}

	// Check chunk count threshold
	if batch.totalChunks >= batchChunksThreshold {
		return true
	}

	return false
}

// reset resets the batch for reuse, preserving the allocated capacity.
func (b *chunkBatch) reset() {
	b.chunks = b.chunks[:0]
	b.totalSize = 0
	b.totalChunks = 0
	b.startTime = time.Now()
}

// add adds a chunk to the batch.
func (b *chunkBatch) add(chunk []byte) {
	b.chunks = append(b.chunks, chunk)
	b.totalSize += int64(len(chunk))
	b.totalChunks++
}

// combine returns all chunks concatenated into a single byte slice.
func (b *chunkBatch) combine() []byte {
	if b.totalChunks == 0 {
		return nil
	}

	combined := make([]byte, 0, b.totalSize)
	for _, chunk := range b.chunks {
		combined = append(combined, chunk...)
	}
	return combined
}

// StreamingResponseHandler handles streaming HTTP responses with batch-based signing.
// It signs chunks in batches and writes them to the client with the POKT stream delimiter.
type StreamingResponseHandler struct {
	logger         logging.Logger
	responseSigner *ResponseSigner
	relayRequest   *servicetypes.RelayRequest
	serviceTimeout time.Duration // Timeout from profile (for deadline extension)
}

// NewStreamingResponseHandler creates a new streaming response handler.
// serviceTimeout is used to extend write deadlines during long streaming responses.
func NewStreamingResponseHandler(
	logger logging.Logger,
	responseSigner *ResponseSigner,
	relayRequest *servicetypes.RelayRequest,
	serviceTimeout time.Duration,
) *StreamingResponseHandler {
	return &StreamingResponseHandler{
		logger:         logging.ForComponent(logger, logging.ComponentHTTPStream),
		responseSigner: responseSigner,
		relayRequest:   relayRequest,
		serviceTimeout: serviceTimeout,
	}
}

// HandleStreamingResponse processes a streaming HTTP response from a backend service.
//
// Streaming flow with batching:
//  1. Accumulate newline-delimited chunks in a batch buffer
//  2. Flush batch when any threshold is met (time/size/count)
//  3. When flushing:
//     - Combine all buffered chunks into single POKTHTTPResponse
//     - Sign batch once (not per-chunk)
//     - Write signed batch with delimiter to client
//     - Flush to ensure low-latency delivery
//  4. Final batch automatically flushes when stream ends
//
// This batching strategy reduces signing overhead and improves throughput while
// maintaining low-latency streaming (max 100ms delay) for SSE and NDJSON responses.
//
// Returns:
//   - Full response body (all chunks concatenated) for relay publishing
//   - Total response size across all batches (for metrics)
//   - Error if streaming fails
func (h *StreamingResponseHandler) HandleStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	w http.ResponseWriter,
) ([]byte, int64, error) {
	defer func() { _ = resp.Body.Close() }()

	// Copy headers to response
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	// Set connection close to prevent client reuse issues with streaming
	w.Header().Set("Connection", "close")
	w.WriteHeader(resp.StatusCode)

	// Check if writer supports flushing (required for streaming)
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		h.logger.Warn().Msg("ResponseWriter does not support flushing, streaming may have high latency")
	}

	// Buffer to collect full response for relay publishing
	var fullResponse bytes.Buffer

	// Initialize batch buffer
	batch := newChunkBatch()

	// Track total response size
	var totalResponseSize int64

	// Create scanner with increased buffer for large chunks
	scanner := bufio.NewScanner(resp.Body)
	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	// Create a ticker for time-based flushing
	ticker := time.NewTicker(batchTimeThreshold)
	defer ticker.Stop()

	// Process chunks from backend stream with batch-based signing
	for {
		select {
		case <-ctx.Done():
			// Context cancelled - flush any remaining batch before exiting
			if batch.totalChunks > 0 {
				size, err := h.flushBatch(batch, resp.StatusCode, w, flusher)
				if err != nil {
					h.logger.Error().Err(err).Msg("failed to flush final batch on context cancellation")
				}
				totalResponseSize += size
			}
			return fullResponse.Bytes(), totalResponseSize, ctx.Err()

		case <-ticker.C:
			// Time threshold reached - flush if batch has chunks
			if shouldFlushBatch(batch, false) {
				size, err := h.flushBatch(batch, resp.StatusCode, w, flusher)
				if err != nil {
					return fullResponse.Bytes(), totalResponseSize, fmt.Errorf("failed to flush batch on time threshold: %w", err)
				}
				totalResponseSize += size
				batch.reset()
			}

		default:
			// Try to read next chunk from scanner
			if !scanner.Scan() {
				// Stream ended - flush final batch if it has chunks
				if batch.totalChunks > 0 {
					size, err := h.flushBatch(batch, resp.StatusCode, w, flusher)
					if err != nil {
						return fullResponse.Bytes(), totalResponseSize, fmt.Errorf("failed to flush final batch: %w", err)
					}
					totalResponseSize += size
				}

				// Check for scanner errors
				if err := scanner.Err(); err != nil {
					return fullResponse.Bytes(), totalResponseSize, fmt.Errorf("stream scanning error: %w", err)
				}

				// Stream ended successfully
				return fullResponse.Bytes(), totalResponseSize, nil
			}

			// Restore newline stripped by scanner (needed for protocol compatibility)
			lineBz := scanner.Bytes()
			line := make([]byte, len(lineBz)+1)
			copy(line, lineBz)
			line[len(lineBz)] = '\n'

			// Collect for full response (for relay publishing)
			fullResponse.Write(line)

			// Add chunk to batch
			batch.add(line)

			// Check if batch thresholds exceeded
			if shouldFlushBatch(batch, false) {
				size, err := h.flushBatch(batch, resp.StatusCode, w, flusher)
				if err != nil {
					return fullResponse.Bytes(), totalResponseSize, fmt.Errorf("failed to flush batch on threshold: %w", err)
				}
				totalResponseSize += size

				// Reset timer and batch
				ticker.Reset(batchTimeThreshold)
				batch.reset()
			}
		}
	}
}

// flushBatch signs and writes the accumulated batch to the client.
// Also extends the write deadline to allow for continued streaming.
// Returns the size of the signed batch written.
func (h *StreamingResponseHandler) flushBatch(
	batch *chunkBatch,
	statusCode int,
	w http.ResponseWriter,
	flusher http.Flusher,
) (int64, error) {
	if batch.totalChunks == 0 {
		return 0, nil
	}

	// Extend write deadline for continued streaming
	// This prevents timeout for long-running streams (LLM responses, etc.)
	if h.serviceTimeout > 0 {
		rc := http.NewResponseController(w)
		// Extend deadline by the service timeout + buffer
		if err := rc.SetWriteDeadline(time.Now().Add(h.serviceTimeout + 30*time.Second)); err != nil {
			// Non-fatal: log debug and continue
			h.logger.Debug().Err(err).Msg("failed to extend write deadline (non-fatal)")
		}
	}

	// Combine all chunks into a single payload
	combinedPayload := batch.combine()

	// If we have a response signer, sign the batch
	if h.responseSigner != nil && h.relayRequest != nil {
		signedBatch, err := h.signBatch(combinedPayload, statusCode)
		if err != nil {
			return 0, fmt.Errorf("failed to sign batch: %w", err)
		}

		// Append POKT stream delimiter (allows client-side batch detection)
		signedBatch = append(signedBatch, []byte(streamDelimiter)...)

		// Write signed batch to client
		n, err := w.Write(signedBatch)
		if err != nil {
			return 0, fmt.Errorf("failed to write signed batch: %w", err)
		}

		// Flush to ensure data reaches client with low latency
		if flusher != nil {
			flusher.Flush()
		}

		streamingBatchesSigned.Inc()
		streamingBytesForwarded.Add(float64(n))

		return int64(n), nil
	}

	// No signer configured - forward raw chunks without signing
	// (This is for backward compatibility / testing scenarios)
	n, err := w.Write(combinedPayload)
	if err != nil {
		return 0, fmt.Errorf("failed to write raw batch: %w", err)
	}

	if flusher != nil {
		flusher.Flush()
	}

	streamingBytesForwarded.Add(float64(n))
	return int64(n), nil
}

// signBatch wraps the payload in a POKTHTTPResponse and signs it as a RelayResponse.
func (h *StreamingResponseHandler) signBatch(payload []byte, statusCode int) ([]byte, error) {
	// Use the existing BuildAndSignRelayResponseFromBody method
	// which handles POKT HTTP response wrapping and signing
	relayResponse, signedBatch, err := h.responseSigner.BuildAndSignRelayResponseFromBody(
		h.relayRequest,
		payload,
		nil, // No headers for streaming batches
		statusCode,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to sign relay response batch: %w", err)
	}

	h.logger.Debug().
		Int("payload_size", len(payload)).
		Int("signed_size", len(signedBatch)).
		Str("session_id", relayResponse.Meta.SessionHeader.SessionId).
		Msg("signed streaming batch")

	return signedBatch, nil
}

// ScanStreamEvents is a bufio.SplitFunc that splits streaming data by the POKT stream delimiter.
// This is used by clients to parse the signed relay response batches from the stream.
//
// Usage:
//
//	scanner := bufio.NewScanner(responseBody)
//	scanner.Split(ScanStreamEvents)
//	for scanner.Scan() {
//	    signedBatch := scanner.Bytes()
//	    // Unmarshal as RelayResponse...
//	}
func ScanStreamEvents(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// Look for the POKT_STREAM delimiter
	if i := strings.Index(string(data), streamDelimiter); i >= 0 {
		// Return chunk without the delimiter
		return i + len(streamDelimiter), data[0:i], nil
	}

	// If we're at EOF, return whatever we have
	if atEOF {
		return len(data), data, nil
	}

	// Request more data
	return 0, nil, nil
}

// handleStreamingResponseWithSigning is a helper method on ProxyServer that uses
// the StreamingResponseHandler for proper batch-based signing.
func (p *ProxyServer) handleStreamingResponseWithSigning(
	ctx context.Context,
	resp *http.Response,
	w http.ResponseWriter,
	relayRequest *servicetypes.RelayRequest,
	serviceID string,
	rpcType string,
) ([]byte, error) {
	// Get service timeout for deadline extension during streaming
	serviceTimeout := p.config.GetServiceTimeout(serviceID)

	handler := NewStreamingResponseHandler(
		p.logger,
		p.responseSigner,
		relayRequest,
		serviceTimeout,
	)

	fullBody, totalSize, err := handler.HandleStreamingResponse(ctx, resp, w)
	if err != nil {
		return fullBody, err
	}

	// Track metrics
	responseBodySize.WithLabelValues(serviceID, rpcType).Observe(float64(totalSize))

	return fullBody, nil
}

// closeBody safely closes an io.ReadCloser and logs any errors.
