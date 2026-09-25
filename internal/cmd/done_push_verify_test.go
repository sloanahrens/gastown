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

// countingLandedProbe answers classifyResumedPush's target question and counts
// how often it was asked. A checkpoint that matches nothing must never reach it:
// where the work sits says nothing about a branch this run's checkpoint does not
// describe.
type countingLandedProbe struct {
	landed bool
	calls  int
}

func (p *countingLandedProbe) CommitLandedOnTarget(remote, target, commit string) bool {
	p.calls++
	return p.landed
}

// TestClassifyResumedPushAsksTheTargetOnlyForThisCommit is the gt-mik3
// regression test at the decision level. A checkpoint naming this branch and
// commit is an earlier run's verified push, and the merge that lands work
// deletes its branch from origin — so the re-run must ask the target whether the
// work is already on it, instead of pushing the branch back and reading the
// missing ref as an unlanded push.
func TestClassifyResumedPushAsksTheTargetOnlyForThisCommit(t *testing.T) {
	t.Parallel()
	const (
		branch = "polecat/granite/gt-mik3"
		shaA   = "8eb0cf6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB   = "c890451bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	tests := []struct {
		name       string
		checkpoint string
		sha        string
		landed     bool
		want       resumedWork
		wantProbed bool
	}{
		{
			name:       "this commit, on the target",
			checkpoint: pushedCheckpointValue(branch, shaA), sha: shaA, landed: true,
			want: resumedWorkLanded, wantProbed: true,
		},
		{
			name:       "this commit, not on the target",
			checkpoint: pushedCheckpointValue(branch, shaA), sha: shaA, landed: false,
			want: resumedWorkPushed, wantProbed: true,
		},
		{
			name:       "another commit on the same branch",
			checkpoint: pushedCheckpointValue(branch, shaA), sha: shaB, landed: true,
			want: resumedWorkStale, wantProbed: false,
		},
		{
			name:       "another branch",
			checkpoint: pushedCheckpointValue("polecat/amber/gt-uoqg", shaA), sha: shaA, landed: true,
			want: resumedWorkStale, wantProbed: false,
		},
		{
			name:       "legacy branch-only checkpoint",
			checkpoint: branch, sha: shaA, landed: true,
			want: resumedWorkStale, wantProbed: false,
		},
		{
			name:       "no checkpoint",
			checkpoint: "", sha: shaA, landed: true,
			want: resumedWorkStale, wantProbed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &countingLandedProbe{landed: tt.landed}
			if got := classifyResumedPush(probe, "origin", "main", tt.checkpoint, branch, tt.sha); got != tt.want {
				t.Errorf("classifyResumedPush(%q, %q, %q) = %v, want %v",
					tt.checkpoint, branch, tt.sha, got, tt.want)
			}
			if probed := probe.calls > 0; probed != tt.wantProbed {
				t.Errorf("target probed = %v (%d calls), want %v", probed, probe.calls, tt.wantProbed)
			}
		})
	}
}

// TestClassifyResumedPushSeesAMergedAndDeletedBranch pins the gt-mik3 shape
// against real refs: the merge lands the commit on main and the post-merge
// cleanup deletes the branch from origin, which is why a re-run cannot take the
// branch's absence for an unlanded push. The unmerged control is the same
// missing-ref shape and must still be classified as pushed — the branch being
// gone is never on its own evidence that the work landed.
func TestClassifyResumedPushSeesAMergedAndDeletedBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", remote)

	const branch = "polecat/granite/gt-mik3"
	g, head := seedRepoWithCommit(t, tmp, remote, branch)
	repo := filepath.Join(tmp, "work")
	if err := g.Push("origin", branch+":"+branch, false); err != nil {
		t.Fatalf("push branch: %v", err)
	}

	// The merge: main fast-forwards to the submitted commit, then the branch is
	// deleted and the worktree's refs are refreshed the way a fetch would.
	testRunGit(t, repo, "push", "origin", branch+":main")
	testRunGit(t, repo, "push", "origin", "--delete", branch)
	testRunGit(t, repo, "fetch", "--prune", "origin")

	if got := classifyResumedPush(g, "origin", "main", pushedCheckpointValue(branch, head), branch, head); got != resumedWorkLanded {
		t.Errorf("classifyResumedPush(merged, branch deleted) = %v, want resumedWorkLanded", got)
	}

	const unmerged = "polecat/granite/gt-mik3-unmerged"
	writeRepoFile(t, repo, "other.txt", "unmerged\n")
	testRunGit(t, repo, "checkout", "-b", unmerged)
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "unmerged work")
	if err := g.Push("origin", unmerged+":"+unmerged, false); err != nil {
		t.Fatalf("push unmerged branch: %v", err)
	}
	unmergedHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	unmergedHead = strings.TrimSpace(unmergedHead)
	testRunGit(t, repo, "push", "origin", "--delete", unmerged)
	testRunGit(t, repo, "fetch", "--prune", "origin")

	if got := classifyResumedPush(g, "origin", "main", pushedCheckpointValue(unmerged, unmergedHead), unmerged, unmergedHead); got != resumedWorkPushed {
		t.Errorf("classifyResumedPush(branch deleted, work never landed) = %v, want resumedWorkPushed", got)
	}
}

