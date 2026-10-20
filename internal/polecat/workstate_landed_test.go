package polecat

import (
	"strings"
	"testing"

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
