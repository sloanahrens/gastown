package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

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

// TestVerifyPushLandedBeforeMRFailsClosedOnRejectingRemote is the gt-2wqt
// regression test: a push the remote refuses must never be reported as landed,
// and the MR the caller would have created from local HEAD is refused.
//
// The trap this pins down is the removed bare-repo fallback. Worktrees share
// the rig's ref store, so .repo.git/refs/heads/<branch> IS the polecat's local
// HEAD — comparing it to the commit being verified succeeds trivially and
// proved nothing about origin. The test keeps that ref at the unpushed commit
// on purpose: it must no longer satisfy the guard.
func TestVerifyPushLandedBeforeMRFailsClosedOnRejectingRemote(t *testing.T) {
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

	err = verifyPushLandedBeforeMR(g, townRoot, rig, branch, head)
	if err == nil {
		t.Fatal("verifyPushLandedBeforeMR = nil, want failure: origin never received the commit")
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
	if !strings.Contains(msg, "no MR created") {
		t.Errorf("error = %q, want it to say no MR was created", msg)
	}
}

// TestVerifyPushLandedBeforeMRPassesWhenOriginHasCommit is the positive
// control: a real push must still verify, or the guard would block every
// submission.
func TestVerifyPushLandedBeforeMRPassesWhenOriginHasCommit(t *testing.T) {
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
	if err := verifyPushLandedBeforeMR(g, townRoot, "gastown", branch, head); err != nil {
		t.Fatalf("verifyPushLandedBeforeMR = %v, want nil for a landed push", err)
	}
}

// TestVerifyPushLandedBeforeMRBareFallbackStillQueriesRemote covers the reason
// the fallback exists (GH #1348): when the worktree cannot reach the remote at
// all, the rig's bare repo — which shares the object database — re-runs the
// same remote assertion. The fallback must answer from origin, not from a local
// ref, so a bare repo whose branch ref points at an unpushed commit still fails.
func TestVerifyPushLandedBeforeMRBareFallbackStillQueriesRemote(t *testing.T) {
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

		if err := verifyPushLandedBeforeMR(g, townRoot, "gastown", branch, head); err != nil {
			t.Fatalf("verifyPushLandedBeforeMR = %v, want nil via the bare repo fallback", err)
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

		if err := verifyPushLandedBeforeMR(g, townRoot, "gastown", branch, strings.TrimSpace(unpushed)); err == nil {
			t.Fatal("verifyPushLandedBeforeMR = nil, want failure: the bare repo ref is not origin")
		}
	})
}

// TestPushedCheckpointRequiresMatchingCommit is the other half of gt-2wqt: the
// push checkpoint must stop a retry from skipping the push after the branch has
// moved — the "gate fails → fix commit → re-run gt done" cycle that produced an
// MR declaring a commit origin never had.
func TestPushedCheckpointRequiresMatchingCommit(t *testing.T) {
	t.Parallel()
	const (
		branch = "polecat/garnet/gt-2wqt"
		shaA   = "8eb0cf6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB   = "c890451bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	tests := []struct {
		name  string
		value string
		sha   string
		want  bool
	}{
		{"same branch, same commit", pushedCheckpointValue(branch, shaA), shaA, true},
		{"same branch, new commit after the push", pushedCheckpointValue(branch, shaA), shaB, false},
		{"different branch", pushedCheckpointValue("polecat/amber/gt-uoqg", shaA), shaA, false},
		{"legacy branch-only checkpoint", branch, shaA, false},
		{"no checkpoint", "", shaA, false},
		{"unresolvable HEAD", pushedCheckpointValue(branch, shaA), "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pushedCheckpointMatches(tt.value, branch, tt.sha); got != tt.want {
				t.Errorf("pushedCheckpointMatches(%q, %q, %q) = %v, want %v",
					tt.value, branch, tt.sha, got, tt.want)
			}
		})
	}

	t.Run("branch names containing @ round-trip", func(t *testing.T) {
		const oddball = "polecat/user@host/gt-2wqt"
		value := pushedCheckpointValue(oddball, shaA)
		if got := pushedCheckpointBranch(value); got != oddball {
			t.Errorf("pushedCheckpointBranch(%q) = %q, want %q", value, got, oddball)
		}
		if !pushedCheckpointMatches(value, oddball, shaA) {
			t.Errorf("pushedCheckpointMatches(%q, %q, %q) = false, want true", value, oddball, shaA)
		}
	})

	t.Run("legacy values still resolve to their branch", func(t *testing.T) {
		if got := pushedCheckpointBranch(branch); got != branch {
			t.Errorf("pushedCheckpointBranch(%q) = %q, want %q", branch, got, branch)
		}
	})
}
