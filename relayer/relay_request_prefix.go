package relayer

import "google.golang.org/protobuf/encoding/protowire"

// serviceIDFromRelayRequestPrefix reads RelayRequest.meta(1).session_header(1).service_id(2)
// out of a possibly truncated RelayRequest, without unmarshalling it. It exists so a relay
// refused for its size is still attributed to its service: the body is cut at the limit, and
// the metadata sits at the front because Go serializes fields in field-number order. That order
// is observed behaviour, not a protobuf guarantee, so anything not found in the prefix, or
// malformed, returns "" and the caller reports it as unknown.
func serviceIDFromRelayRequestPrefix(body []byte) string {
	meta, ok := protoBytesField(body, 1)
	if !ok {
		return ""
	}
	header, ok := protoBytesField(meta, 1)
	if !ok {
		return ""
	}
	serviceID, ok := protoBytesField(header, 2)
	if !ok {
		return ""
	}
	return string(serviceID)
}

// protoBytesField returns the first complete length-delimited field `num` in b. A field cut by
// truncation, or any malformed tag or value before it, returns false.
func protoBytesField(b []byte, num protowire.Number) ([]byte, bool) {
	for len(b) > 0 {
		fieldNum, wireType, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false
		}
		b = b[n:]
		if fieldNum == num && wireType == protowire.BytesType {
			value, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, false
			}
			return value, true
		}
		m := protowire.ConsumeFieldValue(fieldNum, wireType, b)
		if m < 0 {
			return nil, false
		}
		b = b[m:]
	}
	return nil, false
}
