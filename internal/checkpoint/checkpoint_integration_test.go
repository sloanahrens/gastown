//go:build integration

package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the package against real git repositories, pinning the git
// behaviour the unit tests' fakeRepo models: rename detection and -z output
// for additions, and the soft reset plus commit a squash performs.

func newRealRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git not available: %v", err)
	}
	dir := t.TempDir()
	realGitRun(t, dir, "init", "-q", "-b", "main")
	realGitRun(t, dir, "config", "user.email", "test@test.com")
	realGitRun(t, dir, "config", "user.name", "Test")
	realGitRun(t, dir, "config", "commit.gpgsign", "false")
	realGitCommit(t, dir, "README.md", "# Test\n", "initial commit")
	return dir
}

func realGitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func realGitCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	full := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	realGitRun(t, dir, "add", "--", file)
	realGitRun(t, dir, "commit", "-q", "--allow-empty-message", "-m", msg)
}

// TestIntegrationAddedThrowawayPaths_RealGit pins what git reports for the
// listing: a scratch copy git's rename detection would call a rename (R) is
// still an addition, a path with a space survives -z, and a modified tracked
// file with a throwaway name is not reported.
func TestIntegrationAddedThrowawayPaths_RealGit(t *testing.T) {
	t.Parallel()
	dir := newRealRepo(t)
	realGitCommit(t, dir, "helper.go", "package main\n\nfunc helper() {}\n", "tracked helper")
	realGitCommit(t, dir, "zz_fixture_test.go", "package main\n", "tracked fixture")
	base := realGitRun(t, dir, "rev-parse", "HEAD")

	realGitRun(t, dir, "checkout", "-q", "-b", "polecat/garnet/gt-trpzw")
	realGitRun(t, dir, "mv", "helper.go", "helper_tmp.go")
	realGitRun(t, dir, "commit", "-q", "-m", WIPCommitPrefix)
	realGitCommit(t, dir, "zz_fixture_test.go", "package main\n\n// real work\n", "real work")
	realGitCommit(t, dir, "scratch/notes copy.md", "scratch\n", WIPCommitPrefix)

	found, err := AddedThrowawayPaths(dir, base, "HEAD")
	if err != nil {
		t.Fatalf("AddedThrowawayPaths: %v", err)
	}
	want := []string{"helper_tmp.go", "scratch/notes copy.md"}
	if strings.Join(found, "|") != strings.Join(want, "|") {
		t.Fatalf("AddedThrowawayPaths() = %#v, want %#v", found, want)
	}
}

// TestIntegrationSquashAndInspect_RealGit squashes a mixed branch through a
// real soft reset and commit, and reads an empty tip subject back through
// real git log output.
func TestIntegrationSquashAndInspect_RealGit(t *testing.T) {
	t.Parallel()
	dir := newRealRepo(t)
	realGitRun(t, dir, "checkout", "-q", "-b", "feature")
	realGitCommit(t, dir, "a.go", "package a", "implement auth handler (gt-abc)")
	realGitCommit(t, dir, "b.go", "package b", WIPCommitPrefix)
	realGitCommit(t, dir, "c.go", "package c", "")

	tip, err := InspectAutoSaveTip(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if want := (AutoSaveTip{Subject: "", Ahead: 3}); tip != want {
		t.Errorf("InspectAutoSaveTip = %+v, want %+v", tip, want)
	}

	count, err := SquashAutoSaveCommits(dir, "main", "fallback title")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("squashed %d, want 1", count)
	}
	if got := realGitRun(t, dir, "log", "--format=%s", "main..HEAD"); got != "implement auth handler (gt-abc)" {
		t.Errorf("subjects after squash = %q, want the one real subject", got)
	}
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s missing after squash: %v", f, err)
		}
	}
	if status := realGitRun(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("worktree dirty after squash:\n%s", status)
	}
}
