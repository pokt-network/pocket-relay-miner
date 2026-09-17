// Package memlimit gives the process a memory limit when it starts, so the
// runtime's GC and the miner's memory bounds work against a number even when
// the operator did not set GOMEMLIMIT.
//
// The limit is GOMEMLIMIT when it is set. Otherwise it is the cgroup v2
// memory.max of the process's own cgroup less a margin, and without a numeric
// one the host's RAM less a margin. The host's RAM is only used when no cgroup
// limit is found: inside a container it is the node's, not the container's.
package memlimit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// Where a limit came from.
const (
	SourceEnv    = "env"
	SourceCgroup = "cgroup"
	SourceHost   = "host"
	// SourceNone is a limit that could not be found: the process runs without one.
	SourceNone = "none"
)

// maxMargin caps the margin a limit keeps below its base.
const maxMargin = 1 << 30

// Limit is the process's memory limit and where it came from.
type Limit struct {
	Source string
	// Base is what the limit was derived from: the cgroup's memory.max or the
	// host's RAM. It is the limit itself when the source is env.
	Base uint64
	// Bytes is the limit.
	Bytes uint64
	// Fallback says why the sources before Source were not used.
	Fallback string
}

// Margin is what a limit of limit bytes keeps free below it: an eighth, at
// most 1 GiB.
func Margin(limit uint64) uint64 {
	return min(maxMargin, limit/8)
}

// Sources are what Resolve reads.
type Sources struct {
	// Limit is the runtime's current limit, math.MaxInt64 when GOMEMLIMIT is
	// unset or "off".
	Limit func() uint64
	// Root is the file system /proc and /sys are read from.
	Root fs.FS
	// Totalram is the host's RAM in bytes.
	Totalram func() (uint64, error)
}

// Resolve finds the process's memory limit.
func Resolve(src Sources) Limit {
	if limit := src.Limit(); limit != math.MaxInt64 {
		return Limit{Source: SourceEnv, Base: limit, Bytes: limit}
	}
	fallback := "GOMEMLIMIT is unset or off"

	memoryMax, err := cgroupMemoryMax(src.Root)
	if err == nil {
		return Limit{Source: SourceCgroup, Base: memoryMax, Bytes: memoryMax - Margin(memoryMax), Fallback: fallback}
	}
	fallback += "; " + err.Error()

	ram, err := src.Totalram()
	if err == nil && ram == 0 {
		err = errors.New("reads 0")
	}
	if err != nil {
		return Limit{Source: SourceNone, Fallback: fallback + "; host RAM: " + err.Error()}
	}
	return Limit{Source: SourceHost, Base: ram, Bytes: ram - Margin(ram), Fallback: fallback}
}

// cgroupMemoryMax reads memory.max of the cgroup v2 /proc/self/cgroup names.
func cgroupMemoryMax(root fs.FS) (uint64, error) {
	membership, err := fs.ReadFile(root, "proc/self/cgroup")
	if err != nil {
		return 0, fmt.Errorf("no cgroup: %w", err)
	}
	group, found := "", false
	for _, line := range strings.Split(string(membership), "\n") {
		if group, found = strings.CutPrefix(line, "0::"); found {
			break
		}
	}
	if !found {
		return 0, errors.New("no cgroup v2 in /proc/self/cgroup")
	}
	file := path.Join("sys/fs/cgroup", group, "memory.max")
	raw, err := fs.ReadFile(root, file)
	if err != nil {
		return 0, fmt.Errorf("cgroup memory.max: %w", err)
	}
	value := string(bytes.TrimSpace(raw))
	if value == "max" {
		return 0, fmt.Errorf("/%s is max", file)
	}
	limit, err := strconv.ParseUint(value, 10, 64)
	if err != nil || limit == 0 {
		return 0, fmt.Errorf("/%s is not a limit: %q", file, value)
	}
	return limit, nil
}

// Apply resolves the process's memory limit, sets it on the runtime and logs it.
func Apply(logger logging.Logger) Limit {
	limit := Resolve(Sources{
		Limit:    func() uint64 { return uint64(debug.SetMemoryLimit(-1)) },
		Root:     os.DirFS("/"),
		Totalram: totalram,
	})
	if limit.Source == SourceNone {
		logger.Warn().
			Str("source", limit.Source).
			Str("fallback", limit.Fallback).
			Msg("no memory limit found: the process runs without one")
		return limit
	}
	if limit.Source != SourceEnv {
		debug.SetMemoryLimit(int64(min(limit.Bytes, math.MaxInt64)))
	}
	logger.Info().
		Str("source", limit.Source).
		Str("fallback", limit.Fallback).
		Uint64("base_bytes", limit.Base).
		Uint64("limit_bytes", limit.Bytes).
		Uint64("margin_bytes", Margin(limit.Bytes)).
		Msg("process memory limit set")
	return limit
}
