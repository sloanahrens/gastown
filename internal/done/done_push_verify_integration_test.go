//go:build integration

package done

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

// verifyPushLanded and landBranchPush against real bare remotes with a
// rejecting pre-receive hook and a rig bare repo sharing the ref store. The
// decisions are unit-tested against fakes in done_push_verify_test.go.

// writeRejectingRemote creates a bare repo whose pre-receive hook refuses every
// push, so a `git push` against it fails at the receiving end while the local
// commit remains perfectly intact — the shape of the gt-2wqt defect.
func writeRejectingRemote(t *testing.T, dir, name string) string {
	t.Helper()
	remote := filepath.Join(dir, name)
	testRunGit(t, dir, "init", "--bare", "--initial-branch", "main", remote)
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'rejected by test hook' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}
	return remote
}

// seedRepoWithCommit creates a repo on branch with one commit on top of main,
// and returns the git client plus HEAD.
func seedRepoWithCommit(t *testing.T, dir, remote, branch string) (*gitpkg.Git, string) {
	t.Helper()
	repo := filepath.Join(dir, "work")
	testRunGit(t, dir, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	writeRepoFile(t, repo, "README.md", "# seed\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "seed")
	testRunGit(t, repo, "remote", "add", "origin", remote)

	testRunGit(t, repo, "checkout", "-b", branch)
	writeRepoFile(t, repo, "fix.txt", "the fix\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "the fix")

	g := gitpkg.NewGit(repo)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	return g, strings.TrimSpace(head)
}

// initBareRigRepo provisions townRoot/<rig>/.repo.git the way a rig does, with
// the same origin as the worktree, and returns its path.
func initBareRigRepo(t *testing.T, townRoot, rig, remote string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, rig), 0o755); err != nil {
		t.Fatalf("mkdir rig dir: %v", err)
	}
	bareRepoPath := filepath.Join(townRoot, rig, ".repo.git")
	testRunGit(t, townRoot, "init", "--bare", bareRepoPath)
	testRunGit(t, bareRepoPath, "remote", "add", "origin", remote)
	return bareRepoPath
}

// TestVerifyPushLandedFailsClosedOnRejectingRemote is the gt-2wqt
// regression test: a push the remote refuses must never be reported as landed,
// and the MR the caller would have created from local HEAD is refused.
//
// The trap this pins down is the removed bare-repo fallback. Worktrees share
// the rig's ref store, so .repo.git/refs/heads/<branch> IS the polecat's local
// HEAD — comparing it to the commit being verified succeeds trivially and
// proved nothing about origin. The test keeps that ref at the unpushed commit
// on purpose: it must no longer satisfy the guard.
func TestIntegrationVerifyPushLandedFailsClosedOnRejectingRemote(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := writeRejectingRemote(t, tmp, "remote.git")
	const branch = "polecat/garnet/gt-2wqt"

	g, head := seedRepoWithCommit(t, tmp, remote, branch)

	// Sanity: the local commit exists, and the remote refuses to take it.
	if err := g.Push("origin", branch+":"+branch, false); err == nil {
		t.Fatal("expected the rejecting remote to refuse the push")
	}

	townRoot := filepath.Join(tmp, "town")
	const rig = "gastown"
	bareRepoPath := initBareRigRepo(t, townRoot, rig, remote)

	// Point the shared ref store at the unpushed commit. This is what a worktree
	// leaves behind, and what the old fallback accepted as proof of a push.
	testRunGit(t, bareRepoPath, "fetch", g.WorkDir(), "refs/heads/"+branch+":refs/heads/"+branch)
	bareGit := gitpkg.NewGitWithDir(bareRepoPath, bareRepoPath)
	bareTip, err := bareGit.Rev("refs/heads/" + branch)
	if err != nil {
		t.Fatalf("resolve bare repo branch ref: %v", err)
	}
	if strings.TrimSpace(bareTip) != head {
		t.Fatalf("test premise broken: bare ref = %q, want local HEAD %q", bareTip, head)
	}

	err = verifyPushLanded(g, townRoot, rig, branch, head)
	if err == nil {
		t.Fatal("verifyPushLanded = nil, want failure: origin never received the commit")
	}
	msg := err.Error()
	if !strings.Contains(msg, "verified_push_failed") {
		t.Errorf("error = %q, want the verified_push_failed prefix", msg)
	}
	if !strings.Contains(msg, head) {
		t.Errorf("error = %q, want it to name the local HEAD %s", msg, head)
	}
	if !strings.Contains(msg, "(missing on origin)") {
		t.Errorf("error = %q, want it to name origin's tip (missing)", msg)
	}
	if !strings.Contains(msg, "would declare ready to land") {
		t.Errorf("error = %q, want it to say what was not declared", msg)
	}
}

// TestVerifyPushLandedPassesWhenOriginHasCommit is the positive
// control: a real push must still verify, or the guard would block every
// submission.
func TestIntegrationVerifyPushLandedPassesWhenOriginHasCommit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)
	const branch = "polecat/garnet/gt-2wqt-ok"

	g, head := seedRepoWithCommit(t, tmp, remote, branch)
	if err := g.Push("origin", branch+":"+branch, false); err != nil {
		t.Fatalf("push: %v", err)
	}

	townRoot := filepath.Join(tmp, "town")
	if err := verifyPushLanded(g, townRoot, "gastown", branch, head); err != nil {
		t.Fatalf("verifyPushLanded = %v, want nil for a landed push", err)
	}
}

