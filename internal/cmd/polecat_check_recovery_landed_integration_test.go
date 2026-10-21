//go:build integration

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestIntegrationCheckRecoveryClearsASeatWhoseLandedBranchWasReaped is
// gt-jol0i: the landing took the branch, kept its head at refs/landed/<bead>,
// and deleted the origin branch, so the seat's earlier point on that branch
// looks to every branch-presence check like commits on no remote.
func TestIntegrationCheckRecoveryClearsASeatWhoseLandedBranchWasReaped(t *testing.T) {
	t.Parallel()
	const (
		rigName     = "gastown"
		polecatName = "opal"
		beadID      = "gt-abc"
		branch      = "polecat/opal/gt-abc+x1"
	)

	repo := setupGitStateRemoteRepo(t)
	runGitCmd(t, repo, "switch", "-c", branch, "main")
	writeTestFile(t, filepath.Join(repo, "a.txt"), "earlier attempt\n")
	runGitCmd(t, repo, "add", "a.txt")
	runGitCmd(t, repo, "commit", "-m", "earlier attempt")
	earlier := gitOut(t, repo, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(repo, "b.txt"), "carried on\n")
	runGitCmd(t, repo, "add", "b.txt")
	runGitCmd(t, repo, "commit", "-m", "carried on")
	later := gitOut(t, repo, "rev-parse", "HEAD")
	runGitCmd(t, repo, "push", "-u", "origin", branch)

	// The landing: refs/landed holds the head that landed, the branch is gone
	// from origin, and the seat is left at the earlier point it ran.
	runGitCmd(t, repo, "update-ref", land.LandedRef(beadID), later)
	runGitCmd(t, repo, "reset", "--hard", earlier)
	runGitCmd(t, repo, "push", "origin", "--delete", branch)
	runGitCmd(t, repo, "fetch", "--prune", "origin")

	r := &rig.Rig{Name: rigName, Path: filepath.Join(t.TempDir(), rigName)}
	db := &recoveryDB{Fake: beadsfake.New()}
	// hook_bead is cleared, so the bead has to come from the branch name.
	db.Seed(beads.Issue{
		ID: polecatBeadIDForRig(r, rigName, polecatName), Title: "Polecat " + polecatName, Status: "open",
		Labels:      []string{"gt:agent"},
		Description: "role_type: polecat\nrig: " + rigName + "\nagent_state: idle\ncleanup_status: clean\nactive_mr: null\nbranch: " + branch + "\n",
	})
	db.Seed(beads.Issue{
		ID: beadID, Title: "the work", Status: "closed",
		Notes: land.LandingRecord{BeadID: beadID, Head: later, LandedCommit: later, Target: "main"}.NoteBlock(),
	})

	p := &polecat.Polecat{Name: polecatName, Rig: rigName, State: polecat.StateIdle, ClonePath: repo, Branch: branch}
	status := checkRecoveryForPolecat(db, nil, r, rigName, polecatName, p, false)

	if status.Verdict != polecat.WorkstateVerdictSafeToNuke || !status.SafeToNuke || status.NeedsRecovery {
		t.Fatalf("check-recovery(landed seat) = %s safe=%v needs=%v blockers=%v; want SAFE_TO_NUKE",
			status.Verdict, status.SafeToNuke, status.NeedsRecovery, status.Blockers)
	}
	if status.Reason != "landed" {
		t.Fatalf("reason = %q; want landed", status.Reason)
	}
}

// The same seat with no landing evidence is the refusal the check exists for:
// an open bead's unpushed commits stay at risk.
func TestIntegrationCheckRecoveryKeepsUnlandedUnpushedWorkInRecovery(t *testing.T) {
	t.Parallel()
	const (
		rigName     = "gastown"
		polecatName = "opal"
		beadID      = "gt-abc"
		branch      = "polecat/opal/gt-abc+x1"
	)

	repo := setupGitStateRemoteRepo(t)
	runGitCmd(t, repo, "switch", "-c", branch, "main")
	runGitCmd(t, repo, "push", "-u", "origin", branch)
	writeTestFile(t, filepath.Join(repo, "a.txt"), "work\n")
	runGitCmd(t, repo, "add", "a.txt")
	runGitCmd(t, repo, "commit", "-m", "work")

	r := &rig.Rig{Name: rigName, Path: filepath.Join(t.TempDir(), rigName)}
	db := &recoveryDB{Fake: beadsfake.New()}
	db.Seed(beads.Issue{
		ID: polecatBeadIDForRig(r, rigName, polecatName), Title: "Polecat " + polecatName, Status: "open",
		Labels:      []string{"gt:agent"},
		Description: "role_type: polecat\nrig: " + rigName + "\nagent_state: idle\nhook_bead: " + beadID + "\ncleanup_status: clean\nactive_mr: null\nbranch: " + branch + "\n",
	})
	db.Seed(beads.Issue{ID: beadID, Title: "the work", Status: "open", Assignee: rigName + "/polecats/" + polecatName})

	p := &polecat.Polecat{Name: polecatName, Rig: rigName, State: polecat.StateIdle, ClonePath: repo, Branch: branch, Issue: beadID}
	status := checkRecoveryForPolecat(db, nil, r, rigName, polecatName, p, false)

	if status.Verdict != polecat.WorkstateVerdictNeedsRecovery || !status.NeedsRecovery || status.SafeToNuke {
		t.Fatalf("check-recovery(unlanded unpushed work) = %s safe=%v needs=%v; want NEEDS_RECOVERY",
			status.Verdict, status.SafeToNuke, status.NeedsRecovery)
	}
	// The seat still holds its hook, which is listed first, so the unpushed
	// commits are checked among the blockers rather than as the reason.
	unpushed := false
	for _, b := range status.Blockers {
		if strings.HasPrefix(b, "git_state=has_unpushed ") {
			unpushed = true
		}
	}
	if !unpushed {
		t.Fatalf("blockers = %v; want one reporting the unpushed commits (git_state=has_unpushed)", status.Blockers)
	}
}
