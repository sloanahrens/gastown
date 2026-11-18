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
			name:  "needs-split label",
			issue: beads.Issue{Status: "open", Labels: []string{"needs-split"}},
			want:  "label needs-split",
		},
		{
			// A bead only a person can land: the crew holds it, and a polecat
			// slung onto it can only waste a seat (gt-me800).
			name:  "human-only label",
			issue: beads.Issue{Status: "open", Labels: []string{HumanOnlyLabel}},
			want:  "label human-only",
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
			issue: beads.Issue{Status: "open", Assignee: "ada"},
			want:  "assignee ada is not an agent address",
		},
		{
			name:  "agent assignee",
			issue: beads.Issue{Status: "open", Assignee: "gastown/polecats/onyx"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DispatchHoldFields(tc.issue.Status, tc.issue.Labels, tc.issue.Assignee, tc.issue.Design, tc.issue.Notes, nil)
			if got != tc.want {
				t.Errorf("DispatchHoldFields(%+v) = %q, want %q", tc.issue, got, tc.want)
			}
		})
	}
}

// TestSlingHoldFields_Verdicts pins the read a sling makes at the moment it
// takes a bead (gt-0k7kb): the fields a dispatch starting mid-flight can find
// changed, less the routing labels. A hold written while a dispatch was
// spawning — the operator parking a bead for themselves, a ruling added to its
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
			// A manual sling onto a human-only bead is refused here, which is
			// the read a `gt sling` without --force answers to (gt-me800).
			name:  "human-only label",
			issue: beads.Issue{Status: "open", Labels: []string{"Human-Only"}},
			want:  "label Human-Only",
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
			issue: beads.Issue{Status: "open", Assignee: "ada"},
			want:  "assignee ada is not an agent address",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SlingHoldFields(tc.issue.Labels, tc.issue.Assignee, tc.issue.Design, tc.issue.Notes, nil)
			if got != tc.want {
				t.Errorf("SlingHoldFields(%+v) = %q, want %q", tc.issue, got, tc.want)
			}
		})
	}
}

// TestHasLabel pins the fold the hold rule and its callers share: a label typed
// by hand matches whatever its case or padding (gt-qaelo).
func TestHasLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
		want   string
		found  bool
	}{
		{"exact", []string{"human-only"}, HumanOnlyLabel, true},
		{"upper case", []string{"Human-Only"}, HumanOnlyLabel, true},
		{"padded", []string{"  human-only "}, HumanOnlyLabel, true},
		{"padded want", []string{"human-only"}, " HUMAN-ONLY ", true},
		{"among others", []string{"gt:bug", "HUMAN-ONLY"}, HumanOnlyLabel, true},
		{"absent", []string{"gt:bug"}, HumanOnlyLabel, false},
		{"prefix only", []string{"human-only-ish"}, HumanOnlyLabel, false},
		{"no labels", nil, HumanOnlyLabel, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasLabel(tc.labels, tc.want); got != tc.found {
				t.Errorf("HasLabel(%q, %q) = %v, want %v", tc.labels, tc.want, got, tc.found)
			}
		})
	}
}

// TestHumanHeld pins the holds that keep a rejected or reverted bead with its
// holder: the human-only label and the two overseer-first labels that replace
// it, matched as HasLabel matches. needs-human is not among them.
func TestHumanHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
		held   bool
	}{
		{"human-only", []string{HumanOnlyLabel}, true},
		{"overseer-only", []string{OverseerOnlyLabel}, true},
		{"operator-only", []string{OperatorOnlyLabel}, true},
		{"overseer-only typed by hand", []string{" Overseer-Only "}, true},
		{"among others", []string{"gt:bug", "OPERATOR-ONLY"}, true},
		{"prefix only", []string{"overseer-only-ish"}, false},
		{"needs-human is not a holder hold", []string{"needs-human"}, false},
		{"no labels", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HumanHeld(tc.labels); got != tc.held {
				t.Errorf("HumanHeld(%q) = %v, want %v", tc.labels, got, tc.held)
			}
		})
	}
}

// TestOverseerFirstLabelsHold pins the overseer-first labels to the hold rule
// the old names follow: a bead wearing one is held by the dispatch read and by
// the sling's read, with the reason naming the label as typed. The readers learn
// these names before any writer moves to them (gt-ft52x).
func TestOverseerFirstLabelsHold(t *testing.T) {
	t.Parallel()
	labels := []string{
		OverseerOnlyLabel,
		OperatorOnlyLabel,
		NeedsOverseerLabel,
		NeedsOperatorLabel,
		"needs-overseer",
		"needs-operator",
		"Overseer-Only",
	}
	for _, label := range labels {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			want := "label " + label
			if got := DispatchHoldFields("open", []string{label}, "", "", "", nil); got != want {
				t.Errorf("DispatchHoldFields with %q = %q, want %q", label, got, want)
			}
			if got := SlingHoldFields([]string{label}, "", "", "", nil); got != want {
				t.Errorf("SlingHoldFields with %q = %q, want %q", label, got, want)
			}
		})
	}
}

