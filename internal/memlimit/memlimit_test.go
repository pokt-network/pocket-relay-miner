//go:build test

package memlimit

import (
	"errors"
	"math"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

const gib = 1 << 30

func unset() uint64 { return math.MaxInt64 }

func hostRAM(n uint64) func() (uint64, error) {
	return func() (uint64, error) { return n, nil }
}

// podRoot is a container's view without a cgroup namespace: its cgroup's
// memory.max sits under the path /proc/self/cgroup names, and the cgroup root
// has none.
func podRoot(memoryMax string) fstest.MapFS {
	return fstest.MapFS{
		"proc/self/cgroup": {Data: []byte("0::/kubepods/burstable/pod1/ctr\n")},
		"sys/fs/cgroup/kubepods/burstable/pod1/ctr/memory.max": {Data: []byte(memoryMax)},
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		limit    func() uint64
		root     fstest.MapFS
		totalram func() (uint64, error)
		want     Limit
		fallback string
	}{
		{
			name:     "GOMEMLIMIT set is kept as it is, whatever the cgroup says",
			limit:    func() uint64 { return 7 * gib },
			root:     podRoot("8589934592\n"),
			totalram: hostRAM(31 * gib),
			want:     Limit{Source: SourceEnv, Base: 7 * gib, Bytes: 7 * gib},
		},
		{
			name:     "LINK cgroup-first: a numeric memory.max of the process's cgroup is used before the host's RAM",
			limit:    unset,
			root:     podRoot("8589934592\n"),
			totalram: hostRAM(31 * gib),
			want:     Limit{Source: SourceCgroup, Base: 8 * gib, Bytes: 7 * gib},
			fallback: "GOMEMLIMIT is unset or off",
		},
		{
			name:  "with a cgroup namespace the process's cgroup is the root",
			limit: unset,
			root: fstest.MapFS{
				"proc/self/cgroup":         {Data: []byte("0::/\n")},
				"sys/fs/cgroup/memory.max": {Data: []byte("8589934592\n")},
			},
			totalram: hostRAM(31 * gib),
			want:     Limit{Source: SourceCgroup, Base: 8 * gib, Bytes: 7 * gib},
			fallback: "GOMEMLIMIT is unset or off",
		},
		{
			name:     "a small container keeps an eighth: 2 GiB leaves 256 MiB",
			limit:    unset,
			root:     podRoot("2147483648"),
			totalram: hostRAM(31 * gib),
			want:     Limit{Source: SourceCgroup, Base: 2 * gib, Bytes: 2*gib - 256<<20},
			fallback: "GOMEMLIMIT is unset or off",
		},
		{
			name:     "memory.max max falls back to the host's RAM",
			limit:    unset,
			root:     podRoot("max\n"),
			totalram: hostRAM(31 * gib),
			want:     Limit{Source: SourceHost, Base: 31 * gib, Bytes: 30 * gib},
			fallback: "GOMEMLIMIT is unset or off; /sys/fs/cgroup/kubepods/burstable/pod1/ctr/memory.max is max",
		},
		{
			name:     "no memory.max for the process's cgroup falls back to the host's RAM",
			limit:    unset,
			root:     fstest.MapFS{"proc/self/cgroup": {Data: []byte("0::/init.scope\n")}},
			totalram: hostRAM(8 * gib),
			want:     Limit{Source: SourceHost, Base: 8 * gib, Bytes: 7 * gib},
			fallback: "GOMEMLIMIT is unset or off; cgroup memory.max: open sys/fs/cgroup/init.scope/memory.max: file does not exist",
		},
		{
			name:     "cgroup v1 only falls back to the host's RAM",
			limit:    unset,
			root:     fstest.MapFS{"proc/self/cgroup": {Data: []byte("12:memory:/docker/abc\n")}},
			totalram: hostRAM(8 * gib),
			want:     Limit{Source: SourceHost, Base: 8 * gib, Bytes: 7 * gib},
			fallback: "GOMEMLIMIT is unset or off; no cgroup v2 in /proc/self/cgroup",
		},
		{
			name:     "no /proc falls back to the host's RAM",
			limit:    unset,
			root:     fstest.MapFS{},
			totalram: hostRAM(8 * gib),
			want:     Limit{Source: SourceHost, Base: 8 * gib, Bytes: 7 * gib},
			fallback: "GOMEMLIMIT is unset or off; no cgroup: open proc/self/cgroup: file does not exist",
		},
		{
			name:     "an unparseable memory.max falls back to the host's RAM",
			limit:    unset,
			root:     podRoot("lots\n"),
			totalram: hostRAM(8 * gib),
			want:     Limit{Source: SourceHost, Base: 8 * gib, Bytes: 7 * gib},
			fallback: `GOMEMLIMIT is unset or off; /sys/fs/cgroup/kubepods/burstable/pod1/ctr/memory.max is not a limit: "lots"`,
		},
		{
			name:     "a zero memory.max is not a limit",
			limit:    unset,
			root:     podRoot("0\n"),
			totalram: hostRAM(8 * gib),
			want:     Limit{Source: SourceHost, Base: 8 * gib, Bytes: 7 * gib},
			fallback: `GOMEMLIMIT is unset or off; /sys/fs/cgroup/kubepods/burstable/pod1/ctr/memory.max is not a limit: "0"`,
		},
		{
			name:     "without the host's RAM there is no limit",
			limit:    unset,
			root:     podRoot("max"),
			totalram: func() (uint64, error) { return 0, errors.New("unsupported") },
			want:     Limit{Source: SourceNone},
			fallback: "GOMEMLIMIT is unset or off; /sys/fs/cgroup/kubepods/burstable/pod1/ctr/memory.max is max; host RAM: unsupported",
		},
		{
			name:     "a host RAM of zero is no limit, not a limit of zero",
			limit:    unset,
			root:     podRoot("max"),
			totalram: hostRAM(0),
			want:     Limit{Source: SourceNone},
			fallback: "GOMEMLIMIT is unset or off; /sys/fs/cgroup/kubepods/burstable/pod1/ctr/memory.max is max; host RAM: reads 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(Sources{Limit: tt.limit, Root: tt.root, Totalram: tt.totalram})
			want := tt.want
			want.Fallback = tt.fallback
			require.Equal(t, want, got)
		})
	}
}

func TestMargin(t *testing.T) {
	require.Equal(t, uint64(gib), Margin(8*gib), "an eighth of 8 GiB is the cap")
	require.Equal(t, uint64(gib), Margin(31*gib), "above 8 GiB the margin stays at 1 GiB")
	require.Equal(t, uint64(896<<20), Margin(7*gib))
	require.Equal(t, uint64(256<<20), Margin(2*gib))
	require.Zero(t, Margin(0))
}

func TestBrakeThresholds(t *testing.T) {
	closeAbove, reopenBelow := BrakeThresholds(7 * gib)
	require.Equal(t, uint64(7*gib-896<<20), closeAbove, "a margin under the limit")
	require.Equal(t, uint64(7*gib-896<<20-448<<20), reopenBelow, "LINK brake-hysteresis: a margin and a half under the limit")

	closeAbove, reopenBelow = BrakeThresholds(0)
	require.Zero(t, closeAbove)
	require.Zero(t, reopenBelow)
}
