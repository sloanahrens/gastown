package done

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// noSleep stands in for time.Sleep so the landing-retry tests never wait.
func noSleep(time.Duration) {}

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

	recovered, err := landBranchPush(attemptPush, verify, noSleep, pushLandingRetryDelays)
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
		pushLandingRetryDelays,
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
		pushLandingRetryDelays,
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

// TestUnlandedPushMessageNamesTheHalfThatFailed: the push-command error and the
// origin assertion answer different questions, and a reader of a stranded
// submission needs both — unless there was no push error, in which case the
// assertion is the whole story.
func TestUnlandedPushMessageNamesTheHalfThatFailed(t *testing.T) {
	t.Parallel()
	const branch = "polecat/emerald/gt-0opm"
	verifyErr := errors.New("verified_push_failed: branch is not at the commit gt done would declare ready to land")

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

// fakeBareRepo is the rig's bare repo in verifyPushLanded's fallback: it
// answers from the same origin the worktree pushes to.
type fakeBareRepo struct{ origin *fakeDoneRepo }

func (b fakeBareRepo) VerifyPushedCommit(remote, branch, commit string) error {
	b.origin.verifyErr = nil
	return b.origin.VerifyPushedCommit(remote, branch, commit)
}

// TestVerifyPushLandedFailsClosedWhenOriginLacksTheCommit is the gt-2wqt
// regression: a commit origin does not hold is never reported landed, and
// the error names both tips. The rig's bare repo is asked too, and its
// answer comes from origin, so it cannot vouch for the unpushed commit.
func TestVerifyPushLandedFailsClosedWhenOriginLacksTheCommit(t *testing.T) {
	t.Parallel()
	repo := newFakeDoneRepo()
	err := verifyPushLandedVia(repo, fakeBareRepo{origin: repo}, "origin", doneTestBranch, "feature1")
	if err == nil {
		t.Fatal("verifyPushLanded = nil, want failure: origin never received the commit")
	}
	for _, want := range []string{"verified_push_failed", "feature1", "(missing on origin)", "would declare ready to land"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
}

// TestVerifyPushLandedPassesWhenOriginHasTheCommit is the control: a landed
// push verifies, or the guard would block every submission.
func TestVerifyPushLandedPassesWhenOriginHasTheCommit(t *testing.T) {
	t.Parallel()
	repo := newFakeDoneRepo()
	repo.origin[doneTestBranch] = "feature1"
	if err := verifyPushLandedVia(repo, nil, "origin", doneTestBranch, " feature1\n"); err != nil {
		t.Fatalf("verifyPushLanded = %v, want nil", err)
	}
	if err := verifyPushLandedVia(repo, nil, "origin", doneTestBranch, ""); err != nil {
		t.Fatalf("verifyPushLanded with no commit named = %v, want HEAD verified", err)
	}
}

// TestVerifyPushLandedFallsBackToTheBareRepo covers GH #1348: the worktree
// cannot query origin at all, and the rig's bare repo asks origin instead.
// Without a bare repo the failure stands.
func TestVerifyPushLandedFallsBackToTheBareRepo(t *testing.T) {
	t.Parallel()
	repo := newFakeDoneRepo()
	repo.origin[doneTestBranch] = "feature1"
	repo.verifyErr = errors.New("fatal: 'origin' does not appear to be a git repository")
	if err := verifyPushLandedVia(repo, fakeBareRepo{origin: repo}, "origin", doneTestBranch, "feature1"); err != nil {
		t.Fatalf("verifyPushLanded = %v, want nil via the bare repo", err)
	}
	repo.verifyErr = errors.New("fatal: 'origin' does not appear to be a git repository")
	if err := verifyPushLandedVia(repo, nil, "origin", doneTestBranch, "feature1"); err == nil {
		t.Fatal("verifyPushLanded = nil with no way to query origin, want failure")
	}
}

// TestLandBranchPushProceedsWhenOriginAlreadyHasTheCommit is the gt-0opm
// regression: the first assertion failed transiently while origin already
// held the commit. The retry proves it without a second send failing it.
func TestLandBranchPushProceedsWhenOriginAlreadyHasTheCommit(t *testing.T) {
	t.Parallel()
	repo := newFakeDoneRepo()
	repo.origin[doneTestBranch] = "feature1"
	verifyCalls := 0
	verify := func() error {
		verifyCalls++
		if verifyCalls == 1 {
			return errors.New("verified_push_failed: connection reset")
		}
		return verifyPushLandedVia(repo, nil, "origin", doneTestBranch, "feature1")
	}
	pushCalls := 0
	push := func() error {
		pushCalls++
		return pushBranchToOrigin(repo, "origin", t.TempDir(), "gastown", doneTestBranch, "feature1", "origin/main")
	}
	recovered, err := landBranchPush(push, verify, noSleep, pushLandingRetryDelays)
	if err != nil || !recovered {
		t.Fatalf("landBranchPush = %v, recovered %v; want proven by the retry", err, recovered)
	}
	if pushCalls != 1 || len(repo.pushes) != 0 {
		t.Errorf("push attempts %d, sends %v; want one attempt that sent nothing (origin had it)", pushCalls, repo.pushes)
	}
}

// TestLandBranchPushRejectingRemoteStillFails: a remote that refuses every
// push is never talked into "landed" by the retry (gt-2wqt).
func TestLandBranchPushRejectingRemoteStillFails(t *testing.T) {
	t.Parallel()
	repo := newFakeDoneRepo()
	repo.pushErrs = []error{errors.New("pre-receive hook declined"), errors.New("pre-receive hook declined")}
	townRoot := t.TempDir()
	recovered, err := landBranchPush(
		func() error {
			return pushBranchToOrigin(repo, "origin", townRoot, "gastown", doneTestBranch, "feature1", "origin/main")
		},
		func() error { return verifyPushLanded(repo, "origin", townRoot, "gastown", doneTestBranch, "feature1") },
		noSleep,
		pushLandingRetryDelays,
	)
	if err == nil || recovered {
		t.Fatalf("landBranchPush = %v, recovered %v; want failure", err, recovered)
	}
	if !strings.Contains(err.Error(), "would declare ready to land") {
		t.Errorf("error = %q, want it to say what was not declared", err)
	}
}

// TestLandingPushUsesTheConfiguredRemote is the gt-fn9e6.9 guard: the push and
// the read-back name the rig's configured landing remote, not a hard-coded
// origin, so a rig on a separately named Forgejo remote pushes there.
func TestLandingPushUsesTheConfiguredRemote(t *testing.T) {
	t.Parallel()
	repo := newFakeDoneRepo()
	if err := pushBranchToOrigin(repo, "forgejo", t.TempDir(), "gastown", doneTestBranch, "feature1", "origin/main"); err != nil {
		t.Fatalf("pushBranchToOrigin = %v, want nil", err)
	}
	if len(repo.pushRemotes) != 1 || repo.pushRemotes[0] != "forgejo" {
		t.Fatalf("push remotes = %v, want [forgejo]", repo.pushRemotes)
	}
	if err := verifyPushLandedVia(repo, nil, "forgejo", doneTestBranch, "feature1"); err != nil {
		t.Fatalf("verifyPushLandedVia = %v, want nil", err)
	}
	if len(repo.verifyRemotes) == 0 || repo.verifyRemotes[0] != "forgejo" {
		t.Errorf("verify remotes = %v, want the read-back to name forgejo", repo.verifyRemotes)
	}
}
