//go:build integration

package gtwrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdingProbe is the argv of a probe whose shell runs script. The script must
// write the pid of the background child it starts to the file named by $1, so the
// test can kill that child: the child keeps stdout open, and a child left running
// would outlive the test.
func holdingProbe(t *testing.T, script string) []string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Cleanup(func() { killPIDFile(t, pidFile) })
	return []string{"sh", "-c", script, "sh", pidFile}
}

// killPIDFile kills the child whose pid the script wrote to pidFile. A child that
// already exited, or a script that never wrote the file, has nothing to kill.
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Errorf("pid file %s holds %q, not a pid", pidFile, data)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Errorf("kill child %d: %v", pid, err)
	}
}

// TestIntegrationCaptureHoldsTheProbeBoundWhenAChildKeepsStdoutOpen runs a real process, which
// the unit tier refuses. The shell leaves a child holding stdout open and then
// runs past the probe's deadline. Capture must return near the deadline and
// refuse, not wait for the child to exit.
func TestIntegrationCaptureHoldsTheProbeBoundWhenAChildKeepsStdoutOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	argv := holdingProbe(t, `sleep 30 & echo $! > "$1"; exec sleep 30`)
	type result struct {
		exit int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, exit, err := OSShell{}.Capture(ctx, argv)
		done <- result{exit, err}
	}()
	select {
	case got := <-done:
		// The deadline kills the shell with SIGKILL, which a shell reports as 128+9.
		if got.exit != 137 {
			t.Fatalf("Capture = exit %d, err %v; want 137, the status of a probe the deadline killed", got.exit, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Capture did not return: a child holding stdout open kept the probe past its bound")
	}
}

// TestIntegrationCaptureRefusesAProbeThatExitedWhileAChildHeldStdout covers a probe
// whose shell exits 0 while its child still holds stdout past the deadline. The
// output was cut short by the bound, so the probe must still read as a refusal.
func TestIntegrationCaptureRefusesAProbeThatExitedWhileAChildHeldStdout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	argv := holdingProbe(t, `sleep 30 & echo $! > "$1"; echo ready`)
	out, exit, err := OSShell{}.Capture(ctx, argv)
	if exit == 0 {
		t.Fatalf("Capture = %q, exit 0, err %v; want a refusal, since the bound cut the output short", out, err)
	}
}

// TestIntegrationCaptureReturnsAHealthyProbeWhileAChildHoldsStdoutBriefly covers a
// probe that exits 0 while a child still holds stdout for less than the bound. The
// probe must return all of its output and exit 0, not be refused by the pipe.
func TestIntegrationCaptureReturnsAHealthyProbeWhileAChildHoldsStdoutBriefly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	argv := holdingProbe(t, `sleep 2 & echo $! > "$1"; echo ready`)
	out, exit, err := OSShell{}.Capture(ctx, argv)
	if err != nil || exit != 0 || out != "ready\n" {
		t.Fatalf("Capture = %q, exit %d, err %v; want \"ready\\n\", 0, nil", out, exit, err)
	}
}

// TestIntegrationOSShellAttachStreamsPipesTheOutput: a real process started by
// AttachStreams writes to writers that are not the wrapper's own files, so exec
// copies its output through a pipe, which is the path the kubernetes target's
// rewriter takes. Both streams arrive as the process wrote them, and its exit
// status comes back.
func TestIntegrationOSShellAttachStreamsPipesTheOutput(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	argv := []string{"sh", "-c", `printf 'out %s\n' "$1"; printf 'err %s\n' "$2" >&2; exit 3`, "sh", "one", "two"}
	exit, err := OSShell{}.AttachStreams(argv, nil, nil, &stdout, &stderr)
	if err != nil || exit != 3 {
		t.Fatalf("AttachStreams = %d, %v; want exit 3 and no error", exit, err)
	}
	if got, want := stdout.String(), "out one\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "err two\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}
