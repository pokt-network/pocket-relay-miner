package logging

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRedactURL pins that only scheme://host survives: backend URLs carry
// operator topology, and provider API keys travel in the path and query.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "api key in path is stripped",
			raw:  "https://mainnet.infura.io/v3/SECRET-API-KEY",
			want: "https://mainnet.infura.io",
		},
		{
			name: "api key in query is stripped",
			raw:  "https://node.example.com/rpc?apikey=SECRET&x=1",
			want: "https://node.example.com",
		},
		{
			name: "userinfo is stripped",
			raw:  "https://user:password@node.example.com/rpc",
			want: "https://node.example.com",
		},
		{
			name: "port survives",
			raw:  "http://backend-1:8545/path",
			want: "http://backend-1:8545",
		},
		{
			name: "scheme-less host:port (gRPC style) survives whole",
			raw:  "backend-1:9090",
			want: "backend-1:9090",
		},
		{
			name: "websocket scheme survives",
			raw:  "ws://node:8546/ws?token=SECRET",
			want: "ws://node:8546",
		},
		{
			name: "empty input",
			raw:  "",
			want: "<redacted-invalid-url>",
		},
		{
			name: "unparseable input never leaks the raw value",
			raw:  "http://bad url with spaces/key-SECRET",
			want: "<redacted-invalid-url>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, RedactURL(tt.raw))
		})
	}
}
