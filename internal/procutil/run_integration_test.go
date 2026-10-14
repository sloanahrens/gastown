//go:build integration && !windows

package procutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests here prove that a child is reaped, which only the process table
// shows: a zombie still answers kill(pid, 0), and cmd.Wait's return is the
// thing under test. They look at the table through ps and never at the clock:
// each assertion holds however slowly the child starts or the host runs.

// eventually bounds a wait for something that must happen, never one that must
// not; it is long so that a loaded host passes.
const eventually = 60 * time.Second

// psChildren returns the pids whose parent is this test process, defunct ones
// included. The ps it runs is itself one of them, so its pid is left out.
func psChildren(t *testing.T) map[int]string {
	t.Helper()
	ps := exec.Command("ps", "-axo", "pid=,ppid=,stat=")
	out, err := ps.Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	children := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		if ppid == os.Getpid() && pid != ps.Process.Pid {
			children[pid] = f[2]
		}
	}
	return children
}

// assertNoNewChildren fails when a child of this process exists now that did
// not exist in before. A child that was waited on is gone from the table; one
// that was not shows up as stat Z.
func assertNoNewChildren(t *testing.T, before map[int]string) {
	t.Helper()
	for pid, stat := range psChildren(t) {
		if _, ok := before[pid]; !ok {
			t.Errorf("child %d (stat %q) outlived the call; a waited-on child leaves the process table", pid, stat)
		}
	}
}

// inProcessTable reports whether ps knows pid, in any state including Z.
func inProcessTable(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("ps -p %d: %v", pid, err)
	}
	return strings.TrimSpace(string(out)) != ""
}

func TestIntegrationOutput_ReturnsStdoutAndReapsTheChild(t *testing.T) {
	before := psChildren(t)

	out, err := Output(context.Background(), 0, "sh", "-c", "printf hello")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("stdout = %q, want %q", out, "hello")
	}
	assertNoNewChildren(t, before)
}

func TestIntegrationOutput_AFailedCommandKeepsItsStderrAndIsReaped(t *testing.T) {
	before := psChildren(t)

	_, err := Output(context.Background(), 0, "sh", "-c", "echo oops >&2; exit 3")
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v, want an *exec.ExitError", err)
	}
	if exit.ExitCode() != 3 || !strings.Contains(string(exit.Stderr), "oops") {
		t.Errorf("exit %d stderr %q, want exit 3 and stderr naming oops", exit.ExitCode(), exit.Stderr)
	}
	assertNoNewChildren(t, before)
}

func TestIntegrationOutput_ACommandThatNeverStartsLeavesNoChild(t *testing.T) {
	before := psChildren(t)

	if _, err := Output(context.Background(), 0, "procutil-no-such-tool"); err == nil {
		t.Fatal("Output of a missing tool returned no error")
	}
	assertNoNewChildren(t, before)
}

// TestIntegrationOutput_ATimeoutKillsAndReapsTheChild runs a child that would
// sleep for ten minutes under a short timeout. Whether the timeout lands
// before or after the child has started, the call must return the deadline
// error and leave nothing behind, so the assertion has no timing in it.
func TestIntegrationOutput_ATimeoutKillsAndReapsTheChild(t *testing.T) {
	before := psChildren(t)

	_, err := Output(context.Background(), 200*time.Millisecond, "sh", "-c", "exec sleep 600")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	assertNoNewChildren(t, before)
}

// TestIntegrationOutput_ACanceledContextReapsTheNamedChild is the same proof
// with the child's pid in hand, so the table check is on that process and not
// on "any child". The test cancels only after the child has written its pid,
// which it waits for up to eventually, so the child certainly existed.
func TestIntegrationOutput_ACanceledContextReapsTheNamedChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// mv publishes the pid whole; exec keeps the pid the shell wrote.
	script := `echo $$ > "$1.tmp" && mv "$1.tmp" "$1" && exec sleep 600`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := Output(ctx, 0, "sh", "-c", script, "sh", pidFile)
		result <- err
	}()

	var pid int
	deadline := time.Now().Add(eventually)
	for pid == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if pid == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the child never wrote its pid")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !inProcessTable(t, pid) {
		t.Fatalf("child %d is not running before the cancel; the test proves nothing", pid)
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(eventually):
		t.Fatal("Output did not return after the context was canceled")
	}
	if inProcessTable(t, pid) {
		t.Errorf("child %d is still in the process table after Output returned (defunct: not waited on)", pid)
	}
}

func TestIntegrationStart_ReapsAChildTheCallerDoesNotWaitOn(t *testing.T) {
	before := psChildren(t)

	done, err := Start("sh", "-c", "exit 0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Wait = %v, want nil", err)
		}
	case <-time.After(eventually):
		t.Fatal("the child was not waited on")
	}
	assertNoNewChildren(t, before)
}

func TestIntegrationStart_AMissingToolReturnsTheErrorAndNoChannel(t *testing.T) {
	done, err := Start("procutil-no-such-tool")
	if err == nil || done != nil {
		t.Errorf("Start = (%v, %v), want a nil channel and an error", done, err)
	}
}
