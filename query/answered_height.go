package query

import (
	"errors"
	"strconv"

	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	"google.golang.org/grpc/metadata"
)

// answeredHeight reads the height a node answered from out of the response
// header it stamps on every query; 0 when the header is absent or unreadable.
func answeredHeight(md metadata.MD) int64 {
	values := md.Get(grpctypes.GRPCBlockHeightHeader)
	if len(values) != 1 {
		return 0
	}
	h, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || h < 0 {
		return 0
	}
	return h
}

// answeredError is a failed query with the height the node answered it from.
type answeredError struct {
	height int64
	err    error
}

func (e *answeredError) Error() string { return e.err.Error() }
func (e *answeredError) Unwrap() error { return e.err }

// AnsweredHeight is the height the node answered a failed query from, 0 when
// it is not known. A NotFound is the chain's verdict only from a height where
// the entity could no longer be on its way.
func AnsweredHeight(err error) int64 {
	var a *answeredError
	if errors.As(err, &a) {
		return a.height
	}
	return 0
}
