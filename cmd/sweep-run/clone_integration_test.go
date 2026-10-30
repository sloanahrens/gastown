//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationGitCloneChecksOutTheSHA is the only test that runs real git:
// the unit tier starts no subprocess, so it drives cloneWith through a fake
// runner, and this test pins the real command against a local repository with
// no network (gt-dibnm). It stays tiny on purpose.
func TestIntegrationGitCloneChecksOutTheSHA(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGitIn(t, repo, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("first\n"), 0o644); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	runGitIn(t, repo, "add", "README.md")
	runGitIn(t, repo, "-c", "user.email=sweep-run@example.invalid", "-c", "user.name=sweep-run",
		"commit", "--quiet", "--no-gpg-sign", "-m", "first")
	want := strings.TrimSpace(runGitIn(t, repo, "rev-parse", "HEAD"))

	work := filepath.Join(t.TempDir(), "work")
	if err := gitClone(repo, want, work); err != nil {
		t.Fatalf("gitClone: %v", err)
	}
	if got := strings.TrimSpace(runGitIn(t, work, "rev-parse", "HEAD")); got != want {
		t.Errorf("checked out %s, want %s", got, want)
	}
	// The checkout is detached, so the sweep tests the commit and not a branch
	// that could move under it.
	if got := strings.TrimSpace(runGitIn(t, work, "rev-parse", "--abbrev-ref", "HEAD")); got != "HEAD" {
		t.Errorf("HEAD is on %q, want a detached checkout", got)
	}

	// A commit the repository does not hold is the error the run records, not a
	// tree the sweep would test at the wrong commit.
	missing := strings.Repeat("0", 40)
	err := gitClone(repo, missing, filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("gitClone accepted a commit the repository does not hold")
	}
	if !strings.Contains(err.Error(), "git checkout "+missing) {
		t.Errorf("error = %v, want it to name the failed checkout", err)
	}
}

// runGitIn runs one git command in dir and returns its output, failing the test
// if git refuses.
func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