// TestDispatchHoldLabelPrefixVerdicts pins the hold: labels the birds write on
// a bead they file for the overseer to vet (gt-la0djc): a label starting with
// the exact lower-case hold: prefix holds the bead, with the reason naming the
// label as typed, and a label that merely carries "hold" does not. Both reads
// of the shared rule refuse it — the dispatcher's draw through
// DispatchHoldFields and the sling guard's through SlingHoldFields — since a
// server-side ready query cannot filter a prefix.
func TestDispatchHoldLabelPrefixVerdicts(t *testing.T) {
	t.Parallel()
	t.Run("holds", func(t *testing.T) {
		t.Parallel()
		for _, label := range []string{"hold:decision", "hold:vet", "hold:order"} {
			t.Run(label, func(t *testing.T) {
				t.Parallel()
				want := "label " + label
				if got := DispatchHoldFields("open", []string{label}, "", "", "", nil); got != want {
					t.Errorf("DispatchHoldFields with %q = %q, want %q", label, got, want)
				}
				if got := SlingHoldFields([]string{label}, "", "", "", nil); got != want {
					t.Errorf("SlingHoldFields with %q = %q, want %q", label, got, want)
				}
			})
		}
	})
	t.Run("does not hold", func(t *testing.T) {
		t.Parallel()
		for _, label := range []string{"holdover", "on-hold", "hold", "Hold:vet"} {
			t.Run(label, func(t *testing.T) {
				t.Parallel()
				if got := DispatchHoldFields("open", []string{label}, "", "", "", nil); got != "" {
					t.Errorf("DispatchHoldFields with %q = %q, want no hold", label, got)
				}
				if got := SlingHoldFields([]string{label}, "", "", "", nil); got != "" {
					t.Errorf("SlingHoldFields with %q = %q, want no hold", label, got)
				}
			})
		}
	})
	// A bead that already has a reason keeps it, whichever order the labels
	// are written in: the prefix arm runs after the operator reservation and
	// the label lists.
	t.Run("keeps the existing reason", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name     string
			labels   []string
			assignee string
			want     string
		}{
			{"overseer-only before the hold: label", []string{OverseerOnlyLabel, "hold:vet"}, "", "label " + OverseerOnlyLabel},
			{"overseer-only after the hold: label", []string{"hold:vet", OverseerOnlyLabel}, "", "label " + OverseerOnlyLabel},
			{"the operator reservation", []string{"hold:vet", OperatorLabel}, "", "label " + OperatorLabel},
			{"a human assignee", []string{"hold:vet"}, "ada", "assignee ada is not an agent address"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := DispatchHoldFields("open", tc.labels, tc.assignee, "", "", nil); got != tc.want {
					t.Errorf("DispatchHoldFields(%q, %q) = %q, want %q", tc.labels, tc.assignee, got, tc.want)
				}
			})
		}
	})
}

// TestDispatchHoldLabelPrefixLeavesWorkingBeadsAlone pins the blast-radius
// limit on the hold: arm (gt-la0djc): DispatchHoldFields is also the patrol
// scan's restart read of a seat's hooked bead (internal/daemon/patrol_scan.go,
// gt-n38c6), so a bead already worked must not be held by its hold: label. The
// arm applies only to an open, unassigned bead; a hooked or in_progress bead,
// or an assigned one, returns no hold from the patrol scan's verdict. A sling
// re-read under --force keeps the same rule for an assigned bead.
func TestDispatchHoldLabelPrefixLeavesWorkingBeadsAlone(t *testing.T) {
	t.Parallel()
	const seat = "gastown/polecats/ace"
	cases := []struct {
		name     string
		status   string
		assignee string
	}{
		{"hooked and assigned", "hooked", seat},
		{"in_progress and assigned", "in_progress", seat},
		{"in_progress and unassigned", "in_progress", ""},
		{"open and assigned", "open", seat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, label := range []string{"hold:decision", "hold:vet", "hold:order"} {
				if got := DispatchHoldFields(tc.status, []string{label}, tc.assignee, "", "", nil); got != "" {
					t.Errorf("patrol scan DispatchHoldFields(%q, assignee %q) with %q = %q, want no hold", tc.status, tc.assignee, label, got)
				}
			}
		})
	}
	t.Run("sling re-read of an assigned bead", func(t *testing.T) {
		t.Parallel()
		if got := SlingHoldFields([]string{"hold:vet"}, seat, "", "", nil); got != "" {
			t.Errorf("SlingHoldFields with hold:vet on assigned bead = %q, want no hold", got)
		}
	})
}
