package cmd

import (
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// creatorSpecFor is a spec whose Goal cites each of the given creator ids.
func creatorSpecFor(cites ...string) specdispatch.Spec {
	goal := "A thing exists."
	for _, c := range cites {
		goal += " created by bead " + c + "."
	}
	return specdispatch.Spec{Description: "## Goal\n" + goal + "\n\n## Constraints\nGo only.\n"}
}

// blockedBy is an open bead with an open blocking edge to each id.
func blockedBy(id string, blockers ...string) *beads.Issue {
	issue := &beads.Issue{ID: id, Status: "open"}
	for _, b := range blockers {
		issue.Dependencies = append(issue.Dependencies, beads.IssueDep{ID: b, DependencyType: "blocks", Status: "open"})
	}
	return issue
}

// seededCreatorRead answers from issues and counts the reads of each id. A nil
// issue answers no bead, and an id absent from issues is a failed read.
func seededCreatorRead(issues map[string]*beads.Issue, reads map[string]int) func(string) (*beads.Issue, error) {
	return func(id string) (*beads.Issue, error) {
		reads[id]++
		if issue, ok := issues[id]; ok {
			return issue, nil
		}
		return nil, errors.New("no bead " + id)
	}
}

// Each cite's state is its verdict, one row per close-reason start (creator cite
// 3/4). A cite in the blocker set is read only by the walk; any other cite is
// read exactly once.
func TestSpecCreatorStates(t *testing.T) {
	t.Parallel()
	const open = "gt-cite"
	tests := []struct {
		name    string
		blocked bool
		cite    *beads.Issue
		want    specdispatch.CreatorCite
	}{
		{"open cite in the blocker set is a blocker", true, &beads.Issue{Status: "open"}, specdispatch.CreatorCite{ID: open, State: specdispatch.CreatorBlocker}},
		{"split into retires", false, &beads.Issue{Status: "closed", CloseReason: "split into gt-new"}, specdispatch.CreatorCite{ID: open, State: specdispatch.CreatorRetired, Reason: "split into gt-new"}},
		{"superseded retires, any case", false, &beads.Issue{Status: "closed", CloseReason: "Superseded by gt-new"}, specdispatch.CreatorCite{ID: open, State: specdispatch.CreatorRetired, Reason: "Superseded by gt-new"}},
		{"folded into retires, padded", false, &beads.Issue{Status: "closed", CloseReason: "  FOLDED INTO gt-new"}, specdispatch.CreatorCite{ID: open, State: specdispatch.CreatorRetired, Reason: "  FOLDED INTO gt-new"}},
		{"other close reason lands", false, &beads.Issue{Status: "closed", CloseReason: "closed as done"}, specdispatch.CreatorCite{ID: open, State: specdispatch.CreatorLanded}},
		{"reason naming a split, not starting with it, lands", false, &beads.Issue{Status: "closed", CloseReason: "was split into gt-new"}, specdispatch.CreatorCite{ID: open, State: specdispatch.CreatorLanded}},
		{"open cite outside the set is unlinked", false, &beads.Issue{Status: "open"}, specdispatch.CreatorCite{ID: open, State: specdispatch.CreatorUnlinked}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := map[string]int{}
			self := blockedBy("gt-self")
			if tc.blocked {
				self = blockedBy("gt-self", open)
			}
			issues := map[string]*beads.Issue{"gt-self": self, open: tc.cite}
			got, err := specCreatorStates(creatorSpecFor(open), seededCreatorRead(issues, reads), "gt-self")
			if err != nil {
				t.Fatalf("specCreatorStates: %v", err)
			}
			if want := []specdispatch.CreatorCite{tc.want}; !slices.Equal(got, want) {
				t.Errorf("specCreatorStates = %+v, want %+v", got, want)
			}
			if reads[open] != 1 {
				t.Errorf("cite read %d times, want 1", reads[open])
			}
		})
	}
}

// A spec with no cite reads nothing (creator cite 3/4).
func TestSpecCreatorStatesNoCite(t *testing.T) {
	t.Parallel()
	reads := map[string]int{}
	got, err := specCreatorStates(specdispatch.Spec{Description: "## Goal\nA thing exists.\n"}, seededCreatorRead(nil, reads), "gt-self")
	if err != nil || got != nil || len(reads) != 0 {
		t.Errorf("specCreatorStates = %+v, %v after %d reads, want nil, no error, no reads", got, err, len(reads))
	}
}

// A cite whose read fails or answers no bead is unreadable, and a failed walk is
// an error with no states (creator cite 3/4).
func TestSpecCreatorStatesUnread(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issues  map[string]*beads.Issue
		want    []specdispatch.CreatorCite
		wantErr bool
	}{
		{"cite read fails", map[string]*beads.Issue{"gt-self": blockedBy("gt-self")}, []specdispatch.CreatorCite{{ID: "gt-cite", State: specdispatch.CreatorUnreadable}}, false},
		{"cite answers no bead", map[string]*beads.Issue{"gt-self": blockedBy("gt-self"), "gt-cite": nil}, []specdispatch.CreatorCite{{ID: "gt-cite", State: specdispatch.CreatorUnreadable}}, false},
		{"walk of the bead fails", map[string]*beads.Issue{}, nil, true},
		{"blocker read fails", map[string]*beads.Issue{"gt-self": blockedBy("gt-self", "gt-b")}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := specCreatorStates(creatorSpecFor("gt-cite"), seededCreatorRead(tc.issues, map[string]int{}), "gt-self")
			if (err != nil) != tc.wantErr {
				t.Fatalf("specCreatorStates error = %v, want error %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("specCreatorStates = %+v, want %+v", got, tc.want)
			}
		})
	}
}
