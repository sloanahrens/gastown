package polecat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
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

// recordingExec answers every git call with answer and records the call. It
// matches no argument, so the probe's set of git subcommands is free to change
// without breaking a test that uses it.
func recordingExec(calls *[]git.ExecCall, answer func() (string, string, error)) git.ExecFunc {
	return func(c git.ExecCall) (string, string, error) {
		*calls = append(*calls, c)
		return answer()
	}
}

// TestLiveProbeLocalBoundsEveryExecCall: the probe the pool runs per
// sessionless dir must finish. Every git process it starts runs under
// liveProbeTimeout, so a worktree whose filesystem stalls cannot hold the
// roster read (and every sling behind it) open.
//
// It asserts the bound on every call the recording runner saw and nothing
// about which calls those are or what they answer: the facts the probe reads
// are pinned by the real-repository tests above.
func TestLiveProbeLocalBoundsEveryExecCall(t *testing.T) {
	t.Parallel()
	var calls []git.ExecCall

	probeLiveGitStateLocalExec(t.TempDir(), recordingExec(&calls, func() (string, string, error) {
		return "", "", nil
	}))

	if len(calls) == 0 {
		t.Fatal("runner saw no git calls; the bound cannot be asserted")
	}
	for _, c := range calls {
		if c.Timeout() != liveProbeTimeout {
			t.Errorf("git %s ran with timeout %v, want %v",
				strings.Join(c.Args(), " "), c.Timeout(), liveProbeTimeout)
		}
	}
}

// TestLiveProbeLocalReadsAKilledCallAsUnknown: a call that runs out the
// deadline is a failed measurement, not clean work. The state must be unknown
// with a reason, which the pool counts as an occupied seat. Every call is
// killed, so the test names no subcommand.
func TestLiveProbeLocalReadsAKilledCallAsUnknown(t *testing.T) {
	t.Parallel()
	var calls []git.ExecCall

	got := probeLiveGitStateLocalExec(t.TempDir(), recordingExec(&calls, func() (string, string, error) {
		return "", "", git.ErrTimedOut
	}))

	if len(calls) == 0 {
		t.Fatal("runner saw no git calls; the probe abandoned the read before it could be killed")
	}
	if got.Source != GitStateSourceUnknown {
		t.Fatalf("Source = %q, want %q", got.Source, GitStateSourceUnknown)
	}
	if got.FailedReason == "" {
		t.Fatal("FailedReason = empty for a killed git call")
	}
}

// TestLiveProbeLocalProductionOpenerTakesTheShortcuts: the opener production
// probes through carries no runner and no custom open, plus the deadline, so
// the probe answers the worktree-root and branch questions from .git on disk
// without starting a process. Those are the calls the deadline cannot bound,
// and the reason it is documented as bounding only the process calls.
//
// The unit tier runs a refusing git on PATH, so a fallback to a subprocess
// fails here rather than answering.
func TestLiveProbeLocalProductionOpenerTakesTheShortcuts(t *testing.T) {
	t.Parallel()
	opener := liveProbeOpener(nil)
	if opener.exec != nil || opener.open != nil {
		t.Fatalf("liveProbeOpener(nil) = exec set %v, open set %v; want production's nil runner and nil open",
			opener.exec != nil, opener.open != nil)
	}
	if opener.timeout != liveProbeTimeout {
		t.Fatalf("liveProbeOpener(nil).timeout = %v, want %v", opener.timeout, liveProbeTimeout)
	}

	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "refs", "heads", "main"),
		[]byte("1111111111111111111111111111111111111111\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	branch, err := opener.Open(dir).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch on a hand-written .git = %v; the production opener fell back to a git process", err)
	}
	if branch != "main" {
		t.Fatalf("CurrentBranch = %q, want %q read from HEAD", branch, "main")
	}
}
