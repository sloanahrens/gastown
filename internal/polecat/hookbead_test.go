package polecat

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestClassifyHookBead pins the policy every hook-reading site shares
// (gt-eqiid). The regression it guards is a false positive: a recorded hook
// reference outlives the work it names, and the site reads the reference
// instead of the bead. gt-2xqtj on flint/garnet and gt-gyw5w on granite were
// both `deferred` with no assignee while their old polecats still carried them
// as hook_bead, and `gt hook` in those worktrees renders "Nothing on hook".
func TestClassifyHookBead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		hookBead      string
		issue         *beads.Issue
		err           error
		wantSafe      bool
		wantTerminal  bool
		wantSubmitted bool
	}{
		{name: "no reference", hookBead: "", wantSafe: true},
		{
			name:         "closed bead is terminal",
			hookBead:     "gt-work",
			issue:        &beads.Issue{ID: "gt-work", Status: string(beads.StatusClosed)},
			wantSafe:     true,
			wantTerminal: true,
		},
		{
			// The class 2 false positive, in the shape the live beads had.
			name:     "deferred and unassigned is a stale reference",
			hookBead: "gt-2xqtj",
			issue:    &beads.Issue{ID: "gt-2xqtj", Status: string(beads.StatusDeferred)},
			wantSafe: true,
		},
		{
			name:     "blocked is not an active assignment either",
			hookBead: "gt-gyw5w",
			issue:    &beads.Issue{ID: "gt-gyw5w", Status: "blocked"},
			wantSafe: true,
		},
		{
			// pinned is a permanent reference bead, not an assignment
			// (CLAUDE.md), so a pinned hook reference is inert too — and it is
			// on the enumerated list rather than in the default arm.
			name:     "pinned is a permanent reference, not live work",
			hookBead: "gt-pin",
			issue:    &beads.Issue{ID: "gt-pin", Status: string(beads.IssueStatusPinned)},
			wantSafe: true,
		},
		{
			// The fail-open control. Any status this classifier has not been
			// taught is NOT evidence that the reference went stale, and this
			// gates a nuke: clearing on an unmodeled status is the same
			// fail-open gt-7kr and gt-14a each closed in turn, moved to a
			// fourth place.
			name:     "an unrecognized status fails closed",
			hookBead: "gt-mystery",
			issue:    &beads.Issue{ID: "gt-mystery", Status: "quarantined"},
		},
		{
			name:     "an empty status fails closed",
			hookBead: "gt-mystery",
			issue:    &beads.Issue{ID: "gt-mystery"},
		},
		{
			// The class 1 false positive: a submitted bead IS status=hooked,
			// so it must be recognized before the active-status test.
			name:          "submitted work belongs to the landing worker",
			hookBead:      "gt-acdfp",
			issue:         &beads.Issue{ID: "gt-acdfp", Status: string(beads.IssueStatusHooked), Labels: []string{"gt:ready-to-land"}},
			wantSafe:      true,
			wantSubmitted: true,
		},
		{
			name:         "a landed bead that keeps the label is terminal, not submitted",
			hookBead:     "gt-acdfp",
			issue:        &beads.Issue{ID: "gt-acdfp", Status: string(beads.StatusClosed), Labels: []string{"gt:ready-to-land"}},
			wantSafe:     true,
			wantTerminal: true,
		},
		{
			name:     "hooked is live work",
			hookBead: "gt-work",
			issue:    &beads.Issue{ID: "gt-work", Status: string(beads.IssueStatusHooked)},
		},
		{
			name:     "in_progress is live work",
			hookBead: "gt-work",
			issue:    &beads.Issue{ID: "gt-work", Status: string(beads.StatusInProgress)},
		},
		{
			name:     "open is live work",
			hookBead: "gt-work",
			issue:    &beads.Issue{ID: "gt-work", Status: string(beads.StatusOpen)},
		},
		{
			name:     "a bead that is gone fails closed",
			hookBead: "gt-work",
		},
		{
			name:     "a lookup that failed fails closed",
			hookBead: "gt-work",
			err:      errors.New("bd exploded"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyHookBead(tt.hookBead, tt.issue, tt.err)
			if got.Safe != tt.wantSafe || got.Terminal != tt.wantTerminal || got.Submitted != tt.wantSubmitted {
				t.Fatalf("ClassifyHookBead() = %+v, want safe=%v terminal=%v submitted=%v", got, tt.wantSafe, tt.wantTerminal, tt.wantSubmitted)
			}
			if !got.Safe && got.Blocker == "" {
				t.Fatal("ClassifyHookBead() refused without naming a blocker (gt-3r1h)")
			}
			if got.Safe && got.Blocker != "" {
				t.Fatalf("ClassifyHookBead() = %+v, want no blocker on a safe disposition", got)
			}
		})
	}
}

// TestClassifyHookBeadBlockerNamesTheBead keeps the refusal actionable: the
// operator reading a blocked nuke needs the bead whose status refused, not
// just the word "hook".
func TestClassifyHookBeadBlockerNamesTheBead(t *testing.T) {
	t.Parallel()
	got := ClassifyHookBead("gt-work", &beads.Issue{ID: "gt-work", Status: string(beads.IssueStatusHooked)}, nil)
	if got.Blocker != "hook_bead=gt-work status=hooked" {
		t.Fatalf("Blocker = %q, want %q", got.Blocker, "hook_bead=gt-work status=hooked")
	}
}

// TestClassifyHookBeadUnrecognizedStatusNamesTheStatus: the fail-closed default
// has to be legible. An operator whose nuke was refused by a status this
// classifier does not model needs to see which status that was, not a bare
// "unexpected" — that is the gt-3r1h lesson, and the fastest route to teaching
// the classifier the status is reading it (gt-eqiid).
func TestClassifyHookBeadUnrecognizedStatusNamesTheStatus(t *testing.T) {
	t.Parallel()
	got := ClassifyHookBead("gt-mystery", &beads.Issue{ID: "gt-mystery", Status: "quarantined"}, nil)
	if got.Safe {
		t.Fatal("an unmodeled status must not clear a nuke gate")
	}
	if got.Blocker != "hook_bead=gt-mystery status=quarantined status_unrecognized" {
		t.Fatalf("Blocker = %q, want the status named and marked unrecognized", got.Blocker)
	}
}
