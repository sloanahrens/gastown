package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/storypoints/estimate"
)

// specEstimateAheadIssue is an open, unassigned bead with the sections the
// estimate hashes, created at the time given.
func specEstimateAheadIssue(id string, created time.Time) *beads.Issue {
	return &beads.Issue{
		ID:          id,
		Title:       "estimate ahead " + id,
		Status:      string(beads.StatusOpen),
		CreatedAt:   created.UTC().Format(time.RFC3339),
		Description: "## Goal\nMake the thing.\n\n## Constraints\n- one file\n\n## Acceptance\n- [ ] it works\n\n## Size\nOne worker.\n",
	}
}

// specEstimateAheadCandidate is a bead the pass may estimate: open,
// unassigned, shaped, its comments read and none written.
func specEstimateAheadCandidate(id string, created time.Time) specEstimateAheadBead {
	return specEstimateAheadBead{
		Issue:        specEstimateAheadIssue(id, created),
		CommentsRead: true,
		Shaped:       true,
	}
}

// specEstimateAheadNote is the comment gt spec estimate writes for an estimate
// of issue: the note carrying the hash of the sections it was made from.
func specEstimateAheadNote(issue *beads.Issue) beads.Comment {
	return beads.Comment{Text: estimate.Note(estimate.Result{
		Points:  3,
		Minutes: 12,
		Rung:    "shape",
		Inputs:  []string{"shape"},
		Hash:    estimate.SectionHash(issue),
	})}
}

func TestStoryPointsEstimateAheadPickTakesTheOldestWithinTheLimit(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	cands := []specEstimateAheadBead{
		specEstimateAheadCandidate("gt-third", base.Add(2*time.Minute)),
		specEstimateAheadCandidate("gt-first", base),
		specEstimateAheadCandidate("gt-second", base.Add(time.Minute)),
	}

	got := specEstimateAheadPick(cands, specEstimateAheadLimit)
	if want := []string{"gt-first", "gt-second"}; !slices.Equal(got, want) {
		t.Fatalf("pick = %v, want %v: the two oldest, oldest first", got, want)
	}

	// The third is eligible and left for the next pass by the budget alone.
	if all := specEstimateAheadPick(cands, 3); !slices.Equal(all, []string{"gt-first", "gt-second", "gt-third"}) {
		t.Fatalf("pick with a budget of three = %v, want all three", all)
	}
}

func TestStoryPointsEstimateAheadPickOrdersDeterministically(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	unreadable := specEstimateAheadCandidate("gt-undated", base)
	unreadable.Issue.CreatedAt = ""
	cands := []specEstimateAheadBead{
		unreadable,
		specEstimateAheadCandidate("gt-b", base),
		specEstimateAheadCandidate("gt-a", base),
		specEstimateAheadCandidate("gt-late", base.Add(time.Minute)),
	}

	// A bead with no readable created_at sorts after every dated one, and
	// beads created together are ordered by ID.
	got := specEstimateAheadPick(cands, 10)
	if want := []string{"gt-a", "gt-b", "gt-late", "gt-undated"}; !slices.Equal(got, want) {
		t.Fatalf("pick = %v, want %v", got, want)
	}
}