// TestVerifyPushLandedBareFallbackStillQueriesRemote covers the reason
// the fallback exists (GH #1348): when the worktree cannot reach the remote at
// all, the rig's bare repo — which shares the object database — re-runs the
// same remote assertion. The fallback must answer from origin, not from a local
// ref, so a bare repo whose branch ref points at an unpushed commit still fails.
func TestIntegrationVerifyPushLandedBareFallbackStillQueriesRemote(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)
	const branch = "polecat/garnet/gt-2wqt-fallback"

	g, head := seedRepoWithCommit(t, tmp, remote, branch)
	if err := g.Push("origin", branch+":"+branch, false); err != nil {
		t.Fatalf("push: %v", err)
	}

	townRoot := filepath.Join(tmp, "town")
	bareRepoPath := initBareRigRepo(t, townRoot, "gastown", remote)

	t.Run("worktree remote unusable, bare repo confirms the push", func(t *testing.T) {
		// Break the worktree's view of origin so its ls-remote cannot run.
		testRunGit(t, g.WorkDir(), "remote", "remove", "origin")
		defer testRunGit(t, g.WorkDir(), "remote", "add", "origin", remote)

		if err := verifyPushLanded(g, townRoot, "gastown", branch, head); err != nil {
			t.Fatalf("verifyPushLanded = %v, want nil via the bare repo fallback", err)
		}
	})

	t.Run("bare repo branch ref alone is not proof", func(t *testing.T) {
		// A commit origin does not have, present only in the shared ref store.
		writeRepoFile(t, g.WorkDir(), "unpushed.txt", "never pushed\n")
		testRunGit(t, g.WorkDir(), "add", ".")
		testRunGit(t, g.WorkDir(), "commit", "-m", "unpushed work")
		unpushed, err := g.Rev("HEAD")
		if err != nil {
			t.Fatalf("resolve HEAD: %v", err)
		}
		testRunGit(t, bareRepoPath, "fetch", g.WorkDir(), "refs/heads/"+branch+":refs/heads/"+branch)

		if err := verifyPushLanded(g, townRoot, "gastown", branch, strings.TrimSpace(unpushed)); err == nil {
			t.Fatal("verifyPushLanded = nil, want failure: the bare repo ref is not origin")
		}
	})
}

// TestLandBranchPushProceedsWhenOriginAlreadyHasTheCommit is the
// gt-0opm regression test: the first assertion failed the way a remote query
// races the ref update it asks about, while origin already holds the commit.
// The submission must go through rather than exit on the failed attempt.
func TestIntegrationLandBranchPushProceedsWhenOriginAlreadyHasTheCommit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)
	const branch = "polecat/emerald/gt-0opm"

	g, head := seedRepoWithCommit(t, tmp, remote, branch)
	if err := g.Push("origin", branch+":"+branch, false); err != nil {
		t.Fatalf("push: %v", err)
	}
	townRoot := filepath.Join(tmp, "town")

	// The first assertion fails the way a transient remote query does. The
	// second is the real one, against real origin.
	verifyCalls := 0
	verify := func() error {
		verifyCalls++
		if verifyCalls == 1 {
			return errors.New("verified_push_failed: unable to read origin/" + branch + ": connection reset")
		}
		return verifyPushLanded(g, townRoot, "gastown", branch, head)
	}
	pushCalls := 0
	attemptPush := func() error {
		pushCalls++
		return g.Push("origin", branch+":"+branch, false)
	}

	recovered, err := landBranchPush(attemptPush, verify, noSleep)
	if err != nil {
		t.Fatalf("landBranchPush = %v, want nil: origin has the commit", err)
	}
	if !recovered {
		t.Error("recovered = false, want true: the landing was only proven by the retry")
	}
	if pushCalls != 1 {
		t.Errorf("push attempts = %d, want 1", pushCalls)
	}
	if verifyCalls != 2 {
		t.Errorf("verify calls = %d, want 2", verifyCalls)
	}
}

// TestLandBranchPushRejectingRemoteStillFails is the gt-2wqt guard
// against the retry weakening it. A remote that refuses the push must never be
// talked into "landed" by a retry: the assertion is the same remote query, so
// re-running it cannot turn a refusal into a subtree the refinery would merge.
func TestIntegrationLandBranchPushRejectingRemoteStillFails(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := writeRejectingRemote(t, tmp, "remote.git")
	const branch = "polecat/emerald/gt-0opm-rejected"

	g, head := seedRepoWithCommit(t, tmp, remote, branch)
	townRoot := filepath.Join(tmp, "town")

	recovered, err := landBranchPush(
		func() error { return g.Push("origin", branch+":"+branch, false) },
		func() error { return verifyPushLanded(g, townRoot, "gastown", branch, head) },
		noSleep,
	)
	if err == nil {
		t.Fatal("landBranchPush = nil, want failure: the remote refuses the push")
	}
	if recovered {
		t.Error("recovered = true, want false: nothing landed")
	}
	if !strings.Contains(err.Error(), "would declare ready to land") {
		t.Errorf("error = %q, want it to say what was not declared", err)
	}
}
