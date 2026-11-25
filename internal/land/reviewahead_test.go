package land

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// aheadReadyIssue is the bead a landing of w finds ready: actionable, labelled
// for landing, with no policy refusal and no overseer waiver.
func aheadReadyIssue(w Work) beads.Issue {
	return beads.Issue{ID: w.BeadID, Title: "add b", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(w)}
}

// aheadSeedBead replaces the fixture's bead store with one holding issue, the
// only bead ReviewAhead reads.
func aheadSeedBead(f *landFixture, issue beads.Issue) {
	f.t.Helper()
	f.bd = beadsfake.New(beadsfake.WithPrefix("gt"))
	f.bd.Seed(issue)
}

// aheadBeadWrites counts the writes the bead store has journaled. The journal
// holds every create, update, close, note and comment the Beads interface can
// make, so a write that ReviewAhead made would move the count.
func aheadBeadWrites(t *testing.T, bd *beadsfake.Fake) int {
	t.Helper()
	page, err := bd.EventsTail(0, 0)
	if err != nil {
		t.Fatalf("reading the bead journal: %v", err)
	}
	return len(page.Records)
}

func TestVerdictReuseAheadSkips(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		store bool // whether the lander has a review-ahead store
		issue func(w Work) beads.Issue
	}{
		{"nil Ahead", false, aheadReadyIssue},
		{"bead not actionable", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Status = "closed"
			return i
		}},
		{"bead lacks ready label", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Labels = nil
			return i
		}},
		{"policy refusal", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.AcceptanceCriteria = "- [ ] add b"
			return i
		}},
		{"waived head", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Labels = append(i.Labels, LabelOverseerReviewed)
			i.Notes = FormatReadyNote(w) + "\n" + FormatOverseerReviewedNote(w.Head)
			return i
		}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			aheadSeedBead(f, tc.issue(f.work))
			l := f.lander()
			if tc.store {
				l.Ahead = TownAheadStore(f.town, "gastown")
			} else {
				// With no store the bead must not be read: a read that fails
				// would surface as an error, so reading it is observable.
				f.bd.FailWith("show", errors.New("bd show: read with the feature off"))
			}
			before := aheadBeadWrites(t, f.bd)
			got, err := l.ReviewAhead(context.Background(), f.work)
			if err != nil {
				t.Fatalf("ReviewAhead: unexpected error: %v", err)
			}
			if got != AheadSkipped {
				t.Errorf("ReviewAhead = %q, want %q", got, AheadSkipped)
			}
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
			}
		})
	}
}

// aheadNilShowBeads is a bead store whose Show answers no bead and no error,
// the shape an unexpected store answer takes.
type aheadNilShowBeads struct {
	*beadsfake.Fake
}

func (aheadNilShowBeads) Show(string) (*beads.Issue, error) { return nil, nil }

func TestVerdictReuseAheadShowFails(t *testing.T) {
	t.Parallel()
	errShow := errors.New("bd show: store unavailable")
	rows := []struct {
		name    string
		beads   func(f *landFixture) Beads
		wantErr error // nil means any error
	}{
		{"Show returns an error", func(f *landFixture) Beads {
			f.bd.FailWith("show", errShow)
			return f.bd
		}, errShow},
		{"Show returns no bead", func(f *landFixture) Beads {
			return aheadNilShowBeads{f.bd}
		}, nil},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			aheadSeedBead(f, aheadReadyIssue(f.work))
			l := f.lander()
			l.Ahead = TownAheadStore(f.town, "gastown")
			l.Beads = tc.beads(f)
			got, err := l.ReviewAhead(context.Background(), f.work)
			if err == nil {
				t.Fatalf("ReviewAhead: got outcome %q and no error, want an error", got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("ReviewAhead error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if got != "" {
				t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
			}
		})
	}
}
