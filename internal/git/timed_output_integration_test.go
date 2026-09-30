//go:build integration && unix

package git

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// holdOutputAlias returns `-c alias.hold=...` arguments for a git alias that
// prints want, leaves a background sleep holding git's stdout and stderr
// open, and exits 0. It is a local stand-in for a credential or remote helper
// that outlives git (gt-22hdp.50). The grandchild's pid is written to a file
// and the grandchild is killed by that pid when the test ends.
func holdOutputAlias(t *testing.T, want string) []string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			t.Errorf("reading grandchild pid: %v", err)
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 1 {
			t.Errorf("grandchild pid file holds %q", b)
			return
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	script := "!echo " + want + "; sleep 600 & echo $! > '" + pidFile + "'"
	return []string{"-c", "alias.hold=" + script, "hold"}
}

// A timed git command that exits 0 must return its output even when a
// grandchild still holds git's output pipes. Before gt-22hdp.50 the command's
// pipes were drained under exec's WaitDelay, so a helper that outlived git
// turned a successful ls-remote or push into "exec: WaitDelay expired before
// I/O complete" — the refinery then reported verified_push_failed for a push
// that had landed.
func TestIntegrationRunWithTimeoutSucceedsWhenGrandchildHoldsOutput(t *testing.T) {
	t.Parallel()
	g := NewGit(t.TempDir())
	out, err := g.runWithTimeout(remoteQueryTimeout, holdOutputAlias(t, "tip-sha")...)
	if err != nil {
		t.Fatalf("runWithTimeout: %v, want the successful git's output", err)
	}
	if out != "tip-sha" {
		t.Errorf("out = %q, want %q", out, "tip-sha")
	}
}

// The env-carrying timed path (git push with GT_* env) behaves the same way.
func TestIntegrationRunWithEnvAndTimeoutSucceedsWhenGrandchildHoldsOutput(t *testing.T) {
	t.Parallel()
	g := NewGit(t.TempDir())
	out, err := g.runWithEnvAndTimeout(holdOutputAlias(t, "pushed"), []string{"GT_TEST_MARKER=1"}, pushTimeout)
	if err != nil {
		t.Fatalf("runWithEnvAndTimeout: %v, want the successful git's output", err)
	}
	if out != "pushed" {
		t.Errorf("out = %q, want %q", out, "pushed")
	}
}
