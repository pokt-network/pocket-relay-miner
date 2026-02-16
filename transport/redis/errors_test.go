package redis

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsOOMError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "exact Redis OOM message",
			err:  errors.New("OOM command not allowed when used memory > 'maxmemory'"),
			want: true,
		},
		{
			name: "wrapped OOM error",
			err:  fmt.Errorf("redis script failed: %w", errors.New("OOM command not allowed when used memory > 'maxmemory'")),
			want: true,
		},
		{
			name: "non-OOM error",
			err:  errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"),
			want: false,
		},
		{
			name: "connection error",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "OOM substring in different context",
			err:  errors.New("OOM"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOOMError(tt.err)
			if got != tt.want {
				t.Errorf("IsOOMError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
