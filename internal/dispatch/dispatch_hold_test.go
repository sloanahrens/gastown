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
			issue: beads.Issue{Status: "open", Labels: []string{"gt:needs-human"}},
			want:  "label gt:needs-human",
		},
		{
			name:  "label, however typed",
			issue: beads.Issue{Status: "open", Labels: []string{"NEEDS-PRO"}},
			want:  "label NEEDS-PRO",
		},
		{
			// The reviewer label was retired with the role (gt-rwp7z.11): a
			// bead still carrying it is held by nothing and dispatches as
			// ordinary ready work.
			name:  "retired review label",
			issue: beads.Issue{Status: "open", Labels: []string{"needs-mayor-review"}},
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
			issue: beads.Issue{Status: "open", Design: "## do not redispatch\npark it"},
			want:  "do not redispatch in design",
		},
		{
			// The ruling the retired role wrote is no longer a hold either: it
			// named the decision's author, not a marker a dispatcher still
			// reads (gt-rwp7z.11).
			name:  "retired design ruling",
			issue: beads.Issue{Status: "open", Design: "## MAYOR DESIGN DECISION\npark it"},
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

// TestSlingHoldFields_Verdicts pins the read a sling makes at the moment it
// takes a bead (gt-0k7kb): the fields a dispatch starting mid-flight can find
// changed, less the routing labels. A hold written while a dispatch was
// spawning — the steward parking a bead for a person, a ruling added to its
// notes — is the read's to catch, a selector the sling's own target reserves
// is not, and the status is the guards' at the start.
func TestSlingHoldFields_Verdicts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		issue beads.Issue
		want  string
	}{
		{
			name:  "clean",
			issue: beads.Issue{Status: "open"},
		},
		{
			// The incident: a needs-human label lands after the dispatcher's
			// last read and before the sling's hook write.
			name:  "human hold label",
			issue: beads.Issue{Status: "open", Labels: []string{"gt:task", "needs-human"}},
			want:  "label needs-human",
		},
		{
			name:  "human hold label, the spelling internal/land writes",
			issue: beads.Issue{Status: "open", Labels: []string{"gt:needs-human"}},
			want:  "label gt:needs-human",
		},
		{
			// The pro seat's selector routes a bead to that seat; a sling is
			// already given its target, so this label is not a hold here. The
			// dispatcher's own read keeps it (specdispatch.Eligible drops only
			// the labels its seats reserve), which is what holds a needs-pro
			// bead in a town with no pro seat.
			name:  "routing label is not a hold",
			issue: beads.Issue{Status: "open", Labels: []string{"needs-pro"}},
		},
		{
			name:  "routing label alongside a human hold",
			issue: beads.Issue{Status: "open", Labels: []string{"needs-pro", "Needs-Human"}},
			want:  "label Needs-Human",
		},
		{
			// A pinned or hooked bead is re-slingable, with --force and without
			// it when its holder is dead; that decision is the guards' at the
			// start, and a second read of the status would refuse it here.
			name:  "status is the guards' at the start",
			issue: beads.Issue{Status: "hooked"},
		},
		{
			name:  "decision in notes",
			issue: beads.Issue{Status: "open", Notes: "context\n\n- do not redispatch"},
			want:  "do not redispatch in notes",
		},
		{
			name:  "operator reservation",
			issue: beads.Issue{Status: "open", Assignee: "sloan"},
			want:  "assignee sloan is not an agent address",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SlingHoldFields(tc.issue.Labels, tc.issue.Assignee, tc.issue.Design, tc.issue.Notes)
			if got != tc.want {
				t.Errorf("SlingHoldFields(%+v) = %q, want %q", tc.issue, got, tc.want)
			}
		})
	}
}
