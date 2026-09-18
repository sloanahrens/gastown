package git

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRemoteRefsContainingReportsReachability(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitIn(t, "", "init", "--bare", remote)
	runGitIn(t, dir, "remote", "add", "origin", remote)
	runGitIn(t, dir, "push", "-u", "origin", branch)

	base := runGitIn(t, dir, "rev-parse", "HEAD")
	refs, err := g.RemoteRefsContaining(base)
	if err != nil {
		t.Fatalf("RemoteRefsContaining: %v", err)
	}
	if len(refs) != 1 || refs[0] != "origin/"+branch {
		t.Fatalf("refs = %v, want [origin/%s]", refs, branch)
	}

	// A commit on no remote branch is reachable from nothing.
	runGitIn(t, dir, "checkout", "-b", "unpublished")
	runGitIn(t, dir, "commit", "--allow-empty", "-m", "unpublished work")
	local := runGitIn(t, dir, "rev-parse", "HEAD")
	refs, err = g.RemoteRefsContaining(local)
	if err != nil {
		t.Fatalf("RemoteRefsContaining(local): %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want none: this commit is not on the remote", refs)
	}

	// Reachability, not tip membership: once the commit is pushed to a side
	// branch, that branch is reported even though the branch's own name was
	// never published.
	runGitIn(t, dir, "push", "origin", "unpublished:unpublished-side")
	refs, err = g.RemoteRefsContaining(local)
	if err != nil {
		t.Fatalf("RemoteRefsContaining after push: %v", err)
	}
	if len(refs) != 1 || refs[0] != "origin/unpublished-side" {
		t.Fatalf("refs = %v, want [origin/unpublished-side]", refs)
	}

	// An ancestor is reachable from every branch that contains it, so the base
	// commit is now reported for both branches — that is the query that makes
	// "already merged into main" detectable.
	refs, err = g.RemoteRefsContaining(base)
	if err != nil {
		t.Fatalf("RemoteRefsContaining(base) after push: %v", err)
	}
	if !slices.Contains(refs, "origin/"+branch) || !slices.Contains(refs, "origin/unpublished-side") {
		t.Fatalf("refs = %v, want both origin/%s and origin/unpublished-side", refs, branch)
	}
}

func TestRemoteRefsContainingIgnoresSymbolicHead(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitIn(t, "", "init", "--bare", remote)
	runGitIn(t, dir, "remote", "add", "origin", remote)
	runGitIn(t, dir, "push", "-u", "origin", branch)
	// `git branch -r` then prints "origin/HEAD -> origin/<branch>", which names
	// no branch of its own and must not be reported as a custody ref.
	runGitIn(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)

	head := runGitIn(t, dir, "rev-parse", "HEAD")
	refs, err := g.RemoteRefsContaining(head)
	if err != nil {
		t.Fatalf("RemoteRefsContaining: %v", err)
	}
	if len(refs) != 1 || refs[0] != "origin/"+branch {
		t.Fatalf("refs = %v, want [origin/%s] only", refs, branch)
	}
}

func TestRemoteRefsContainingRejectsEmptySHA(t *testing.T) {
	g := NewGit(initTestRepo(t))
	if _, err := g.RemoteRefsContaining("  "); err == nil {
		t.Fatal("RemoteRefsContaining(\"  \") must fail rather than report nothing reachable")
	}
}
