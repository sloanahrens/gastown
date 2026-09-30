//go:build integration

package polecat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// Tests that need real processes: git's index, git's ref-name rules, a bd on
// PATH. The unit tier answers git through gitfake.

func seedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// gt-ycvx: the seat verdict this probe feeds is a cleanliness check, so a
// staged revert of a fix this checkout carries must read dirty — not as index
// skew, which would advertise the seat as reusable with the revert still in
// its index. gitfake has no index, so this runs on real git, through the
// exported probe.
func TestIntegrationProbeLiveGitStateStagedRevertReadsDirty(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "work")
	remote := filepath.Join(root, "remote.git")
	runGit(t, root, "init", "--initial-branch=main", dir)
	seedFile(t, filepath.Join(dir, "README.md"), "# Test\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, dir, "remote", "add", "origin", remote)
	runGit(t, dir, "push", "-u", "origin", "main")
	runGit(t, dir, "checkout", "-b", "polecat/jasper/om-x")
	seedFile(t, filepath.Join(dir, "README.md"), "# Test fixed\n")
	runGit(t, dir, "commit", "-am", "fix: confine the reviewer to a read-only allowlist")
	runGit(t, dir, "push", "-u", "origin", "polecat/jasper/om-x")
	runGit(t, dir, "checkout", "origin/main", "--", "README.md")

	got := ProbeLiveGitState(dir)
	if got.Source != GitStateSourceLive {
		t.Fatalf("Source = %q, want %q (reason %q)", got.Source, GitStateSourceLive, got.FailedReason)
	}
	if !got.Dirty {
		t.Fatalf("Dirty = false for a seat holding a staged security-fix revert (probe %+v)", got)
	}
}

// A generated branch name is a name git accepts for a branch.
func TestIntegrationFormatGeneratedBranchNameIsAValidGitRef(t *testing.T) {
	branch := FormatGeneratedBranchName("alpha", "gt-pin-bd-metadata", "mk123456")
	if err := exec.Command("git", "check-ref-format", "--branch", branch).Run(); err != nil {
		t.Fatalf("FormatGeneratedBranchName() = %q, rejected by git check-ref-format: %v", branch, err)
	}
}

// TestIntegrationNewManagerRunsTheBdOnPath is the wiring guard for NewManager:
// every unit test builds its manager with newTestManager and a fake bd, so
// this one puts a logging bd first on PATH and checks that a manager from
// NewManager reaches it. It changes PATH, so it is not parallel.
func TestIntegrationNewManagerRunsTheBdOnPath(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + logPath + "'\n" +
		"case \" $* \" in *\" show \"*) echo '[]' ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	m := NewManager(&rig.Rig{Name: "rig", Path: root}, nil, nil)
	if err := m.CheckDoltHealth(); err != nil {
		t.Fatalf("CheckDoltHealth: %v", err)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "show __health_check_nonexistent__") {
		t.Fatalf("the bd on PATH never saw the health check; its log:\n%s", logged)
	}
}
