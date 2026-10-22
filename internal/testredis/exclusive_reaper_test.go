//go:build test

package testredis

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"runtime"
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
	selfStart, err := processStart(self)
	require.NoError(t, err)
	require.NotEmpty(t, selfStart, "premise: this process's start is readable")

	gone := exec.Command("true")
	require.NoError(t, gone.Run())
	deadPID := strconv.Itoa(gone.ProcessState.Pid())
	_, err = processStart(gone.ProcessState.Pid())
	require.ErrorIs(t, err, fs.ErrNotExist, "premise: that process has exited")

	dead := planted(t, host, deadPID, "another-boot:1")
	reused := planted(t, host, strconv.Itoa(self), "another-boot:"+selfStart[strings.LastIndexByte(selfStart, ':')+1:])
	alive := planted(t, host, strconv.Itoa(self), selfStart)
	foreign := planted(t, host+"-elsewhere", deadPID, "another-boot:1")

	removeLeaked(processStart)

	require.False(t, exists(t, dead), "the container of a process that is gone is removed")
	require.False(t, exists(t, reused), "a pid now held by another process does not keep a dead run's container")
	require.True(t, exists(t, alive), "the container of a process that still runs is kept")
	require.True(t, exists(t, foreign), "another host's container is not this host's to judge")
}

// A process that cannot be read is not a dead one: its container is kept.
func TestReapLeaked_KeepsAContainerWhoseProcessCannotBeRead(t *testing.T) {
	requireDocker(t)
	host, err := os.Hostname()
	require.NoError(t, err)
	unread := planted(t, host, strconv.Itoa(os.Getpid()), "another-boot:1")

	removeLeaked(func(int) (string, error) { return "", fs.ErrPermission })

	require.True(t, exists(t, unread), "a read that failed for another reason than a missing process removes nothing")
}

// A container under the selector of binaries before .v2 is theirs to judge:
// they stamp another start format, and judging it would remove a live run.
func TestReapLeaked_LeavesTheContainersOfOlderBinaries(t *testing.T) {
	requireDocker(t)
	host, err := os.Hostname()
	require.NoError(t, err)
	gone := exec.Command("true")
	require.NoError(t, gone.Run())
	old := dockerOut(t, "create",
		"--label", "prm.testredis.exclusive=true",
		"--label", labelHost+"="+host,
		"--label", labelPID+"="+strconv.Itoa(gone.ProcessState.Pid()),
		"--label", labelProcessStart+"=Thu Jan  1 00:00:00 1970",
		exclusiveImage())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", old).Run() })

	removeLeaked(processStart)

	require.True(t, exists(t, old), "an older binary's container is left to older binaries")
}

// processStart is the boot and the starttime the kernel recorded, read here
// by another method: a value derived from the wall clock, as ps prints it,
// is not this and moves between reads.
func TestProcessStart_IsTheBootAndTheKernelsStartTicks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the kernel's start ticks are read from /proc")
	}
	pid := os.Getpid()
	got, err := processStart(pid)
	require.NoError(t, err)

	stat, err := os.ReadFile("/proc/self/stat")
	require.NoError(t, err)
	m := regexp.MustCompile(`^\d+ \(.*\) (.*)$`).FindStringSubmatch(strings.TrimSpace(string(stat)))
	require.Len(t, m, 2, "control: the stat line parses")
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(boot))+":"+strings.Fields(m[1])[19], got)

	for i := 0; i < 50; i++ {
		again, err := processStart(pid)
		require.NoError(t, err)
		require.Equal(t, got, again, "the same process reads the same start")
	}
}

func TestParseStartTicks(t *testing.T) {
	tail := " S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 1913446 20 21"
	for name, line := range map[string]string{
		"plain":                   "42 (sleep)" + tail,
		"spaces in the name":      "42 (my cmd)" + tail,
		"parentheses in the name": "42 (x) 1 2 (y)" + tail,
		"empty name":              "42 ()" + tail,
	} {
		got, err := parseStartTicks(line)
		require.NoError(t, err, name)
		require.Equal(t, "1913446", got, name)
	}
	for name, line := range map[string]string{
		"empty":        "",
		"no name":      "42 sleep S 1 2 3",
		"too short":    "42 (sleep) S 1 2 3",
		"not a number": "42 (sleep) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 x 20",
	} {
		_, err := parseStartTicks(line)
		require.Error(t, err, name)
	}
}
