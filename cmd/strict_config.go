package cmd

import (
	"fmt"
	"strings"
)

// strictConfigError is the refusal --strict-config returns for keys the config
// carries but the binary does not understand. It lists every key itself: the
// warnings logged just before it can be lost, since an async logger may not
// have written them when the process exits.
func strictConfigError(subject string, unknown []string) error {
	return fmt.Errorf("--strict-config: refusing to start, %d key(s) %s does not understand:\n  %s",
		len(unknown), subject, strings.Join(unknown, "\n  "))
}
