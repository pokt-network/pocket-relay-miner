//go:build test

package relayer

import (
	"bytes"
	"testing"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	"github.com/stretchr/testify/require"
)

// oversizedRelayRequest marshals a real RelayRequest with the gogoproto marshaller the gateway
// uses, and a payload far past any request limit.
func oversizedRelayRequest(t *testing.T, serviceID string) []byte {
	t.Helper()
	rr := &servicetypes.RelayRequest{
		Meta: servicetypes.RelayRequestMetadata{
			SessionHeader: &sessiontypes.SessionHeader{
				ApplicationAddress:      "pokt1app",
				ServiceId:               serviceID,
				SessionId:               "session-1",
				SessionStartBlockHeight: 100,
				SessionEndBlockHeight:   119,
			},
			SupplierOperatorAddress: "pokt1supplier",
		},
		Payload: bytes.Repeat([]byte("x"), 300<<10),
	}
	b, err := rr.Marshal()
	require.NoError(t, err)
	return b
}

// TestServiceIDIsReadFromTheTruncatedPrefix is the case the refusal sees: the body cut at the
// limit + 1 byte by the LimitReader, with the payload unread past that point.
func TestServiceIDIsReadFromTheTruncatedPrefix(t *testing.T) {
	body := oversizedRelayRequest(t, "eth")
	require.Equal(t, "eth", serviceIDFromRelayRequestPrefix(body[:256<<10+1]))
	require.Equal(t, "eth", serviceIDFromRelayRequestPrefix(body), "the whole body too")
}

func TestServiceIDIsEmptyWhenItCannotBeRead(t *testing.T) {
	body := oversizedRelayRequest(t, "eth")
	cases := map[string][]byte{
		"cut inside the metadata": body[:8],
		"not a protobuf":          bytes.Repeat([]byte{0xff}, 1024),
		"empty":                   nil,
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, "", serviceIDFromRelayRequestPrefix(b))
		})
	}
}
