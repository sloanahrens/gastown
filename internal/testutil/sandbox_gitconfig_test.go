package testutil

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox gitconfig turns off git's fsync for every package under the
// hermetic harness: nothing a test writes outlives the test binary, and the
// flushes serialise parallel git-heavy tests (gt-22hdp.33).
func TestSandboxGitConfigDisablesFsync(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := writeSandboxGitConfig(home); err != nil {
		t.Fatalf("writeSandboxGitConfig: %v", err)
	}
	out, err := exec.Command("git", "config", "--file", filepath.Join(home, ".gitconfig"), "core.fsync").Output()
	if err != nil {
		t.Fatalf("git config core.fsync: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "none" {
		t.Fatalf("core.fsync = %q, want none", got)
	}
}
