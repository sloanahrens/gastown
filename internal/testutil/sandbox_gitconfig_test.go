package testutil

import (
	"os"
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

// The sandbox template keeps what code under test may write into (hooks/,
// info/exclude) and drops the stock template's inert sample hooks.
func TestSandboxGitConfigUsesMinimalTemplate(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := writeSandboxGitConfig(home); err != nil {
		t.Fatalf("writeSandboxGitConfig: %v", err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "init", "-q", repo)
	cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if info, err := os.Stat(filepath.Join(repo, ".git", "hooks")); err != nil || !info.IsDir() {
		t.Fatalf(".git/hooks missing from a repo made with the sandbox template: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "info", "exclude")); err != nil {
		t.Fatalf(".git/info/exclude missing from a repo made with the sandbox template: %v", err)
	}
	samples, err := filepath.Glob(filepath.Join(repo, ".git", "hooks", "*.sample"))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("sample hooks in a repo made with the sandbox template: %v", samples)
	}
}
