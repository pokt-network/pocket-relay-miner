//go:build test

package testredis

import (
	"context"
	"os"
	"os/exec"
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
const (
	labelExclusive    = "prm.testredis.exclusive"
	labelHost         = "prm.testredis.host"
	labelPID          = "prm.testredis.pid"
	labelProcessStart = "prm.testredis.process_start"
)

// exclusiveLabels names this process: host, pid, and the pid's start time,
// so a reused pid does not keep a dead run's container alive.
func exclusiveLabels() map[string]string {
	host, _ := os.Hostname()
	pid := os.Getpid()
	return map[string]string{
		labelExclusive:    "true",
		labelHost:         host,
		labelPID:          strconv.Itoa(pid),
		labelProcessStart: processStart(pid),
	}
}

// processStart is pid's start time as ps prints it, or "" when no such
// process runs.
func processStart(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var reapOnce sync.Once

// reapLeaked removes, once per process, the exclusive containers of this host
// whose process is gone: a test binary killed before its cleanup ran. A
// container of another host is left alone (its pids are not ours to read),
// and so is one whose process still runs.
func reapLeaked() {
	reapOnce.Do(func() { removeLeaked(processStart) })
}

func removeLeaked(startOf func(pid int) string) {
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
		if err != nil || fields[2] == "" || startOf(pid) != fields[2] {
			leaked = append(leaked, fields[0])
		}
	}
	if len(leaked) > 0 {
		_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, leaked...)...).Run()
	}
}
