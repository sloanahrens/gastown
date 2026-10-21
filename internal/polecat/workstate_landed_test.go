package polecat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// Landed work is not work in flight: the commits that look unpushed are the
// ones the landing took, so the seat is safe to nuke and says why (gt-jol0i).
func TestDecideWorkstateLandedWorkIsSafeToNuke(t *testing.T) {
	t.Parallel()
	in := NewWorkstateInput(WorkstateFacts{
		State:              StateIdle,
		CleanupStatus:      CleanupClean,
		Branch:             "polecat/opal/gt-abc+x1",
		GitStateSource:     GitStateSourceLive,
		UnpushedCommits:    2,
		WorkLanded:         true,
		MQCheckRequired:    true,
		HasSubmittableWork: true,
	})

	d := DecideWorkstate(in)
	if d.Verdict != WorkstateVerdictSafeToNuke || !d.SafeToNuke || d.NeedsRecovery {
		t.Fatalf("DecideWorkstate(landed work) = %+v; want SAFE_TO_NUKE", d)
	}
	if d.Reason != "landed" {
		t.Fatalf("reason = %q; want landed", d.Reason)
	}
	// Nothing to enqueue: the work is already on the target.
	if d.MQStatus != "submitted" {
		t.Fatalf("mq_status = %q; want submitted", d.MQStatus)
	}
}

// The bead landed from another head and this worktree disagrees: the refusal
// stays under a short reason code, and the blocker names the files an operator
// must compare.
func TestDecideWorkstateNamesFilesWhenTheLandingDiffers(t *testing.T) {
	t.Parallel()
	const detail = "landed from another head; worktree differs in b.txt, c.txt"
	in := NewWorkstateInput(WorkstateFacts{
		State:            StateIdle,
		CleanupStatus:    CleanupClean,
		Branch:           "polecat/opal/gt-abc+x1",
		GitStateSource:   GitStateSourceLive,
		UnpushedCommits:  2,
		WorkLandedReason: land.WorkLandedDiffers,
		WorkLandedDetail: detail,
	})

	d := DecideWorkstate(in)
	if d.Verdict != WorkstateVerdictNeedsRecovery || !d.NeedsRecovery {
		t.Fatalf("DecideWorkstate(differing content) = %+v; want NEEDS_RECOVERY", d)
	}
	if d.Reason != land.WorkLandedDiffers {
		t.Fatalf("reason = %q; want the short code %q", d.Reason, land.WorkLandedDiffers)
	}
	blockers := strings.Join(d.Blockers, "; ")
	if !strings.Contains(blockers, "unpushed_commits=2") || !strings.Contains(blockers, "b.txt, c.txt") {
		t.Fatalf("blockers = %q; want the measured unpushed count and the differing files", blockers)
	}
}

// A landed probe that failed keeps the plain git-unpushed refusal — failing
// closed — and the blocker says why the evidence is missing.
func TestDecideWorkstateKeepsTheRefusalWhenTheLandedProbeFailed(t *testing.T) {
	t.Parallel()
	in := NewWorkstateInput(WorkstateFacts{
		State:            StateIdle,
		CleanupStatus:    CleanupClean,
		Branch:           "polecat/opal/gt-abc+x1",
		GitStateSource:   GitStateSourceLive,
		UnpushedCommits:  1,
		WorkLandedReason: land.WorkProbeFailed,
		WorkLandedDetail: "read bead gt-abc: connection refused",
	})

	d := DecideWorkstate(in)
	if d.Verdict != WorkstateVerdictNeedsRecovery || d.Reason != "git-unpushed" {
		t.Fatalf("DecideWorkstate(failed probe) = %+v; want NEEDS_RECOVERY git-unpushed", d)
	}
	if blockers := strings.Join(d.Blockers, "; "); !strings.Contains(blockers, "connection refused") {
		t.Fatalf("blockers = %q; want the probe failure named", blockers)
	}
}

