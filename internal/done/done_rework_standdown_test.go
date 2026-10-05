package done

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

func reworkStandDownNote(kind, branch, head string) string {
	return land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Kind: kind, Reason: "the gate refused it",
		Branch: branch, Target: "main", MR: "gt-3e1z4", Head: head,
	})
}

func reworkBead(labels []string, notes string) *beads.Issue {
	return &beads.Issue{
		ID: "gt-3e1z4", Title: "the work", Type: "task", Status: string(beads.StatusOpen),
		Labels: labels, Notes: notes,
	}
}

const standDownHead = "1111111111111111111111111111111111111111"

// TestReworkUnchangedHeadMessageOnlyForAnUnchangedReworkHead pins the four
// legs of the stand-down: a rework label, a live rejection naming the head the
// worktree is on, and a rejection class the diff can answer.
func TestReworkUnchangedHeadMessageOnlyForAnUnchangedReworkHead(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		issue *beads.Issue
		head  string
		want  bool
	}{
		"a rework bead on the rejected head": {
			reworkBead([]string{land.LabelRework}, reworkStandDownNote("gate", "polecat/a/gt-x", standDownHead)),
			standDownHead, true,
		},
		"an abbreviated head": {
			reworkBead([]string{land.LabelRework}, reworkStandDownNote("review", "polecat/a/gt-x", standDownHead)),
			standDownHead[:8], true,
		},
		"a rejection the diff did not cause": {
			reworkBead([]string{land.LabelRework}, reworkStandDownNote("conflict", "polecat/a/gt-x", standDownHead)),
			standDownHead, false,
		},
		"the head moved past the rejection": {
			reworkBead([]string{land.LabelRework}, reworkStandDownNote("gate", "polecat/a/gt-x", "2222222222222222222222222222222222222222")),
			standDownHead, false,
		},
		"no rework label": {
			reworkBead(nil, reworkStandDownNote("gate", "polecat/a/gt-x", standDownHead)),
			standDownHead, false,
		},
		"no rejection note": {
			reworkBead([]string{land.LabelRework}, "Findings: look at the helper.\n"),
			standDownHead, false,
		},
		"an empty head": {
			reworkBead([]string{land.LabelRework}, reworkStandDownNote("gate", "polecat/a/gt-x", standDownHead)),
			"", false,
		},
		"no bead": {nil, standDownHead, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			message, ok := ReworkUnchangedHeadMessage(tc.issue, tc.head)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v (message %q)", ok, tc.want, message)
			}
			if !ok {
				if message != "" {
					t.Errorf("a false verdict carried a message: %q", message)
				}
				return
			}
			for _, want := range []string{"head unchanged since the rejection", "gt land requeue gt-3e1z4", "a new commit is needed"} {
				if !strings.Contains(message, want) {
					t.Errorf("message %q does not name %q", message, want)
				}
			}
		})
	}
}

// TestSubmitStandsDownOnReworkWithTheRejectedHead: gt done on a rework bead
// still sitting on the head its rejection names does not resubmit. It rebases,
// gates and pushes nothing, and leaves the ready label off — the run falls
// through to the close path, which records why (gt-3e1z4).
func TestSubmitStandsDownOnReworkWithTheRejectedHead(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.bd.Seed(beads.Issue{
		ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusOpen),
		Labels: []string{land.LabelRework},
		Notes:  reworkStandDownNote("gate", doneTestBranch, h.repo.head),
	})
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	wantNothingSubmitted(t, h)
	issue := h.source(t)
	if !beads.HasLabel(issue, land.LabelRework) {
		t.Errorf("the stand-down dropped %s: labels %v", land.LabelRework, issue.Labels)
	}
}

// TestSubmitResubmitsWhenTheHeadMoved is the control: a rework bead whose
// branch is past the rejected head is an ordinary resubmission.
func TestSubmitResubmitsWhenTheHeadMoved(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.bd.Seed(beads.Issue{
		ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusOpen),
		Labels: []string{land.LabelRework},
		Notes:  reworkStandDownNote("gate", doneTestBranch, "2222222222222222222222222222222222222222"),
	})
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	issue := h.source(t)
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("a moved head did not submit: labels %v", issue.Labels)
	}
	if beads.HasLabel(issue, land.LabelRework) {
		t.Errorf("a moved head kept %s: labels %v", land.LabelRework, issue.Labels)
	}
}

// TestSubmitResubmitsWhenTheRejectionWasNotDiffCaused: a conflict rejection
// waits on a rebase, not a code change. Standing down would strand it, so the
// ordinary submit runs.
func TestSubmitResubmitsWhenTheRejectionWasNotDiffCaused(t *testing.T) {
	t.Parallel()
	h := newSubmitHarness(t)
	h.bd.Seed(beads.Issue{
		ID: "bd-source", Title: "the work", Type: "task", Status: string(beads.StatusOpen),
		Labels: []string{land.LabelRework},
		Notes:  reworkStandDownNote("conflict", doneTestBranch, h.repo.head),
	})
	if err := h.submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if issue := h.source(t); !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("a conflict rework did not resubmit: labels %v", issue.Labels)
	}
}
