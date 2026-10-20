package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

// landedBeads is a beads store for the landed-evidence tests: beadsfake reads
// and the empty merge-request answer applyWorkLandedToWorkstateFacts' caller
// never asks for.
type landedBeads struct{ *beadsfake.Fake }

func (landedBeads) FindMRForBranchAny(string) (*beads.Issue, error) { return nil, nil }

// The bead is the seat's hook while it has one, and the bead its branch name
// encodes once the hook is cleared (gt-jol0i).
func TestApplyWorkLandedToWorkstateFactsPicksTheSeatBead(t *testing.T) {
	t.Parallel()
	db := landedBeads{Fake: beadsfake.New()}
	db.Seed(beads.Issue{ID: "gt-hook", Title: "hooked", Status: "closed"})
	db.Seed(beads.Issue{ID: "gt-branch", Title: "from the branch", Status: "closed"})

	tests := []struct {
		name   string
		facts  polecat.WorkstateFacts
		branch string
		want   string
	}{
		{name: "hook bead wins", facts: polecat.WorkstateFacts{HookBead: "gt-hook", UnpushedCommits: 1}, branch: "polecat/nitro/gt-branch+x1", want: "gt-hook"},
		{name: "branch names the bead when the hook is cleared", facts: polecat.WorkstateFacts{UnpushedCommits: 1}, branch: "polecat/nitro/gt-branch+x1", want: "gt-branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotBead string
			var gotIssue *beads.Issue
			probe := func(_ land.WorkLandedRepo, _ string, _ string, beadID string, issue *beads.Issue) (bool, string) {
				gotBead, gotIssue = beadID, issue
				return true, "landed"
			}

			facts := tt.facts
			applyWorkLandedToWorkstateFacts(probe, db, &facts, "/worktree", tt.branch)
			if gotBead != tt.want {
				t.Fatalf("bead = %q; want %q", gotBead, tt.want)
			}
			if gotIssue == nil || gotIssue.ID != tt.want {
				t.Fatalf("issue = %+v; want %s read from the store", gotIssue, tt.want)
			}
			if !facts.WorkLanded {
				t.Fatalf("facts.WorkLanded = false; want the landed answer recorded")
			}
		})
	}
}

// A worktree whose content disagrees with the landing keeps the reason, which
// names the files an operator must compare.
func TestApplyWorkLandedToWorkstateFactsRecordsTheDifferingFiles(t *testing.T) {
	t.Parallel()
	const reason = "landed from another head; worktree differs in b.txt"
	probe := func(land.WorkLandedRepo, string, string, string, *beads.Issue) (bool, string) {
		return false, reason
	}

	facts := polecat.WorkstateFacts{UnpushedCommits: 2}
	applyWorkLandedToWorkstateFacts(probe, landedBeads{Fake: beadsfake.New()}, &facts, "/worktree", "polecat/nitro/gt-branch+x1")
	if facts.WorkLanded {
		t.Fatalf("facts.WorkLanded = true; want false")
	}
	if facts.WorkLandedDiffReason != reason {
		t.Fatalf("facts.WorkLandedDiffReason = %q; want %q", facts.WorkLandedDiffReason, reason)
	}
}

// A branch that names no bead, and a worktree that is gone, leave the facts
// alone rather than inventing evidence.
func TestApplyWorkLandedToWorkstateFactsLeavesNoEvidenceAlone(t *testing.T) {
	t.Parallel()
	calls := 0
	probe := func(land.WorkLandedRepo, string, string, string, *beads.Issue) (bool, string) {
		calls++
		return true, "landed"
	}

	facts := polecat.WorkstateFacts{UnpushedCommits: 1}
	applyWorkLandedToWorkstateFacts(probe, landedBeads{Fake: beadsfake.New()}, &facts, "", "polecat/nitro/gt-branch+x1")
	applyWorkLandedToWorkstateFacts(probe, landedBeads{Fake: beadsfake.New()}, &facts, "/worktree", "main")
	if calls != 0 {
		t.Fatalf("probe calls = %d; want none without a worktree and a bead", calls)
	}
	if facts.WorkLanded || facts.WorkLandedDiffReason != "" {
		t.Fatalf("facts = %+v; want no landing evidence", facts)
	}
}
