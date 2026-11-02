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
	// tree the sweep would test at the wrong commit. The fetch is the step that
	// refuses it, before any checkout runs.
	missing := strings.Repeat("0", 40)
	err := gitClone(repo, missing, filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("gitClone accepted a commit the repository does not hold")
	}
	if !strings.Contains(err.Error(), "git fetch "+missing) {
		t.Errorf("error = %v, want it to name the failed fetch", err)
	}
}

// TestIntegrationGitCloneChecksOutACommitOnlyARemoteTrackingRefHolds clones, with
// the real git, a repository whose only ref to the commit is refs/remotes/origin/main,
// as the rig's bare repository holds its main. cloneWith clones over git's transport,
// which copies only refs/heads/* and tags, so the commit is in no ref the clone
// takes: a checkout of the sha alone fails there, and cloneWith must fetch the sha
// by name to check it out. The baseline clones with cloneArgs, the clone production
// runs, so a clone shape that hides the failure turns the baseline red (gt-2jhtv).
func TestIntegrationGitCloneChecksOutACommitOnlyARemoteTrackingRefHolds(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGitIn(t, repo, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	runGitIn(t, repo, "add", "README.md")
	runGitIn(t, repo, "-c", "user.email=sweep-run@example.invalid", "-c", "user.name=sweep-run",
		"commit", "--quiet", "--no-gpg-sign", "-m", "tracked")
	want := strings.TrimSpace(runGitIn(t, repo, "rev-parse", "HEAD"))
	branch := strings.TrimSpace(runGitIn(t, repo, "symbolic-ref", "--short", "HEAD"))
	runGitIn(t, repo, "update-ref", "refs/remotes/origin/main", want)
	runGitIn(t, repo, "update-ref", "-d", "refs/heads/"+branch)

	// The bug on this fixture: the clone alone cannot check the commit out.
	plain := filepath.Join(t.TempDir(), "plain")
	if code, out := runGit("", cloneArgs(repo, plain)...); code != 0 {
		t.Fatalf("plain clone: exit %d: %s", code, out)
	}
	code, out := runGit(plain, "checkout", "--detach", want)
	if code == 0 {
		t.Fatal("checkout of the sha from the clone alone succeeded: the fixture does not reproduce the bug")
	}
	if !strings.Contains(out, "reference is not a tree") {
		t.Fatalf("checkout of the sha from the clone alone failed with %q, want 'reference is not a tree'", out)
	}

	work := filepath.Join(t.TempDir(), "work")
	if err := cloneWith(runGit, repo, want, work); err != nil {
		t.Fatalf("cloneWith of a commit only refs/remotes/origin/main holds: %v", err)
	}
	if got := strings.TrimSpace(runGitIn(t, work, "rev-parse", "HEAD")); got != want {
		t.Errorf("checked out %s, want %s", got, want)
	}
}

// TestIntegrationGitCloneReadsARepositoryWhoseQuotedPathNeedsTheShell clones, with
// the real git, a repository whose path holds a space and a single quote: the
// upload-pack command carrying the safe.directory exception goes through a
// shell, so a path the quoting got wrong would fail here and not in the unit
// tier's fake runner (gt-4t115). It does not reproduce a mount owned by another
// user, which needs a second uid.
func TestIntegrationGitCloneReadsARepositoryWhoseQuotedPathNeedsTheShell(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "it's a repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("make the repository directory: %v", err)
	}
	runGitIn(t, repo, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("quoted\n"), 0o644); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	runGitIn(t, repo, "add", "README.md")
	runGitIn(t, repo, "-c", "user.email=sweep-run@example.invalid", "-c", "user.name=sweep-run",
		"commit", "--quiet", "--no-gpg-sign", "-m", "quoted")
	want := strings.TrimSpace(runGitIn(t, repo, "rev-parse", "HEAD"))

	work := filepath.Join(t.TempDir(), "work")
	if err := gitClone(repo, want, work); err != nil {
		t.Fatalf("gitClone of a path with a space and a quote: %v", err)
	}
	if got := strings.TrimSpace(runGitIn(t, work, "rev-parse", "HEAD")); got != want {
		t.Errorf("checked out %s, want %s", got, want)
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