// noSleep is the sleep injection for tests that must not wait out the retry
// delay. Returning immediately keeps them fast without changing which code path
// runs; the budget itself is pinned by
// TestLandBranchPushBeforeMRStaysBoundedAndFailsClosed.
func noSleep(time.Duration) {}

// TestLandBranchPushBeforeMRProceedsWhenOriginAlreadyHasTheCommit is the
// gt-0opm regression test: the first assertion failed the way a remote query
// races the ref update it asks about, while origin already holds the commit.
// The submission must go through rather than exit on the failed attempt.
func TestLandBranchPushBeforeMRProceedsWhenOriginAlreadyHasTheCommit(t *testing.T) {
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
		return verifyPushLandedBeforeMR(g, townRoot, "gastown", branch, head)
	}
	pushCalls := 0
	attemptPush := func() error {
		pushCalls++
		return g.Push("origin", branch+":"+branch, false)
	}

	recovered, err := landBranchPushBeforeMR(attemptPush, verify, noSleep)
	if err != nil {
		t.Fatalf("landBranchPushBeforeMR = %v, want nil: origin has the commit", err)
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

// TestLandBranchPushBeforeMRRetriesOnceWhenTheFirstSendFailed covers the other
// half of gt-0opm: the push command itself reported failure. One re-send is
// allowed, and it is what proves the landing — a re-query alone could not.
func TestLandBranchPushBeforeMRRetriesOnceWhenTheFirstSendFailed(t *testing.T) {
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

	recovered, err := landBranchPushBeforeMR(attemptPush, verify, noSleep)
	if err != nil {
		t.Fatalf("landBranchPushBeforeMR = %v, want nil", err)
	}
	if !recovered {
		t.Error("recovered = false, want true")
	}
	if pushCalls != 1 {
		t.Errorf("push attempts = %d, want 1", pushCalls)
	}
}

// TestLandBranchPushBeforeMRSkipsTheRetryWhenTheLandingIsAlreadyProven is the
// negative control: a push that proves out on its first assertion must not
// re-send anything or wait at all. Without this the retry would be a tax on
// every ordinary submission.
func TestLandBranchPushBeforeMRSkipsTheRetryWhenTheLandingIsAlreadyProven(t *testing.T) {
	t.Parallel()

	pushCalls, slept := 0, 0
	recovered, err := landBranchPushBeforeMR(
		func() error { pushCalls++; return nil },
		func() error { return nil },
		func(time.Duration) { slept++ },
	)
	if err != nil {
		t.Fatalf("landBranchPushBeforeMR = %v, want nil", err)
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

// TestLandBranchPushBeforeMRStaysBoundedAndFailsClosed pins the retry budget —
// this path runs when a submission has already failed, so every extra attempt
// holds a polecat's slot open — and that the outcome stays fail-closed, with no
// branch reported as landed that origin does not have.
func TestLandBranchPushBeforeMRStaysBoundedAndFailsClosed(t *testing.T) {
	t.Parallel()

	pushCalls, verifyCalls, slept := 0, 0, 0
	pushErr := errors.New("remote hung up")
	recovered, err := landBranchPushBeforeMR(
		func() error { pushCalls++; return pushErr },
		func() error { verifyCalls++; return errors.New("verified_push_failed: branch missing after push") },
		func(time.Duration) { slept++ },
	)
	if err == nil {
		t.Fatal("landBranchPushBeforeMR = nil, want failure: origin never received the commit")
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

// TestLandBranchPushBeforeMRRejectingRemoteStillFails is the gt-2wqt guard
// against the retry weakening it. A remote that refuses the push must never be
// talked into "landed" by a retry: the assertion is the same remote query, so
// re-running it cannot turn a refusal into a subtree the refinery would merge.
func TestLandBranchPushBeforeMRRejectingRemoteStillFails(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	remote := writeRejectingRemote(t, tmp, "remote.git")
	const branch = "polecat/emerald/gt-0opm-rejected"

	g, head := seedRepoWithCommit(t, tmp, remote, branch)
	townRoot := filepath.Join(tmp, "town")

	recovered, err := landBranchPushBeforeMR(
		func() error { return g.Push("origin", branch+":"+branch, false) },
		func() error { return verifyPushLandedBeforeMR(g, townRoot, "gastown", branch, head) },
		noSleep,
	)
	if err == nil {
		t.Fatal("landBranchPushBeforeMR = nil, want failure: the remote refuses the push")
	}
	if recovered {
		t.Error("recovered = true, want false: nothing landed")
	}
	if !strings.Contains(err.Error(), "no MR created") {
		t.Errorf("error = %q, want it to say no MR was created", err)
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
		got := unlandedPushMessage(branch, nil, "", verifyErr)
		if got != verifyErr.Error() {
			t.Errorf("unlandedPushMessage = %q, want the assertion error verbatim", got)
		}
	})

	t.Run("push error and assertion", func(t *testing.T) {
		got := unlandedPushMessage(branch, errors.New("push timed out"), "origin has unrelated commits", verifyErr)
		for _, want := range []string{"push timed out", "origin has unrelated commits", verifyErr.Error()} {
			if !strings.Contains(got, want) {
				t.Errorf("unlandedPushMessage = %q, want it to contain %q", got, want)
			}
		}
		if !strings.Contains(got, branch) {
			t.Errorf("unlandedPushMessage = %q, want it to name the branch", got)
		}
	})
}
