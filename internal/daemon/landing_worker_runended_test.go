package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/landworker"
)

// runListQuery is one run list read and the filter it carried.
type runListQuery struct {
	owner, repo string
	filter      forgejo.RunFilter
}

// runListerBySHA answers a run list from a script keyed by the head sha the
// filter names, so a test drives the orphan check's read. A filter naming no
// scripted sha lists nothing, a nil scripted list answers the broken reply a
// real client never gives (no list, no error), and listErr answers every read
// with that error.
type runListerBySHA struct {
	mu      sync.Mutex
	bySHA   map[string]*forgejo.RunList
	listErr error
	asked   []runListQuery
}

func (l *runListerBySHA) ListRuns(_ context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, runListQuery{owner: owner, repo: repo, filter: f})
	if l.listErr != nil {
		return nil, l.listErr
	}
	if list, ok := l.bySHA[f.HeadSHA]; ok {
		return list, nil
	}
	return &forgejo.RunList{}, nil
}

// runEnded reads the fixture's runner the way the orphan check will: through
// the RunEnded newPostLandRunner copied from the wiring production builds.
func (w *postLandWiringFixture) runEnded(t *testing.T) (landworker.RunEnd, error) {
	t.Helper()
	return w.runner.RunEnded(context.Background(), landworker.PostLand{BeadID: "gt-abc", Commit: w.older, Target: "main"})
}

