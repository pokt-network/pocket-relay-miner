//go:build test

package testredis

import "testing"

// A disabled reaper is refused on a developer machine, where a killed test
// binary would leave its Redis container behind, and accepted on GitHub
// Actions, where the runner is discarded with its containers.
func TestRyukDisabledIsRefusedOnlyOffCI(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"reaper on, local", map[string]string{}, false},
		{"reaper off, local", map[string]string{"TESTCONTAINERS_RYUK_DISABLED": "true"}, true},
		{"reaper off, GitHub Actions", map[string]string{"TESTCONTAINERS_RYUK_DISABLED": "true", "GITHUB_ACTIONS": "true"}, false},
		{"reaper on, GitHub Actions", map[string]string{"GITHUB_ACTIONS": "true"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ryukDisabledOffCI(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Fatalf("ryukDisabledOffCI = %v, want %v", got, tc.want)
			}
		})
	}
}
