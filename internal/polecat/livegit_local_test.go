package polecat

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runLandedGitOut runs git and returns its stdout, for the tests that assert
// on absence (a branch that is not on the remote) rather than on success.
func runLandedGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The local probe exists so a caller that measures every seat in the town pays
// no network round trip per seat (gt-8q0s: an ls-remote per seat is what put
// `gt polecat list --all` over 90s). These tests pin the two things that make
// that swap safe: it measures the same worktree facts, and where it differs
// from the live probe it is stricter, never looser.

// TestProbeLiveGitStateLocalMatchesLiveOnAPushedBranch: when the clone holds
// the tracking ref for the branch, the local probe answers exactly what the
// network query answers — the common case, and the one the listing runs on.
func TestProbeLiveGitStateLocalMatchesLiveOnAPushedBranch(t *testing.T) {
	repo := initLandedRepo(t)
	repo.startLandedBranch(t, "polecat/zircon/gt-8q0s+abc")

	live := ProbeLiveGitState(repo.work)
	local := ProbeLiveGitStateLocal(repo.work)

	if live.Source != GitStateSourceLive || local.Source != GitStateSourceLive {
		t.Fatalf("Source = live %q / local %q, want both %q", live.Source, local.Source, GitStateSourceLive)
	}
	if local.Branch != live.Branch || local.Branch == "" {
		t.Fatalf("Branch = live %q / local %q, want the same non-empty branch", live.Branch, local.Branch)
	}
	if live.UnpushedCommits != 0 || local.UnpushedCommits != 0 {
		t.Fatalf("UnpushedCommits = live %d / local %d, want both 0 for a pushed branch",
			live.UnpushedCommits, local.UnpushedCommits)
	}
}

// TestProbeLiveGitStateLocalRejectsABranchThatOnlyExistsLocally is the trap
// the local resolver has to avoid. A branch that was never pushed exists as
// refs/heads/<branch>; resolving the "remote" tip through that ref would
// compare HEAD against itself, report the work preserved, and clear the
// unpushed-work blocker that is the whole reason to probe. The probe must
// instead fall through to the integration branch and report the work as
// unpreserved.
func TestProbeLiveGitStateLocalRejectsABranchThatOnlyExistsLocally(t *testing.T) {
	repo := initLandedRepo(t)
	runLandedGit(t, repo.work, "checkout", "-b", "unpushed-work")
	writeLandedFile(t, filepath.Join(repo.work, "feature.txt"), "not pushed anywhere\n")
	runLandedGit(t, repo.work, "add", ".")
	runLandedGit(t, repo.work, "commit", "-m", "local only")

	if published := runLandedGitOut(t, repo.work, "ls-remote", "--heads", "origin", "unpushed-work"); published != "" {
		t.Fatalf("precondition: branch is already published (%q); the test needs it local-only", published)
	}

	local := ProbeLiveGitStateLocal(repo.work)
	if local.Source != GitStateSourceLive {
		t.Fatalf("Source = %q, want %q (reason %q)", local.Source, GitStateSourceLive, local.FailedReason)
	}
	if local.UnpushedCommits == 0 {
		t.Fatalf("UnpushedCommits = 0 for a branch that exists only locally — "+
			"the probe compared HEAD against refs/heads/%s and called unpushed work preserved",
			local.Branch)
	}

	// The live probe agrees, so this is not a local-only correction.
	if live := ProbeLiveGitState(repo.work); live.UnpushedCommits == 0 {
		t.Fatalf("UnpushedCommits = 0 from the live probe for an unpublished branch")
	}
}

// TestProbeLiveGitStateLocalIsStricterWhenTheTrackingRefIsGone pins the one
// case where the two probes disagree, and the direction of the disagreement.
// A pruned tracking ref leaves the local probe unable to see a branch that is
// still on the remote, so it falls back to the integration branch and reports
// work that has not landed as unpreserved.
//
// That is the accepted cost of dropping the network call from the listing:
// the error flags a seat for recovery, rather than clearing a seat whose work
// is genuinely unpreserved. This test exists so the tradeoff is asserted
// rather than assumed — if the local probe ever becomes looser than the live
// one, that is a fail-open regression and this is where it should show up.
func TestProbeLiveGitStateLocalIsStricterWhenTheTrackingRefIsGone(t *testing.T) {
	repo := initLandedRepo(t)
	repo.startLandedBranch(t, "polecat/zircon/gt-8q0s+pruned")

	// Drop only the local view of the remote branch; the branch itself is
	// still on origin, so the live query still finds it.
	runLandedGit(t, repo.work, "update-ref", "-d", "refs/remotes/origin/polecat/zircon/gt-8q0s+pruned")

	live := ProbeLiveGitState(repo.work)
	local := ProbeLiveGitStateLocal(repo.work)

	if live.UnpushedCommits != 0 {
		t.Fatalf("live UnpushedCommits = %d, want 0 — the branch is on origin and contains HEAD", live.UnpushedCommits)
	}
	if local.UnpushedCommits == 0 {
		t.Fatalf("local UnpushedCommits = 0, want >0 — without the tracking ref the local probe " +
			"cannot see the remote branch and must not claim the work is preserved")
	}
}

// TestProbeLiveGitStateLocalFailsClosedLikeTheLiveProbe: swapping the probe
// must not change how an unmeasurable worktree is classified. Both report an
// attempted-and-failed probe, never a clean worktree and never a recorded one.
func TestProbeLiveGitStateLocalFailsClosedLikeTheLiveProbe(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	live := ProbeLiveGitState(gone)
	local := ProbeLiveGitStateLocal(gone)

	if live.Source != GitStateSourceUnknown || local.Source != GitStateSourceUnknown {
		t.Fatalf("Source = live %q / local %q, want both %q", live.Source, local.Source, GitStateSourceUnknown)
	}
	if local.FailedReason == "" {
		t.Fatalf("FailedReason is empty; an unknown probe must say why")
	}
	if local.Dirty || local.StashCount != 0 || local.UnpushedCommits != 0 {
		t.Fatalf("failed local probe reported facts: %+v", local)
	}
}

// TestProbeLiveGitStateLocalMeasuresTheSameFactsAsTheLiveOne: dirt and stashes
// are facts no probe variant is allowed to skip, so the two agree on them even
// though their branch evidence differs.
func TestProbeLiveGitStateLocalMeasuresTheSameFactsAsTheLiveOne(t *testing.T) {
	repo := initLandedRepo(t)
	repo.startLandedBranch(t, "polecat/zircon/gt-8q0s+dirty")
	writeLandedFile(t, filepath.Join(repo.work, "uncommitted.txt"), "churn\n")

	live := ProbeLiveGitState(repo.work)
	local := ProbeLiveGitStateLocal(repo.work)

	if !live.Dirty || !local.Dirty {
		t.Fatalf("Dirty = live %v / local %v, want both true for an uncommitted file", live.Dirty, local.Dirty)
	}
	if local.StashCount != live.StashCount {
		t.Fatalf("StashCount = live %d / local %d, want agreement", live.StashCount, local.StashCount)
	}
}
