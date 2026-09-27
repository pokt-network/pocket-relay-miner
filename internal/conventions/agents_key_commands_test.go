package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AGENTS.md carries a copy of the key and stake commands from
// docs/SUPPLIER_KEYS.md, because a small model reads AGENTS.md (CLAUDE.md
// imports it) and often never opens SUPPLIER_KEYS.md; without the copy it
// wrote pocketd commands from memory, with flags that do not exist. A copy
// drifts, so every pocketd or curl line in AGENTS.md must appear verbatim,
// after trimming indentation and comments, in docs/SUPPLIER_KEYS.md.
func TestAgentsKeyCommandsMatchSupplierKeysDoc(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	normalize := func(line string) string {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "  #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		return line
	}
	doc := map[string]bool{}
	for _, l := range strings.Split(read("docs/SUPPLIER_KEYS.md"), "\n") {
		doc[normalize(l)] = true
	}
	checked := 0
	for _, l := range strings.Split(read("AGENTS.md"), "\n") {
		n := normalize(l)
		if !strings.HasPrefix(n, "pocketd ") && !strings.HasPrefix(n, "curl -sLO ") &&
			!strings.HasPrefix(n, "tar -xzf ") && !strings.HasPrefix(n, "sudo install ") &&
			!strings.HasPrefix(n, "--keyring-backend ") {
			continue
		}
		checked++
		if !doc[n] {
			t.Errorf("AGENTS.md command not found verbatim in docs/SUPPLIER_KEYS.md: %q", n)
		}
	}
	if checked < 7 {
		t.Fatalf("checked %d command lines in AGENTS.md, want at least 7: the copied block is gone or its format changed", checked)
	}
}
