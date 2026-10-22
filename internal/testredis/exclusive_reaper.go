//go:build test

package testredis

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The exclusive Redis containers run without testcontainers' reaper (Ryuk).
// Its lifecycle across test processes is a race testcontainers v0.44.0 does
// not survive: a process found its reaper exited, terminated it, picked
// another reaper of the session that was exited too, and waited 60 s for it
// to start (a coverage run, 2026-10-08). Without a reaper there is nothing to
// lose or mis-select; t.Cleanup terminates each container, and the next run
// removes what a killed one left behind (reapLeaked).
//
// Set before testcontainers reads its configuration, which it does once, on
// the first container. An explicit value from the environment wins.
func init() {
	if _, set := os.LookupEnv("TESTCONTAINERS_RYUK_DISABLED"); !set {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// Labels every exclusive container carries from creation, so a later run can
// tell whether the process that made it is still alive.
//
// The selector is versioned: binaries before .v2 stamp the start time as ps
// prints it, and a binary of one format judging a container of the other would
// remove a live run's Redis. Each version sees only its own containers.
const (
	labelExclusive    = "prm.testredis.exclusive.v2"
	labelHost         = "prm.testredis.host"
	labelPID          = "prm.testredis.pid"
	labelProcessStart = "prm.testredis.process_start"
)

// exclusiveLabels names this process: host, pid, and the pid's start time,
// so a reused pid does not keep a dead run's container alive.
func exclusiveLabels() map[string]string {
	host, _ := os.Hostname()
	pid := os.Getpid()
	start, _ := processStart(pid)
	return map[string]string{
		labelExclusive:    "true",
		labelHost:         host,
		labelPID:          strconv.Itoa(pid),
		labelProcessStart: start,
	}
}

// processStart identifies pid's run: on Linux the boot and the clock ticks
// after boot at which it started, which the kernel stores and never
// recomputes. ps's start time is derived from the wall clock and moves by a
// second between reads of the same process (measured on WSL), which made a live
// run's container look dead. The error is fs.ErrNotExist when no such process
// runs; any other error means it could not be read, which is not a death.
func processStart(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		return readProcStart("/proc", pid)
	}
	// Elsewhere ps is all there is; no gate runs this path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	start := strings.TrimSpace(string(out))
	var exit *exec.ExitError
	if errors.As(err, &exit) && start == "" {
		return "", fs.ErrNotExist
	}
	if err != nil {
		return "", err
	}
	return start, nil
}

// readProcStart reads pid's start from a proc tree at root.
func readProcStart(root string, pid int) (string, error) {
	stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	ticks, err := parseStartTicks(string(stat))
	if err != nil {
		return "", err
	}
	boot, err := os.ReadFile(filepath.Join(root, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + ":" + ticks, nil
}

// parseStartTicks returns field 22 of a /proc/<pid>/stat line, starttime. The
// command name in field 2 may hold spaces and parentheses, so fields are
// counted after the last ')': field 3 is the first of them.
func parseStartTicks(stat string) (string, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", fmt.Errorf("stat line has no command name: %q", stat)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("stat line has %d fields after the command name, want at least 20", len(fields))
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", fmt.Errorf("stat starttime %q: %w", fields[19], err)
	}
	return fields[19], nil
}

var reapOnce sync.Once

// reapLeaked removes, once per process, the exclusive containers of this host
// whose process is gone: a test binary killed before its cleanup ran. A
// container of another host is left alone (its pids are not ours to read),
// and so is one whose process still runs.
func reapLeaked() {
	reapOnce.Do(func() { removeLeaked(processStart) })
}

func removeLeaked(startOf func(pid int) (string, error)) {
	host, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label="+labelExclusive+"=true",
		"--filter", "label="+labelHost+"="+host,
		"--format", `{{.ID}}|{{.Label "`+labelPID+`"}}|{{.Label "`+labelProcessStart+`"}}`).Output()
	if err != nil {
		return
	}
	var leaked []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, "|", 3)
		if len(fields) != 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || fields[2] == "" {
			leaked = append(leaked, fields[0])
			continue
		}
		start, err := startOf(pid)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			leaked = append(leaked, fields[0])
		case err != nil:
			// Could not look: a live run's Redis is not removed on doubt.
		case start != fields[2]:
			leaked = append(leaked, fields[0])
		}
	}
	if len(leaked) > 0 {
		_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, leaked...)...).Run()
	}
}
