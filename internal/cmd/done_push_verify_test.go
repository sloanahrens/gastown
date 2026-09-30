package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestVerifyPushLandedFailsClosedOnRejectingRemote is the gt-2wqt
// regression test: a push the remote refuses must never be reported as landed,
// and the MR the caller would have created from local HEAD is refused.
//
// The trap this pins down is the removed bare-repo fallback. Worktrees share
// the rig's ref store, so .repo.git/refs/heads/<branch> IS the polecat's local
// HEAD — comparing it to the commit being verified succeeds trivially and
// proved nothing about origin. The test keeps that ref at the unpushed commit
// on purpose: it must no longer satisfy the guard.
func TestVerifyPushLandedFailsClosedOnRejectingRemote(t *testing.T) {
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
func TestVerifyPushLandedPassesWhenOriginHasCommit(t *testing.T) {
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
func TestVerifyPushLandedBareFallbackStillQueriesRemote(t *testing.T) {
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

// TestLandBranchPushStaysBoundedAndFailsClosed.
func noSleep(time.Duration) {}

// TestLandBranchPushProceedsWhenOriginAlreadyHasTheCommit is the
// gt-0opm regression test: the first assertion failed the way a remote query
// races the ref update it asks about, while origin already holds the commit.
// The submission must go through rather than exit on the failed attempt.
func TestLandBranchPushProceedsWhenOriginAlreadyHasTheCommit(t *testing.T) {
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

// TestLandBranchPushRetriesOnceWhenTheFirstSendFailed covers the other
// half of gt-0opm: the push command itself reported failure. One re-send is
// allowed, and it is what proves the landing — a re-query alone could not.
func TestLandBranchPushRetriesOnceWhenTheFirstSendFailed(t *testing.T) {
	t.Parallel()

	first := true
	verify := func() error {
		if first {
			first = false
			return errors.New("verified_push_failed: branch missing after push")
		}
		return nil
	}
	pushCalls := 0
	attemptPush := func() error {
		pushCalls++
		return nil // the re-send lands
	}

	recovered, err := landBranchPush(attemptPush, verify, noSleep)
	if err != nil {
		t.Fatalf("landBranchPush = %v, want nil", err)
	}
	if !recovered {
		t.Error("recovered = false, want true")
	}
	if pushCalls != 1 {
		t.Errorf("push attempts = %d, want 1", pushCalls)
	}
}

// TestLandBranchPushSkipsTheRetryWhenTheLandingIsAlreadyProven is the
// negative control: a push that proves out on its first assertion must not
// re-send anything or wait at all. Without this the retry would be a tax on
// every ordinary submission.
func TestLandBranchPushSkipsTheRetryWhenTheLandingIsAlreadyProven(t *testing.T) {
	t.Parallel()

	pushCalls, slept := 0, 0
	recovered, err := landBranchPush(
		func() error { pushCalls++; return nil },
		func() error { return nil },
		func(time.Duration) { slept++ },
	)
	if err != nil {
		t.Fatalf("landBranchPush = %v, want nil", err)
	}
	if recovered {
		t.Error("recovered = true, want false: nothing needed recovering")
	}
	if pushCalls != 0 {
		t.Errorf("push attempts = %d, want 0", pushCalls)
	}
	if slept != 0 {
		t.Errorf("sleeps = %d, want 0", slept)
	}
}

// TestLandBranchPushStaysBoundedAndFailsClosed pins the retry budget —
// this path runs when a submission has already failed, so every extra attempt
// holds a polecat's slot open — and that the outcome stays fail-closed, with no
// branch reported as landed that origin does not have.
func TestLandBranchPushStaysBoundedAndFailsClosed(t *testing.T) {
	t.Parallel()

	pushCalls, verifyCalls, slept := 0, 0, 0
	pushErr := errors.New("remote hung up")
	recovered, err := landBranchPush(
		func() error { pushCalls++; return pushErr },
		func() error { verifyCalls++; return errors.New("verified_push_failed: branch missing after push") },
		func(time.Duration) { slept++ },
	)
	if err == nil {
		t.Fatal("landBranchPush = nil, want failure: origin never received the commit")
	}
	if recovered {
		t.Error("recovered = true, want false")
	}
	if slept != len(pushLandingRetryDelays) {
		t.Errorf("sleeps = %d, want %d (the retry budget)", slept, len(pushLandingRetryDelays))
	}
	if pushCalls != len(pushLandingRetryDelays) {
		t.Errorf("push attempts = %d, want %d (the retry budget)", pushCalls, len(pushLandingRetryDelays))
	}
	if verifyCalls != len(pushLandingRetryDelays)+1 {
		t.Errorf("verify calls = %d, want %d", verifyCalls, len(pushLandingRetryDelays)+1)
	}
	// Both failures must survive into the message: the assertion says where
	// origin stands, the push error says why the re-send broke.
	if !strings.Contains(err.Error(), "verified_push_failed") {
		t.Errorf("error = %q, want the assertion failure", err)
	}
	if !strings.Contains(err.Error(), pushErr.Error()) {
		t.Errorf("error = %q, want the retry push error %q", err, pushErr)
	}
}

// TestLandBranchPushRejectingRemoteStillFails is the gt-2wqt guard
// against the retry weakening it. A remote that refuses the push must never be
// talked into "landed" by a retry: the assertion is the same remote query, so
// re-running it cannot turn a refusal into a subtree the refinery would merge.
func TestLandBranchPushRejectingRemoteStillFails(t *testing.T) {
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

// TestUnlandedPushMessageNamesTheHalfThatFailed: the push-command error and the
// origin assertion answer different questions, and a reader of a stranded
// submission needs both — unless there was no push error, in which case the
// assertion is the whole story.
func TestUnlandedPushMessageNamesTheHalfThatFailed(t *testing.T) {
	t.Parallel()
	const branch = "polecat/emerald/gt-0opm"
	verifyErr := errors.New("verified_push_failed: branch is not at the commit this merge request would declare")

	t.Run("assertion only", func(t *testing.T) {
		got := unlandedPushMessage(branch, nil, verifyErr)
		if got != verifyErr.Error() {
			t.Errorf("unlandedPushMessage = %q, want the assertion error verbatim", got)
		}
	})

	t.Run("push error and assertion", func(t *testing.T) {
		got := unlandedPushMessage(branch, errors.New("push timed out"), verifyErr)
		for _, want := range []string{"push timed out", verifyErr.Error()} {
			if !strings.Contains(got, want) {
				t.Errorf("unlandedPushMessage = %q, want it to contain %q", got, want)
			}
		}
		if !strings.Contains(got, branch) {
			t.Errorf("unlandedPushMessage = %q, want it to name the branch", got)
		}
	})
}
