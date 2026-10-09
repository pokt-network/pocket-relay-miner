//go:build test

package testredis

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// An exclusive Redis starts no reaper container: testcontainers' reaper is
// the lifecycle that failed a coverage run, and this harness runs without it.
func TestExclusive_StartsNoReaper(t *testing.T) {
	requireDocker(t)
	client := Exclusive(t)
	require.NoError(t, client.Ping(context.Background()).Err(), "control: the exclusive Redis answers")

	// This process's session, read off its own container: other test
	// processes, with their own sessions, may run a reaper of their own.
	session := dockerOut(t, "ps", "-a", "--filter", "label="+labelPID+"="+strconv.Itoa(os.Getpid()),
		"--format", `{{.Label "org.testcontainers.sessionId"}}`)
	require.NotEmpty(t, session, "control: the container carries this process's labels and session")
	require.Empty(t, dockerOut(t, "ps", "-a", "-q", "--filter", "label=org.testcontainers.ryuk=true",
		"--filter", "label=org.testcontainers.sessionId="+strings.Split(session, "\n")[0]),
		"no reaper container was created for this session")
}

// planted creates, without starting it, a container labelled as an exclusive
// Redis of host and pid started at start, and returns its id.
func planted(t *testing.T, host, pid, start string) string {
	t.Helper()
	id := dockerOut(t, "create",
		"--label", labelExclusive+"=true",
		"--label", labelHost+"="+host,
		"--label", labelPID+"="+pid,
		"--label", labelProcessStart+"="+start,
		exclusiveImage())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	return id
}

func exists(t *testing.T, id string) bool {
	t.Helper()
	return dockerOut(t, "ps", "-a", "-q", "--no-trunc", "--filter", "id="+id) != ""
}

// The next run removes what a killed test binary left: a container of this
// host whose process is gone, or whose pid now belongs to another process. It
// keeps one whose process still runs, and one of another host, whose pids
// are not ours to read.
func TestReapLeaked_RemovesOnlyTheDeadRunsOfThisHost(t *testing.T) {
	requireDocker(t)
	host, err := os.Hostname()
	require.NoError(t, err)
	self := os.Getpid()
	selfStart := processStart(self)
	require.NotEmpty(t, selfStart, "premise: ps reports this process's start time")

	gone := exec.Command("true")
	require.NoError(t, gone.Run())
	deadPID := strconv.Itoa(gone.ProcessState.Pid())
	require.Empty(t, processStart(gone.ProcessState.Pid()), "premise: that process has exited")

	dead := planted(t, host, deadPID, "Thu Jan  1 00:00:00 1970")
	reused := planted(t, host, strconv.Itoa(self), "Thu Jan  1 00:00:00 1970")
	alive := planted(t, host, strconv.Itoa(self), selfStart)
	foreign := planted(t, host+"-elsewhere", deadPID, "Thu Jan  1 00:00:00 1970")

	removeLeaked(processStart)

	require.False(t, exists(t, dead), "the container of a process that is gone is removed")
	require.False(t, exists(t, reused), "a pid now held by another process does not keep a dead run's container")
	require.True(t, exists(t, alive), "the container of a process that still runs is kept")
	require.True(t, exists(t, foreign), "another host's container is not this host's to judge")
}