// An open bead whose work is on no target and holds no landed ref stays the
// plain refusal; landed evidence is what clears a seat, not a closed bead.
func TestDecideWorkstateUnlandedUnpushedWorkStillNeedsRecovery(t *testing.T) {
	t.Parallel()
	in := NewWorkstateInput(WorkstateFacts{
		State:           StateIdle,
		CleanupStatus:   CleanupClean,
		Branch:          "polecat/opal/gt-abc+x1",
		GitStateSource:  GitStateSourceLive,
		UnpushedCommits: 1,
	})

	d := DecideWorkstate(in)
	if d.Verdict != WorkstateVerdictNeedsRecovery || d.Reason != "git-unpushed" {
		t.Fatalf("DecideWorkstate(unlanded unpushed work) = %+v; want NEEDS_RECOVERY git-unpushed", d)
	}
}

// A landed seat whose commits still read as unpushed is not cleanup-unknown:
// the landing took those commits, so the unpushed refusal is skipped for it and
// the shared git-safe predicate the ignore gate reads agrees (gt-v2muu). The
// same facts without landed evidence stay refused.
func TestDecideWorkstateLandedWithUnpushedCommitsIsNotCleanupUnknown(t *testing.T) {
	t.Parallel()
	base := WorkstateFacts{
		State:              StateIdle,
		Branch:             "polecat/opal/gt-abc+x1",
		GitStateSource:     GitStateSourceLive,
		AgentBeadRead:      true,
		HookBeadSafe:       true,
		UnpushedCommits:    2,
		MQCheckRequired:    true,
		HasSubmittableWork: true,
	}

	landed := base
	landed.WorkLanded = true
	d := DecideWorkstate(NewWorkstateInput(landed))
	if d.Verdict != WorkstateVerdictSafeToNuke || d.Reason != "landed" {
		t.Fatalf("DecideWorkstate(landed, empty cleanup_status) = %+v; want SAFE_TO_NUKE landed", d)
	}

	unlanded := DecideWorkstate(NewWorkstateInput(base))
	if unlanded.Verdict != WorkstateVerdictNeedsRecovery {
		t.Fatalf("DecideWorkstate(unlanded, empty cleanup_status) = %+v; want NEEDS_RECOVERY", unlanded)
	}
	if blockers := strings.Join(unlanded.Blockers, "; "); !strings.Contains(blockers, "unpushed_commits=2") {
		t.Fatalf("blockers = %q; want the unlanded unpushed commits named", blockers)
	}
}

// The reuse gate builds its input from a live probe of the seat, so the landed
// rule must hold through the manager and not only through facts a test writes
// by hand: a seat whose branch commit the landing took, with no recorded
// cleanup_status, is safe to nuke, and the same seat without refs/landed stays
// refused (gt-v2muu).
func TestWorkstateDispositionForPolecatLandedUnpushedSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		landed      bool
		wantVerdict string
		wantReason  string
	}{
		{name: "landed commits are not at risk", landed: true, wantVerdict: WorkstateVerdictSafeToNuke, wantReason: "landed"},
		{name: "unlanded commits stay refused", wantVerdict: WorkstateVerdictNeedsRecovery, wantReason: "cleanup-unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, mayorRig, bd, added, w := canonicalWithPolecats(t, true, "toast")
			bd.Seed(beads.Issue{ID: "gt-seat", Title: "seat", Status: "open"})
			const branch = "polecat/toast/gt-seat+x1"
			clonePath := added["toast"].ClonePath
			// The fixture's origin is the rig's own repo, which the polecat
			// worktrees share, so a new commit would already be "on origin".
			// Give origin a repo of its own that has only main.
			bare := filepath.Join(t.TempDir(), "origin.git")
			w.InitBare(t, bare)
			w.SetRef(t, bare, "refs/heads/main", w.rev(t, clonePath, "HEAD"))
			w.AddRemote(t, clonePath, "origin", bare)
			if err := w.repo(clonePath).CheckoutNewBranch(branch, "HEAD"); err != nil {
				t.Fatalf("checkout %s: %v", branch, err)
			}
			head := w.writeAndCommit(t, clonePath, "feat: seat work", map[string]string{"seat.txt": "work\n"})
			if tt.landed {
				w.SetRef(t, mayorRig, land.LandedRef("gt-seat"), head)
			}
			// No recorded cleanup_status: only the live probe can clear the seat.
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.CleanupStatus = ""; f.Branch = branch })

			d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")
			if d.Verdict != tt.wantVerdict || d.Reason != tt.wantReason {
				t.Fatalf("WorkstateDispositionForPolecat() = %+v; want %s %s", d, tt.wantVerdict, tt.wantReason)
			}
		})
	}
}
