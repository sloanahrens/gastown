//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Split out of plugin_script_test.go: the kill probe is syscall.Kill, which
// does not exist on Windows, and the scripts are sh. In the shared file it
// broke `go vet ./...` on the Windows runner.

// A timeout must kill the whole process tree, not just bash: the child the
// script backgrounds has to be gone too (gt-6t43 is the orphan shape).
func TestRunPluginScript_TimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	p := scriptPlugin(t, "slow", "sleep 60 &\necho $! > "+pidFile+"\nwait\n")
	// The deadline fires once the script has recorded its backgrounded child,
	// not after a fixed 500ms: under load bash had not reached the echo by
	// then, and the test failed on a missing pid file instead of testing the
	// kill.
	deadline := newGatedDeadline()
	go func() {
		for {
			// The whole line, not just the file: `echo $! > f` creates f
			// before it writes the pid.
			if raw, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(raw), "\n") {
				deadline.expire()
				return
			}
			select {
			case <-deadline.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	res := runPluginScript(deadline, p, "/town", time.Hour)
	if time.Since(start) > 30*time.Second {
		t.Fatalf("timeout did not bound the run: %s", time.Since(start))
	}
	if !res.timedOut || res.ok() {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if !strings.HasPrefix(res.status(), "timed out") {
		t.Errorf("status = %q", res.status())
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid not recorded: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		// Never signal a pid we did not read: kill(0, ...) is this test
		// binary's own process group.
		t.Fatalf("child pid file holds %q, not a pid", raw)
	}
	gone := time.Now().Add(3 * time.Second)
	for time.Now().Before(gone) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("backgrounded child %d survived the timeout: process group was not killed", pid)
}