func TestStoryPointsEstimateAheadPickSkipsABeadEstimatedForItsHash(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	estimated := specEstimateAheadIssue("gt-est", base)
	estimated.Labels = []string{"sp:3", "sp-rung:shape", "sp-minutes:12"}
	changed := *estimated
	changed.Description = strings.Replace(estimated.Description, "Make the thing.", "Make the other thing.", 1)
	if estimate.SectionHash(&changed) == estimate.SectionHash(estimated) {
		t.Fatal("the changed description hashes the same as the unchanged one")
	}

	rows := []struct {
		name     string
		issue    *beads.Issue
		comments []beads.Comment
		want     []string
	}{
		{
			name:     "an sp-minutes label for its current sections is never chosen",
			issue:    estimated,
			comments: []beads.Comment{specEstimateAheadNote(estimated)},
		},
		{
			name:  "a note recording no estimate for its current sections is never chosen",
			issue: estimated,
			comments: []beads.Comment{{
				Text: specEstimateNoneNote(estimate.SectionHash(estimated), errors.New("the shape cannot judge it")),
			}},
		},
		{
			name:     "a bead whose description changed since its label is chosen",
			issue:    &changed,
			comments: []beads.Comment{specEstimateAheadNote(estimated)},
			want:     []string{"gt-est"},
		},
		{
			// The estimate writes its labels before its note, so a failed note
			// write leaves a bead wearing current labels and no note. The pick
			// cannot tell those labels are current, so it must leave them be.
			name:  "labels with no estimate note are never chosen",
			issue: estimated,
		},
		{
			name:     "a note naming no hash does not date the labels",
			issue:    estimated,
			comments: []beads.Comment{{Text: specEstimateNotePrefix + " sp:3 rung:shape"}},
		},
		{
			// sp-actual is the landing worker's measurement, not an estimate's
			// label, so it neither dates an estimate nor blocks one.
			name: "a bead wearing only the landing worker's sp-actual label is chosen",
			issue: func() *beads.Issue {
				i := specEstimateAheadIssue("gt-actual", base)
				i.Labels = []string{"sp-actual:9"}
				return i
			}(),
			want: []string{"gt-actual"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cands := []specEstimateAheadBead{{
				Issue:        row.issue,
				Comments:     row.comments,
				CommentsRead: true,
				Shaped:       true,
			}}
			if got := specEstimateAheadPick(cands, specEstimateAheadLimit); !slices.Equal(got, row.want) {
				t.Fatalf("pick = %v, want %v", got, row.want)
			}
		})
	}
}

func TestStoryPointsEstimateAheadPickSkipsBeadsItCannotEstimate(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	subject := func() specEstimateAheadBead {
		// Older than the control, so a filter that lets the subject through
		// shows it first in the result.
		return specEstimateAheadCandidate("gt-sub", base.Add(-time.Minute))
	}

	rows := []struct {
		name  string
		build func() specEstimateAheadBead
		want  []string
	}{
		{
			name: "an assigned bead",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Issue.Assignee = "gastown/polecats/opal"
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name: "a bead the shape lint refused",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Shaped = false
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name: "a bead whose comments were not read",
			build: func() specEstimateAheadBead {
				b := subject()
				b.CommentsRead = false
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name: "a bead with an open blocking dependency",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Issue.Dependencies = []beads.IssueDep{{ID: "gt-blocker", Status: "open", DependencyType: "blocks"}}
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name: "a bead whose block is named only by its list row",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Issue.BlockedBy = []string{"gt-blocker"}
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name: "a bead whose dependency relation was not read",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Issue.Dependencies = []beads.IssueDep{{ID: "gt-blocker", Status: "open"}}
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name: "a bead that is not open",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Issue.Status = string(beads.StatusDeferred)
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name: "a bead with no id",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Issue.ID = ""
				return b
			},
			want: []string{"gt-ok"},
		},
		{
			name:  "no bead at all",
			build: func() specEstimateAheadBead { return specEstimateAheadBead{CommentsRead: true, Shaped: true} },
			want:  []string{"gt-ok"},
		},
		{
			name: "a bead whose blocking dependency is closed",
			build: func() specEstimateAheadBead {
				b := subject()
				b.Issue.Dependencies = []beads.IssueDep{{ID: "gt-blocker", Status: "closed", DependencyType: "blocks"}}
				return b
			},
			want: []string{"gt-sub", "gt-ok"},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cands := []specEstimateAheadBead{row.build(), specEstimateAheadCandidate("gt-ok", base)}
			if got := specEstimateAheadPick(cands, specEstimateAheadLimit); !slices.Equal(got, row.want) {
				t.Fatalf("pick = %v, want %v", got, row.want)
			}
		})
	}
}

func TestStoryPointsEstimateAheadPickWithoutABudget(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	cands := []specEstimateAheadBead{specEstimateAheadCandidate("gt-a", base)}
	for _, limit := range []int{0, -1} {
		if got := specEstimateAheadPick(cands, limit); got != nil {
			t.Fatalf("pick with a budget of %d = %v, want none", limit, got)
		}
	}
}

func TestStoryPointsEstimateAheadPickChoosesARepeatedBeadOnce(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	cands := []specEstimateAheadBead{
		specEstimateAheadCandidate("gt-a", base),
		specEstimateAheadCandidate("gt-a", base),
		specEstimateAheadCandidate("gt-b", base.Add(time.Minute)),
	}
	if got, want := specEstimateAheadPick(cands, specEstimateAheadLimit), []string{"gt-a", "gt-b"}; !slices.Equal(got, want) {
		t.Fatalf("pick = %v, want %v: a bead the input repeats is chosen once", got, want)
	}
}