// TestPostLandRunEndedReadsTheNewestEndedRun: a commit whose post-land runs
// all ended without a verdict reads ended, naming the newest run's id and
// status whatever order the list came in.
func TestPostLandRunEndedReadsTheNewestEndedRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		runs []forgejo.ActionRun
		want landworker.RunEnd
	}{
		{
			name: "a cancelled run",
			runs: []forgejo.ActionRun{{ID: 11205, Status: "cancelled"}},
			want: landworker.RunEnd{Run: 11205, Status: "cancelled", Ended: true},
		},
		{
			name: "a failed run",
			runs: []forgejo.ActionRun{{ID: 11206, Status: "failure"}},
			want: landworker.RunEnd{Run: 11206, Status: "failure", Ended: true},
		},
		{
			name: "an errored run",
			runs: []forgejo.ActionRun{{ID: 11207, Status: "error"}},
			want: landworker.RunEnd{Run: 11207, Status: "error", Ended: true},
		},
		{
			name: "several ended runs name the newest even when the list is descending",
			runs: []forgejo.ActionRun{{ID: 11205, Status: "failure"}, {ID: 11204, Status: "cancelled"}},
			want: landworker.RunEnd{Run: 11205, Status: "failure", Ended: true},
		},
		{
			name: "several ended runs name the newest whatever the list order",
			runs: []forgejo.ActionRun{{ID: 11204, Status: "cancelled"}, {ID: 11205, Status: "error"}},
			want: landworker.RunEnd{Run: 11205, Status: "error", Ended: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newPostLandWiringFixture(t, time.Hour)
			w.runs.bySHA[w.older] = &forgejo.RunList{Runs: tc.runs}
			got, err := w.runEnded(t)
			if err != nil {
				t.Fatalf("RunEnded: %v", err)
			}
			if got != tc.want {
				t.Fatalf("RunEnded = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPostLandRunEndedNotWhenARunCanStillReport: the end is all-or-nothing. A
// running, waiting, successful or blocked run among ended ones, and an older
// run that can still report behind a newest one that ended, each read not
// ended: while any run can still post the verdict the check waits for, the
// verdict is not orphaned.
func TestPostLandRunEndedNotWhenARunCanStillReport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		runs []forgejo.ActionRun
	}{
		{
			name: "a running run among ended ones",
			runs: []forgejo.ActionRun{{ID: 11205, Status: "failure"}, {ID: 11206, Status: forgejo.RunStatusRunning}},
		},
		{
			name: "a waiting run among ended ones",
			runs: []forgejo.ActionRun{{ID: 11205, Status: "cancelled"}, {ID: 11206, Status: forgejo.RunStatusWaiting}},
		},
		{
			name: "a successful run among ended ones",
			runs: []forgejo.ActionRun{{ID: 11205, Status: "error"}, {ID: 11206, Status: string(forgejo.StateSuccess)}},
		},
		{
			name: "a blocked run among ended ones",
			runs: []forgejo.ActionRun{{ID: 11205, Status: "failure"}, {ID: 11206, Status: forgejo.RunStatusBlocked}},
		},
		{
			name: "a newest run ended with an older one that can still report",
			runs: []forgejo.ActionRun{{ID: 11204, Status: forgejo.RunStatusRunning}, {ID: 11205, Status: "failure"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newPostLandWiringFixture(t, time.Hour)
			w.runs.bySHA[w.older] = &forgejo.RunList{Runs: tc.runs}
			got, err := w.runEnded(t)
			if err != nil {
				t.Fatalf("RunEnded: %v", err)
			}
			if got.Ended {
				t.Fatalf("RunEnded = %+v, want not ended: a run that can still report leaves the verdict unorphaned", got)
			}
		})
	}
}

// TestPostLandRunEndedNoRunsIsNotEnded: a commit Forgejo holds no runs for is
// not an end. No run can have been orphaned, and marking it ended would orphan
// a verdict after the wait and name run 0.
func TestPostLandRunEndedNoRunsIsNotEnded(t *testing.T) {
	t.Parallel()
	w := newPostLandWiringFixture(t, time.Hour)
	w.runs.bySHA[w.older] = &forgejo.RunList{}
	got, err := w.runEnded(t)
	if err != nil {
		t.Fatalf("RunEnded: %v", err)
	}
	if got.Ended {
		t.Fatalf("RunEnded = %+v, want not ended for a commit with no runs", got)
	}
}

// TestPostLandRunEndedReturnsTheListingError: a run list that cannot be read
// is an error, never an end, so a broken Forgejo read cannot orphan a landing.
func TestPostLandRunEndedReturnsTheListingError(t *testing.T) {
	t.Parallel()
	w := newPostLandWiringFixture(t, time.Hour)
	boom := errors.New("forgejo is down")
	w.runs.listErr = boom
	if _, err := w.runEnded(t); !errors.Is(err, boom) {
		t.Fatalf("RunEnded error = %v, want the listing error %v", err, boom)
	}
}

// TestPostLandRunEndedRefusesANilRunList: a client that answers no list on no
// error did not answer, and that must not pass for a commit with no runs.
func TestPostLandRunEndedRefusesANilRunList(t *testing.T) {
	t.Parallel()
	w := newPostLandWiringFixture(t, time.Hour)
	w.runs.bySHA[w.older] = nil
	got, err := w.runEnded(t)
	if err == nil {
		t.Fatalf("RunEnded = %+v, want an error for a nil run list", got)
	}
}

// TestPostLandRunEndedAsksForTheCommitAndTheWorkflow: the runs read are the
// landed commit's and the post-land workflow's, so another commit's or
// workflow's runs cannot answer for this landing.
func TestPostLandRunEndedAsksForTheCommitAndTheWorkflow(t *testing.T) {
	t.Parallel()
	w := newPostLandWiringFixture(t, time.Hour)
	if _, err := w.runEnded(t); err != nil {
		t.Fatalf("RunEnded: %v", err)
	}
	if len(w.runs.asked) != 1 {
		t.Fatalf("run list read %d times, want one read per answer", len(w.runs.asked))
	}
	q := w.runs.asked[0]
	if q.owner != postLandTestOwner || q.repo != "gastown" {
		t.Fatalf("run list asked of %s/%s, want %s/gastown (the rig's repository)", q.owner, q.repo, postLandTestOwner)
	}
	if q.filter.HeadSHA != w.older {
		t.Fatalf("HeadSHA filter = %q, want the landed commit %q", q.filter.HeadSHA, w.older)
	}
	if q.filter.WorkflowID != "post-land.yml" {
		t.Fatalf("WorkflowID filter = %q, want post-land.yml", q.filter.WorkflowID)
	}
}

// TestPostLandRunEndedStatusIsAKnownSpelling: a run's status reaches an
// attention item as text, so the wiring reports only the spellings it knows. A
// status it does not know reads not ended and carries no status, so a value
// the API sent unvalidated cannot be quoted, and an ended answer's status is
// always one of the three terminal spellings.
func TestPostLandRunEndedStatusIsAKnownSpelling(t *testing.T) {
	t.Parallel()
	t.Run("an unknown status reads not ended and carries no status", func(t *testing.T) {
		t.Parallel()
		w := newPostLandWiringFixture(t, time.Hour)
		w.runs.bySHA[w.older] = &forgejo.RunList{Runs: []forgejo.ActionRun{
			{ID: 11205, Status: "ghp_notARealStatusToken"},
		}}
		got, err := w.runEnded(t)
		if err != nil {
			t.Fatalf("RunEnded: %v", err)
		}
		if got.Ended {
			t.Fatalf("RunEnded = %+v, want not ended for a status the wiring does not know", got)
		}
		if got.Status != "" {
			t.Fatalf("RunEnded status = %q, want it empty: the API's unknown spelling must not reach the item", got.Status)
		}
	})
	t.Run("an ended answer carries one of the terminal spellings", func(t *testing.T) {
		t.Parallel()
		for _, status := range []string{"cancelled", "failure", "error"} {
			w := newPostLandWiringFixture(t, time.Hour)
			w.runs.bySHA[w.older] = &forgejo.RunList{Runs: []forgejo.ActionRun{{ID: 11205, Status: status}}}
			got, err := w.runEnded(t)
			if err != nil {
				t.Fatalf("RunEnded for %q: %v", status, err)
			}
			if !got.Ended {
				t.Fatalf("RunEnded for %q = %+v, want ended", status, got)
			}
			switch got.Status {
			case "cancelled", "failure", "error":
			default:
				t.Fatalf("an ended answer's status = %q, want one of cancelled, failure, error", got.Status)
			}
		}
	})
}
