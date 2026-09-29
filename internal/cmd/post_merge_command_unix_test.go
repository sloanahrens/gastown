//go:build !windows

package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Split out of post_merge_command_test.go: the kill probe is syscall.Kill, which
// does not exist on Windows, and the scripts are sh. In the shared file it
// broke `go vet ./...` on the Windows runner.

func TestRunPostMergeCommand_TimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	var a postMergeAlerts
	esc := &a.escalations
	dir := t.TempDir()
	start := time.Now()
	runPostMergeCommand(a.wire(postMergeCommandParams{
		RigName: "gastown",
		WorkDir: dir,
		Timeout: time.Second,
		Output:  io.Discard,
		Command: `sleep 30 & echo $! > child.pid; wait`,
	}))
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runner took %v; the 1s timeout did not fire", elapsed)
	}
	if len(*esc) != 1 || !strings.Contains((*esc)[0], "timed out") {
		t.Fatalf("escalations = %v, want one 'timed out'", *esc)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("reading child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing child pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived the timeout: the process group was not killed", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
