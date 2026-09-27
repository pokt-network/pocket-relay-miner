package logging

import (
	"net/url"
	"strings"
)

// RedactURL reduces a raw URL to scheme://host for logging. Backend URLs are
// operator topology, and provider API keys travel in the path, query and
// userinfo — none of that may reach a log line. An unparseable input returns
// a placeholder rather than the raw value, so a malformed secret-bearing URL
// cannot leak through the error path.
func RedactURL(raw string) string {
	const invalid = "<redacted-invalid-url>"

	if strings.TrimSpace(raw) == "" {
		return invalid
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return invalid
	}

	// Scheme-less "host:port" (common for gRPC backends) parses as
	// scheme="host", opaque="port" with an empty Host — same gotcha
	// pool.NewBackendEndpoint handles. Re-parse to recover the host,
	// but return it without the borrowed scheme.
	if parsed.Host == "" && !strings.Contains(raw, "://") {
		reparsed, reErr := url.Parse("http://" + raw)
		if reErr != nil || reparsed.Host == "" {
			return invalid
		}
		return reparsed.Host
	}

	if parsed.Host == "" {
		return invalid
	}

	if parsed.Scheme != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return parsed.Host
}
