//go:build integration

package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationHandoffGitState runs the handoff's workspace checks
// (warnHandoffGitStatusIn, collectGitStateIn) against real repositories.
// TestWarnHandoffGitStatus and TestCollectGitState cover their decisions over
// canned git state.
func TestIntegrationHandoffGitState(t *testing.T) {
	t.Parallel()
	git := func(t *testing.T, dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// repo makes a one-commit repository holding tracked.txt.
	repo := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		git(t, dir, "init")
		git(t, dir, "config", "user.email", "test@test.com")
		git(t, dir, "config", "user.name", "Test")
		write(t, filepath.Join(dir, "tracked.txt"), "original")
		git(t, dir, "add", ".")
		git(t, dir, "commit", "-m", "initial commit")
		return dir
	}
	warn := func(dir string) string {
		var buf bytes.Buffer
		warnHandoffGitStatusIn(&buf, dir)
		return buf.String()
	}

	t.Run("clean repo and outside a repo are silent", func(t *testing.T) {
		t.Parallel()
		if out := warn(repo(t)); out != "" {
			t.Errorf("clean repo: got %q", out)
		}
		if out := warn(t.TempDir()); out != "" {
			t.Errorf("outside a repo: got %q", out)
		}
		if state := collectGitStateIn(t.TempDir()); state != "" {
			t.Errorf("collectGitStateIn outside a repo: got %q", state)
		}
	})

	t.Run("untracked file warns, .beads-only does not", func(t *testing.T) {
		t.Parallel()
		dir := repo(t)
		write(t, filepath.Join(dir, ".beads", "somefile.db"), "db")
		if out := warn(dir); out != "" {
			t.Errorf(".beads-only changes: got %q", out)
		}
		write(t, filepath.Join(dir, "dirty.txt"), "x")
		if out := warn(dir); !strings.Contains(out, "uncommitted work") || !strings.Contains(out, "untracked") {
			t.Errorf("untracked file: got %q", out)
		}
	})

	t.Run("modified tracked file warns and shows in the state", func(t *testing.T) {
		t.Parallel()
		dir := repo(t)
		write(t, filepath.Join(dir, "tracked.txt"), "modified")
		if out := warn(dir); !strings.Contains(out, "uncommitted work") || !strings.Contains(out, "modified") {
			t.Errorf("modified file: got %q", out)
		}
		state := collectGitStateIn(dir)
		for _, want := range []string{"## Workspace State", "Modified", "initial commit"} {
			if !strings.Contains(state, want) {
				t.Errorf("state missing %q:\n%s", want, state)
			}
		}
	})
}
