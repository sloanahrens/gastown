//go:build integration && !windows

package pushcmd_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/pushcmd"
)

// sh builds the argv of a shell one-liner whose positional arguments are args.
func sh(script string, args ...string) []string {
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

func TestIntegrationRunGivesTheCommandTheMessageOnStdin(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "stdin")
	message := "Disk full\nrig gastown has 1G free\n"

	if err := pushcmd.Run(sh(`cat > "$1"`, out), message, pushcmd.Timeout); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != message {
		t.Errorf("command read %q, want %q", got, message)
	}
}

func TestIntegrationRunLimitsTheEnvironmentToPathAndHome(t *testing.T) {
	t.Setenv("GT_TEST_PUSHCMD_SECRET", "hunter2")
	out := filepath.Join(t.TempDir(), "env")

	if err := pushcmd.Run(sh(`env > "$1"`, out), "", pushcmd.Timeout); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// sh adds PWD, SHLVL and _ to whatever it was given.
	shellOwn := []string{"PWD", "SHLVL", "_", "OLDPWD"}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, _, _ := strings.Cut(line, "=")
		seen[key] = true
		if key != "PATH" && key != "HOME" && !slices.Contains(shellOwn, key) {
			t.Errorf("command saw %q in its environment", key)
		}
	}
	if !seen["PATH"] || !seen["HOME"] {
		t.Errorf("environment = %v, want PATH and HOME", seen)
	}
}

func TestIntegrationRunReportsAFailureWithoutTheArgv(t *testing.T) {
	t.Parallel()
	err := pushcmd.Run(sh(`exit 3`, "https://example.invalid/SECRET_TOPIC"), "x", pushcmd.Timeout)
	if err == nil {
		t.Fatal("Run succeeded, want the exit status")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error = %q, want the exit status", err)
	}
	if strings.Contains(err.Error(), "SECRET_TOPIC") {
		t.Errorf("error %q leaks the argv", err)
	}
}

func TestIntegrationRunReportsAMissingProgramWithoutItsPath(t *testing.T) {
	t.Parallel()
	err := pushcmd.Run([]string{"/nonexistent/SECRET_PROGRAM", "--token=SECRET_TOKEN"}, "x", pushcmd.Timeout)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Run = %v, want a program-not-found error", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error %q leaks the argv", err)
	}
}

func TestIntegrationRunRefusesAnEmptyArgv(t *testing.T) {
	t.Parallel()
	for _, argv := range [][]string{nil, {}, {""}} {
		if err := pushcmd.Run(argv, "x", pushcmd.Timeout); err == nil {
			t.Errorf("Run(%q) succeeded", argv)
		}
	}
}

// TestIntegrationRunKillsAHungCommandAndItsGroup: the deadline ends a command
// that never exits, and the process it backgrounded goes with it.
func TestIntegrationRunKillsAHungCommandAndItsGroup(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	start := time.Now()
	err := pushcmd.Run(sh(`sleep 60 & echo $! > "$1"; wait`, pidFile), "x", 500*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Run = %v, want a timeout", err)
	}
	if elapsed > 15*time.Second {
		t.Errorf("Run took %s, want it bounded near the timeout", elapsed)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("the command never recorded its grandchild: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil {
		t.Fatal(convErr)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestIntegrationRunDiscardsALotOfOutput: a command that fills stdout and
// stderr neither blocks on a full pipe nor makes Run buffer it.
func TestIntegrationRunDiscardsALotOfOutput(t *testing.T) {
	t.Parallel()
	script := `head -c 50000000 /dev/zero; head -c 50000000 /dev/zero >&2`

	if err := pushcmd.Run(sh(script), "x", pushcmd.Timeout); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestIntegrationRunIgnoresAnUnreadStdin: a command that never reads its stdin
// still succeeds on a message bigger than the pipe buffer.
func TestIntegrationRunIgnoresAnUnreadStdin(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 1<<20)

	if err := pushcmd.Run(sh(`exit 0`), big, pushcmd.Timeout); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
