//go:build test

package relayer

import (
	"testing"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
)

// TestOptimisticRetainedBytes_CountsThePayloadTheRequestCarries is the defect.
//
// The queue is bounded by bytes, and the number it was given left out the
// biggest thing a queued task holds after the two bodies: the parsed
// RelayRequest's own payload. That is not an alias into the request body --
// gogoproto's generated Unmarshal copies bytes fields
// (`m.Payload = append(m.Payload[:0], dAtA[...]...)`), so the request carries a
// second copy and the task holds both.
//
// Undercounting here is not a reporting nicety. The same number decides
// admission and, once the per-service cap lands, feeds the startup warning an
// operator sizes memory against: a cap of 128 MiB that admits 190 MiB is a
// promise the process cannot keep.
//
// LINK: retained-bytes-counts-the-payload
func TestOptimisticRetainedBytes_CountsThePayloadTheRequestCarries(t *testing.T) {
	const (
		reqLen     = 300
		respLen    = 5000
		payloadLen = 280
		sigLen     = 64
	)

	req := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{SessionId: "session1"},
			Signature:     make([]byte, sigLen),
		},
		Payload: make([]byte, payloadLen),
	}

	got := optimisticRetainedBytes(make([]byte, reqLen), make([]byte, respLen), req)

	require.Equal(t, int64(reqLen+respLen+payloadLen+sigLen), got,
		"LINK retained-bytes-counts-the-payload: the request's own payload (%d B) and signature (%d B) are held by the queued task just as the bodies are; counting only the %d B of bodies bounds the queue by less than it holds",
		payloadLen, sigLen, reqLen+respLen)

	require.Greater(t, got, int64(reqLen+respLen),
		"LINK retained-bytes-counts-the-payload: the total must exceed the two bodies alone")
}

// TestOptimisticRetainedBytes_SurvivesANilRequest pins the guard. A relay whose
// body never parsed still reaches this path, and a panic on admission would turn
// a malformed request into an outage.
//
// LINK: retained-bytes-nil-request
func TestOptimisticRetainedBytes_SurvivesANilRequest(t *testing.T) {
	got := optimisticRetainedBytes(make([]byte, 10), make([]byte, 20), nil)
	require.Equal(t, int64(30), got,
		"LINK retained-bytes-nil-request: with no parsed request there is nothing beyond the bodies to count")
}
