package polecat

import (
	"os"
	"path/filepath"
	"testing"
)

// The local probe exists so a caller that measures every seat in the town pays
// no network round trip per seat (gt-8q0s: an ls-remote per seat is what put
// `gt polecat list --all` over 90s). These tests pin the two things that make
// that swap usable: it measures the same worktree facts, and it differs from
// the live probe only where this clone's view of the branch does — stricter
// when a tracking ref is missing, looser when one outlives its remote branch,
// which the last test here pins as the fail-open no network-free probe can
// close (gt-dt0k).

// TestProbeLiveGitStateLocalMatchesLiveOnAPushedBranch: when the clone holds
// the tracking ref for the branch, the local probe answers exactly what the
// network query answers — the common case, and the one the listing runs on.
func TestProbeLiveGitStateLocalMatchesLiveOnAPushedBranch(t *testing.T) {
	t.Parallel()
	repo := initLandedRepo(t)
	repo.startLandedBranch(t, "polecat/zircon/gt-8q0s+abc")

	live := liveProbe(repo.w, repo.work)
	local := localProbe(repo.w, repo.work)

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
	t.Parallel()
	repo := initLandedRepo(t)
	repo.do(t, repo.g().CheckoutNewBranch("unpushed-work", "HEAD"))
	repo.commit(t, "feature.txt", "not pushed anywhere\n", "local only")

	local := localProbe(repo.w, repo.work)
	if local.Source != GitStateSourceLive {
		t.Fatalf("Source = %q, want %q (reason %q)", local.Source, GitStateSourceLive, local.FailedReason)
	}
	if local.UnpushedCommits == 0 {
		t.Fatalf("UnpushedCommits = 0 for a branch that exists only locally — "+
			"the probe compared HEAD against refs/heads/%s and called unpushed work preserved",
			local.Branch)
	}

	// The live probe agrees, so this is not a local-only correction.
	if live := liveProbe(repo.w, repo.work); live.UnpushedCommits == 0 {
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
// is genuinely unpreserved. The other direction of drift — a ref that outlives
// its remote branch — is the looser one, and
// TestProbeLiveGitStateLocalMeasuresTheSameFactsAsTheLiveOne: dirt and stashes
// are facts no probe variant is allowed to skip, so the two agree on them even
// though their branch evidence differs.
func TestProbeLiveGitStateLocalMeasuresTheSameFactsAsTheLiveOne(t *testing.T) {
	t.Parallel()
	repo := initLandedRepo(t)
	repo.startLandedBranch(t, "polecat/zircon/gt-8q0s+dirty")
	if err := os.WriteFile(filepath.Join(repo.work, "uncommitted.txt"), []byte("churn\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	live := liveProbe(repo.w, repo.work)
	local := localProbe(repo.w, repo.work)

	if !live.Dirty || !local.Dirty {
		t.Fatalf("Dirty = live %v / local %v, want both true for an uncommitted file", live.Dirty, local.Dirty)
	}
	if local.StashCount != live.StashCount {
		t.Fatalf("StashCount = live %d / local %d, want agreement", live.StashCount, local.StashCount)
	}
}
