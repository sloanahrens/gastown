//go:build integration

package polecat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
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

// gitEnv is the environment every git call in these tests runs under: no user
// or system config, and a fixed identity and clock, so a commit this fixture
// makes is the same commit on every run.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// gitOut is runGit for a call whose output the test needs, such as one naming
// the commit it just made or moved.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
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

// TestIntegrationRefreshLetsTheOfflineSeatCheckJudgeALandedSeatPreserved pins
// gt-fn9e6.55 at the seat-state level. A Forgejo landing merges the author's
// work on the remote and deletes the author's branch there, and nothing in the
// author's own worktree fetches the new tip: the seat's origin/<default> keeps
// the pre-landing commit, so the offline probe `gt polecat list` runs reads
// the landed work as unpushed — the false git-unpushed NEEDS_RECOVERY. The
// landing worker's refresh moves that one ref in the seat, and the same
// offline probe then judges the seat preserved, with the reader fetching
// nothing.
func TestIntegrationRefreshLetsTheOfflineSeatCheckJudgeALandedSeatPreserved(t *testing.T) {
	t.Parallel()
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

	// The seat: a polecat branch carrying the fix, pushed to origin.
	branch := "polecat/jasper/gt-fn9e6.55+muv"
	runGit(t, dir, "checkout", "-b", branch)
	seedFile(t, filepath.Join(dir, "fix.go"), "package fix\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "fix: the thing")
	runGit(t, dir, "push", "-u", "origin", branch)

	// The Forgejo landing: the work merged onto the remote's main as a
	// squash (the branch tip's tree under a different commit), the seat's
	// branch deleted on the remote. Nothing fetches here.
	tree := gitOut(t, remote, "rev-parse", branch+"^{tree}")
	base := gitOut(t, remote, "rev-parse", "main")
	landed := gitOut(t, remote, "commit-tree", tree, "-p", base, "-m", "land "+branch)
	runGit(t, remote, "update-ref", "refs/heads/main", landed)
	runGit(t, remote, "update-ref", "-d", "refs/heads/"+branch)

	// The seat as the landing leaves it: detached at its branch tip, the
	// branch and its remote-tracking ref gone, so origin/main is the only
	// evidence the offline probe has left.
	tip := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "--detach", tip)
	runGit(t, dir, "branch", "-D", branch)
	runGit(t, dir, "update-ref", "-d", "refs/remotes/origin/"+branch)

	before := ProbeLiveGitStateLocal(dir)
	if before.Source != GitStateSourceLive {
		t.Fatalf("Source = %q, want %q (reason %q)", before.Source, GitStateSourceLive, before.FailedReason)
	}
	if before.UnpushedCommits == 0 {
		t.Fatalf("probe = %+v; want the false git-unpushed flag: against the seat's stale origin/main the landed work looks local-only", before)
	}

	// The refresh the landing worker runs in the author's seat.
	if err := git.NewGit(dir).RefreshRemoteDefaultBranch("origin"); err != nil {
		t.Fatalf("RefreshRemoteDefaultBranch: %v", err)
	}

	after := ProbeLiveGitStateLocal(dir)
	if after.Source != GitStateSourceLive {
		t.Fatalf("Source after the refresh = %q, want %q (reason %q)", after.Source, GitStateSourceLive, after.FailedReason)
	}
	if after.UnpushedCommits != 0 {
		t.Fatalf("probe after the refresh = %+v; want the seat judged preserved with no live check", after)
	}
	if after.Dirty || after.StashCount != 0 {
		t.Fatalf("probe after the refresh = %+v; want the seat's work untouched", after)
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
	m := NewManager(&rig.Rig{Name: "rig", Path: root}, nil, nil, nil)
	if err := m.CheckDoltHealth(); err != nil {
		t.Fatalf("CheckDoltHealth: %v", err)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "show __health_check_nonexistent__") {
		t.Fatalf("the bd on PATH never saw the health check; its log:\n%s", logged)
	}
}
