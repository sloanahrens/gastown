package dispatch

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestDispatchHoldFields_Verdicts pins the field rule every automatic
// dispatcher shares (gt-n38c6), case by case: the witness reads a polecat's
// hooked bead as JSON and holds no IssueSource, so the verdict here is the
// whole rule it gets. Each hold names the phrase the reason quotes back, which
// is what an operator reads in the log.
func TestDispatchHoldFields_Verdicts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		issue beads.Issue
		want  string
	}{
		{
			name:  "clean",
			issue: beads.Issue{Status: "open", Notes: "ordinary notes"},
		},
		{
			name:  "label",
			issue: beads.Issue{Status: "open", Labels: []string{"needs-mayor-review"}},
			want:  "label needs-mayor-review",
		},
		{
			name:  "label, however typed",
			issue: beads.Issue{Status: "open", Labels: []string{"NEEDS-PRO"}},
			want:  "label NEEDS-PRO",
		},
		{
			name:  "deferred status",
			issue: beads.Issue{Status: "deferred"},
			want:  "status deferred",
		},
		{
			name:  "pinned status",
			issue: beads.Issue{Status: "pinned"},
			want:  "status pinned",
		},
		{
			name:  "decision in design",
			issue: beads.Issue{Status: "open", Design: "## MAYOR DESIGN DECISION\npark it"},
			want:  "MAYOR DESIGN DECISION in design",
		},
		{
			name:  "decision in notes",
			issue: beads.Issue{Status: "open", Notes: "context\n\n- do not redispatch"},
			want:  "do not redispatch in notes",
		},
		{
			name:  "wording only mentioned",
			issue: beads.Issue{Status: "open", Notes: "a review note quoting 'do not redispatch'"},
		},
		{
			name:  "operator label",
			issue: beads.Issue{Status: "open", Labels: []string{"operator"}},
			want:  "label operator",
		},
		{
			name:  "operator label, however typed",
			issue: beads.Issue{Status: "open", Labels: []string{"Operator"}},
			want:  "label operator",
		},
		{
			name:  "human assignee",
			issue: beads.Issue{Status: "open", Assignee: "sloan"},
			want:  "assignee sloan is not an agent address",
		},
		{
			name:  "agent assignee",
			issue: beads.Issue{Status: "open", Assignee: "gastown/polecats/onyx"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DispatchHoldFields(tc.issue.Status, tc.issue.Labels, tc.issue.Assignee, tc.issue.Design, tc.issue.Notes)
			if got != tc.want {
				t.Errorf("DispatchHoldFields(%+v) = %q, want %q", tc.issue, got, tc.want)
			}
		})
	}
}

// TestHoldInComments_ReleaseLiftsAnEarlierHold pins gt-tq6l's release: the
// comment field is append-only, so the newest decision is the live one and a
// later HOLD RELEASED is what takes a comment-recorded hold off.
func TestHoldInComments_ReleaseLiftsAnEarlierHold(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{
		{Text: "MAYOR DESIGN DECISION: park it"},
		{Text: "> **HOLD RELEASED** — the ruling is in"},
	}
	if got := HoldInComments(comments); got != "" {
		t.Errorf("HoldInComments after a release = %q, want \"\"", got)
	}
	if got := HoldInComments(comments[:1]); got != "MAYOR DESIGN DECISION in comment" {
		t.Errorf("HoldInComments before the release = %q, want the design hold", got)
	}
}
