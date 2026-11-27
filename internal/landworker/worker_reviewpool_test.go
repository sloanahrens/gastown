package landworker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// readySubmission is a ready bead whose READY TO LAND block names branch and
// head. It is not seeded: resolveWork reads the block from the bead itself.
func readySubmission(id, branch, head string) *beads.Issue {
	return &beads.Issue{ID: id, Labels: []string{land.LabelReadyToLand},
		Notes: land.FormatReadyNote(land.Work{Branch: branch, Head: head, Target: "main", Worker: "opal"})}
}

// TestReviewPoolWorkerReviewWorksBuilds pins that reviewWorks builds one work
// for each issue given, each with the branch and head of its own submission,
// so the pool reviews the head the landing will take (gt-uuvtbv).
func TestReviewPoolWorkerReviewWorksBuilds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	first := readySubmission("gt-first", "polecat/opal/gt-first+x1", noteHead)
	second := readySubmission("gt-second", "polecat/opal/gt-second+x1", tipHead)

	works, err := h.w.reviewWorks([]*beads.Issue{first, second}, "")
	if err != nil {
		t.Fatalf("reviewWorks: %v", err)
	}
	if len(works) != 2 {
		t.Fatalf("reviewWorks returned %d works; want one for each of the 2 issues", len(works))
	}
	for i, want := range []struct{ bead, branch, head string }{
		{"gt-first", "polecat/opal/gt-first+x1", noteHead},
		{"gt-second", "polecat/opal/gt-second+x1", tipHead},
	} {
		got := works[i]
		if got.BeadID != want.bead || got.Branch != want.branch || got.Head != want.head {
			t.Fatalf("work %d = {bead %q branch %q head %q}; want {bead %q branch %q head %q}",
				i, got.BeadID, got.Branch, got.Head, want.bead, want.branch, want.head)
		}
		if got.Rig != h.w.Rig {
			t.Fatalf("work %d rig = %q; want the worker's rig %q", i, got.Rig, h.w.Rig)
		}
	}
}

// TestReviewPoolWorkerReviewWorksOrder pins that reviewWorks offers the works
// in the order the landing takes them, whatever order the issues arrive in: a
// P0 first, then the earliest submission, priority and then ID breaking a tie
// (gt-vh5psa).
func TestReviewPoolWorkerReviewWorksOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// Given newest first with the P0 in the middle, so the input order is
	// neither the landing order nor its reverse.
	issues := []*beads.Issue{
		readyIssue("gt-late", 2, "2026-09-30T13:00:00Z", "2026-09-30T13:00:00Z"),
		readyIssue("gt-hot", 0, "2026-09-30T11:00:00Z", "2026-09-30T11:00:00Z"),
		readyIssue("gt-tie-b", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
		readyIssue("gt-tie-a", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
		readyIssue("gt-old", 3, "2026-09-30T09:00:00Z", "2026-09-30T09:00:00Z"),
	}

	works, err := h.w.reviewWorks(issues, "")
	if err != nil {
		t.Fatalf("reviewWorks: %v", err)
	}
	if got, want := workOrder(works), "gt-hot,gt-old,gt-tie-a,gt-tie-b,gt-late"; got != want {
		t.Fatalf("reviewWorks order = %s; want %s", got, want)
	}
}

func workOrder(works []land.Work) string {
	ids := make([]string, 0, len(works))
	for _, w := range works {
		ids = append(ids, w.BeadID)
	}
	return strings.Join(ids, ",")
}

// TestReviewPoolWorkerReviewWorksLeavesOutNotReady pins that reviewWorks
// offers no work for an issue the pool must not review ahead: one whose status
// is not actionable, one that lacks the ready label, and the bead named by the
// landing parameter, which is being landed now (gt-iyvkxm).
func TestReviewPoolWorkerReviewWorksLeavesOutNotReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		status     string
		readyLabel bool
		landing    string
	}{
		{name: "not actionable", status: string(beads.StatusDeferred), readyLabel: true},
		{name: "no ready label", readyLabel: false},
		{name: "the bead being landed", readyLabel: true, landing: "gt-dropped"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			kept := readySubmission("gt-kept", "polecat/opal/gt-kept+x1", noteHead)
			dropped := readySubmission("gt-dropped", "polecat/opal/gt-dropped+x1", tipHead)
			dropped.Status = tc.status
			if !tc.readyLabel {
				dropped.Labels = nil
			}

			works, err := h.w.reviewWorks([]*beads.Issue{kept, dropped}, tc.landing)
			if err != nil {
				t.Fatalf("reviewWorks: %v", err)
			}
			if got, want := workOrder(works), "gt-kept"; got != want {
				t.Fatalf("reviewWorks offered %s; want only %s, leaving out %s", got, want, dropped.ID)
			}
		})
	}
}

// TestReviewPoolWorkerReviewWorksLeavesOutParked pins that reviewWorks offers
// no work for an issue the worker's own state holds back from landing: one
// parked on an infrastructure failure, one held by its last outcome, and one
// whose landing record is waiting to be repaired. A review now is spent on a
// bead the pass will not reach for a while (gt-u8s6m7).
func TestReviewPoolWorkerReviewWorksLeavesOutParked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hold func(h *harness, id string)
	}{
		{name: "parked", hold: func(h *harness, id string) {
			st := h.w.bead(id)
			st.parked, st.until = true, h.w.now().Add(time.Hour)
		}},
		{name: "held by its last outcome", hold: func(h *harness, id string) {
			h.w.outcomeMemo().record(id, seededRowAt, h.w.now().Add(time.Hour))
		}},
		{name: "pending repair", hold: func(h *harness, id string) {
			h.w.pendingRepair = map[string]land.Work{id: {BeadID: id, Branch: branch, Head: noteHead}}
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			kept := readySubmission("gt-kept", "polecat/opal/gt-kept+x1", noteHead)
			dropped := readySubmission("gt-dropped", "polecat/opal/gt-dropped+x1", tipHead)
			// The held row keys its hold on the list row's updated_at.
			dropped.UpdatedAt = seededRowAt
			tc.hold(h, dropped.ID)

			works, err := h.w.reviewWorks([]*beads.Issue{kept, dropped}, "")
			if err != nil {
				t.Fatalf("reviewWorks: %v", err)
			}
			if got, want := workOrder(works), "gt-kept"; got != want {
				t.Fatalf("reviewWorks offered %s for a %s bead; want only %s, leaving out %s", got, tc.name, want, dropped.ID)
			}
		})
	}
}

// TestReviewPoolWorkerReviewWorksReturnsUnreadableSubmission pins that an
// issue whose comments cannot be read fails the offer with that read error
// instead of vanishing from the list: what such a bead asks for is unknown,
// never absent (gt-uuvtbv).
func TestReviewPoolWorkerReviewWorksReturnsUnreadableSubmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	readable := readySubmission("gt-first", "polecat/opal/gt-first+x1", noteHead)
	// gt-unreadable carries no READY TO LAND block and is not in the store,
	// so the comments read fails.
	unreadable := &beads.Issue{ID: "gt-unreadable", Labels: []string{land.LabelReadyToLand}}

	works, err := h.w.reviewWorks([]*beads.Issue{readable, unreadable}, "")
	if err == nil {
		t.Fatalf("reviewWorks returned %d works and no error for an issue whose comments cannot be read", len(works))
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("reviewWorks error = %v; want the store's not-found error", err)
	}
	if !strings.Contains(err.Error(), "gt-unreadable") {
		t.Fatalf("reviewWorks error = %v; want it to name gt-unreadable", err)
	}
}

// TestReviewPoolWorkerReviewWorksSkipsUnresolvable pins that an issue whose
// comments read cleanly but name no submission (no READY TO LAND block, no
// submission comment) is left out of the offer with one log line naming it,
// while the other issues are still offered in landing order; a comments read
// that fails is still returned as an error with no works, since what such a
// bead asks for is unknown rather than absent (gt-p3qyg3).
func TestReviewPoolWorkerReviewWorksSkipsUnresolvable(t *testing.T) {
	t.Parallel()

	t.Run("skips the bead that names no submission", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		var logs []string
		h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
		first := readyIssue("gt-first", 0, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z")
		last := readyIssue("gt-last", 0, "2026-09-30T12:00:00Z", "2026-09-30T12:00:00Z")
		// gt-bare sits between them by submission time and names nothing: it is
		// seeded so its comments read cleanly, with no READY TO LAND block.
		bare := beads.Issue{ID: "gt-bare", Title: "work", Status: "hooked", Type: "task",
			Labels: []string{land.LabelReadyToLand}, UpdatedAt: "2026-09-30T11:00:00Z"}
		h.bd.Seed(bare)

		works, err := h.w.reviewWorks([]*beads.Issue{last, &bare, first}, "")
		if err != nil {
			t.Fatalf("reviewWorks: %v: an issue naming no submission is left out, not an error", err)
		}
		if got, want := workOrder(works), "gt-first,gt-last"; got != want {
			t.Fatalf("reviewWorks offered %s; want %s, leaving out gt-bare", got, want)
		}
		var named []string
		for _, line := range logs {
			if strings.Contains(line, "gt-bare") {
				named = append(named, line)
			}
		}
		if len(named) != 1 {
			t.Fatalf("reviewWorks logged %d lines naming gt-bare; want exactly one: %v", len(named), logs)
		}
	})

	t.Run("returns the error for comments that cannot be read", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first := readyIssue("gt-first", 0, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z")
		last := readyIssue("gt-last", 0, "2026-09-30T12:00:00Z", "2026-09-30T12:00:00Z")
		// gt-unreadable is not in the store, so its comments read fails; it sorts
		// between the two by submission time.
		unreadable := &beads.Issue{ID: "gt-unreadable", Labels: []string{land.LabelReadyToLand},
			UpdatedAt: "2026-09-30T11:00:00Z"}

		works, err := h.w.reviewWorks([]*beads.Issue{first, unreadable, last}, "")
		if err == nil {
			t.Fatalf("reviewWorks returned %d works and no error for an issue whose comments cannot be read", len(works))
		}
		if !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("reviewWorks error = %v; want the store's not-found error", err)
		}
		if !strings.Contains(err.Error(), "gt-unreadable") {
			t.Fatalf("reviewWorks error = %v; want it to name gt-unreadable", err)
		}
		if len(works) != 0 {
			t.Fatalf("reviewWorks returned %d works with the error; want none, so an unreadable bead never makes a shorter list", len(works))
		}
	})
}

// TestLandingWorkerParksABeadWhoseCommentsCannotBeRead pins that process parks
// a ready bead whose submission cannot be read as an infrastructure failure
// instead of reporting it skipped: an unknown submission is never read as one
// that names nothing (gt-uuvtbv).
func TestLandingWorkerParksABeadWhoseCommentsCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	issue := &beads.Issue{ID: "gt-unreadable", Title: "work", Status: "hooked", Type: "task",
		Labels: []string{land.LabelReadyToLand}}
	h.bd.Seed(*issue)
	h.w.Beads = unreadableComments{h.bd}

	rep := Report{}
	h.w.process(context.Background(), issue, &rep)

	if want := (Report{Failed: 1}); rep != want {
		t.Fatalf("process report = %+v; want %+v: an unreadable submission is parked, not skipped", rep, want)
	}
}

// TestReviewPoolWorkerProcessWithStoredOnly pins processWith's stored-only pass
// (gt-t3fgce): with storedOnly true the landing carries StoredOnly, so Land
// consumes the bead's stored review, and the recorded-landing repair is left to
// the ready pass even though the landings file holds a record the remote
// contains; processWith writes no outcome memo, so a bead its landing left held
// is read again by the next ready pass. With storedOnly false the landing
// carries StoredOnly false and nothing about today's call changes.
func TestReviewPoolWorkerProcessWithStoredOnly(t *testing.T) {
	t.Parallel()

	// repairRecord is a landing of the seeded bead the remote holds: the ready
	// pass repairs such a bead instead of landing it again.
	repairRecord := func(h *harness) {
		h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Rig: "gastown", Branch: branch,
			Head: "a932cb5a", Target: "main", LandedCommit: "6db29a35"}}
		h.remote.contains["6db29a35"] = true
	}

	t.Run("stored only leaves the recorded landing alone", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.seedReady(t, "gt-abc")
		repairRecord(h)
		issue, err := h.bd.Show("gt-abc")
		if err != nil {
			t.Fatalf("reading the seeded bead: %v", err)
		}

		rep := Report{}
		h.w.processWith(context.Background(), issue, &rep, true)

		if len(h.lander.calls) != 1 {
			t.Fatalf("landed %d times; want once: a stored-only pass lands the bead's submission", len(h.lander.calls))
		}
		got := h.lander.calls[0]
		if !got.StoredOnly {
			t.Fatalf("the landing carries StoredOnly false; want true, so Land consumes the stored review")
		}
		if got.Head != tipHead {
			t.Fatalf("the landing took head %s; want the submitted tip %s: a stored-only pass leaves the earlier landing's record to the ready pass", got.Head, tipHead)
		}
		if len(h.w.pendingRepair) != 0 {
			t.Fatalf("the pass left %d pending repairs; want none: the repair branch is the ready pass's", len(h.w.pendingRepair))
		}
		if want := (Report{Landed: 1}); rep != want {
			t.Fatalf("report = %+v; want %+v", rep, want)
		}
	})

	t.Run("stored only writes no outcome memo", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.seedReady(t, "gt-abc")
		// A landing that leaves the bead held: the ready pass records that hold
		// in the outcome memo, and a stored-only pass must not.
		h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, land.ErrNotReady }
		issue, err := h.bd.Show("gt-abc")
		if err != nil {
			t.Fatalf("reading the seeded bead: %v", err)
		}

		rep := Report{}
		h.w.processWith(context.Background(), issue, &rep, true)

		if want := (Report{Skipped: 1}); rep != want {
			t.Fatalf("report = %+v; want %+v", rep, want)
		}
		if h.w.outcomeMemo().held("gt-abc", issue.UpdatedAt) {
			t.Fatalf("the pass recorded an outcome memo for gt-abc; want none: the memo is landReady's")
		}
	})

	t.Run("not stored only carries StoredOnly false", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.seedReady(t, "gt-abc")
		issue, err := h.bd.Show("gt-abc")
		if err != nil {
			t.Fatalf("reading the seeded bead: %v", err)
		}

		rep := Report{}
		h.w.processWith(context.Background(), issue, &rep, false)

		if len(h.lander.calls) != 1 {
			t.Fatalf("landed %d times; want once", len(h.lander.calls))
		}
		if h.lander.calls[0].StoredOnly {
			t.Fatalf("the landing carries StoredOnly true; want false for today's process")
		}
		if want := (Report{Landed: 1}); rep != want {
			t.Fatalf("report = %+v; want %+v", rep, want)
		}
	})
}

// TestReviewPoolWorkerNoStoredIsNotInfra pins that a stored-only pass which
// finds no stored verdict to consume ends like the answer it is, not like an
// infrastructure failure (gt-ixvxt5): the bead is not parked, the pass counts,
// writes and backs off nothing, and the CI-silence run the ready pass owns
// survives. Absence of a verdict also arrives wrapped, and an unrelated error
// still parks the bead, so the new case consumes only its own sentinel.
func TestReviewPoolWorkerNoStoredIsNotInfra(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no stored verdict", land.ErrNoStoredVerdict},
		{"wrapped no stored verdict", fmt.Errorf("stored-only pass: %w", land.ErrNoStoredVerdict)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedReady(t, "gt-abc")
			h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, tc.err }
			issue, err := h.bd.Show("gt-abc")
			if err != nil {
				t.Fatalf("reading the seeded bead: %v", err)
			}
			// A run the ready pass has already counted on this bead: a pass
			// that consumed nothing did not answer a silence, so it must not
			// clear the run that did.
			h.w.bead("gt-abc").ciSilences = 2

			rep := Report{}
			h.w.processWith(context.Background(), issue, &rep, true)

			if want := (Report{}); rep != want {
				t.Fatalf("report = %+v; want %+v: nothing to consume counts nothing", rep, want)
			}
			if h.w.parked("gt-abc") {
				t.Fatalf("the bead is parked; want it left at its ordinary turn")
			}
			st := h.w.state["gt-abc"]
			if st == nil {
				t.Fatalf("the pass left no state for gt-abc; want its state untouched")
			}
			if !st.until.IsZero() {
				t.Fatalf("the pass set a backoff until %v; want none", st.until)
			}
			if st.failures != 0 {
				t.Fatalf("the pass counted %d failures; want none", st.failures)
			}
			if st.ciSilences != 2 {
				t.Fatalf("ciSilences = %d; want the ready pass's 2 left alone", st.ciSilences)
			}
			if len(h.cleared) != 0 {
				t.Fatalf("intent cleared for %v; want it kept for the bead's ordinary turn", h.cleared)
			}
			if len(h.w.pendingRepair) != 0 {
				t.Fatalf("the pass queued %d repairs; want none", len(h.w.pendingRepair))
			}
			if got := h.comments(t, "gt-abc"); len(got) != 0 {
				t.Fatalf("the pass wrote %d comments (%v); want none", len(got), got)
			}
			if len(h.lander.calls) != 1 || !h.lander.calls[0].StoredOnly {
				t.Fatalf("landings = %+v; want one stored-only landing", h.lander.calls)
			}
		})
	}

	t.Run("another error is still infrastructure", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.seedReady(t, "gt-abc")
		h.lander.fn = func(int, land.Work) (land.Result, error) {
			return land.Result{}, &land.InfraError{Stage: "gate", Err: errors.New("boom")}
		}
		issue, err := h.bd.Show("gt-abc")
		if err != nil {
			t.Fatalf("reading the seeded bead: %v", err)
		}

		rep := Report{}
		h.w.processWith(context.Background(), issue, &rep, true)

		if want := (Report{Failed: 1}); rep != want {
			t.Fatalf("report = %+v; want %+v", rep, want)
		}
		if !h.w.parked("gt-abc") {
			t.Fatalf("the bead is not parked; want an unrelated error still parked")
		}
	})
}

// TestReviewPoolWorkerPoolDefaultsNil pins that the harness Worker leaves Pool
// nil and that a pass over two ready beads still lands both with today's
// report: the field is storage for later beads, and a nil pool changes nothing
// (gt-8vfrwi).
func TestReviewPoolWorkerPoolDefaultsNil(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Pool != nil {
		t.Fatalf("harness Worker Pool = %#v, want nil", h.w.Pool)
	}
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	rep := h.w.Pass(context.Background())
	if want := (Report{Landed: 2}); rep != want {
		t.Fatalf("pass report = %+v, want %+v", rep, want)
	}
}

// TestReviewPoolWorkerStoredDefaultsNil pins that the harness Worker leaves
// Stored nil and that a pass over two ready beads still lands both with
// today's report: the field is storage for later beads, and a nil hint source
// changes nothing (gt-5q8ayw).
func TestReviewPoolWorkerStoredDefaultsNil(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Stored != nil {
		t.Fatalf("harness Worker Stored = %#v, want nil", h.w.Stored)
	}
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	rep := h.w.Pass(context.Background())
	if want := (Report{Landed: 2}); rep != want {
		t.Fatalf("pass report = %+v, want %+v", rep, want)
	}
}

// poolReviewer is the reviewer these tests' pool reviews through: it records
// each work it is asked for and the context that review runs under, and holds
// the review open until the test releases it, so a review can be seen still
// running while the pass runs. instant ends each review as it enters instead: a
// pass waits for the review of the bead it lands, so a test whose pass must land
// reviewed beads needs reviews that end rather than hold it (gt-m9gfv9). Each
// wait is bounded by the test's own context, so a review never outlives the
// test that made it.
type poolReviewer struct {
	hold    chan struct{}
	bound   <-chan struct{}
	once    sync.Once
	entered chan land.Work
	instant bool

	mu    sync.Mutex
	works []land.Work
	ctxs  []context.Context
}

func newPoolReviewer(bound context.Context) *poolReviewer {
	return &poolReviewer{hold: make(chan struct{}), bound: bound.Done(), entered: make(chan land.Work, 8)}
}

func (r *poolReviewer) ReviewAhead(ctx context.Context, w land.Work) (land.AheadOutcome, error) {
	r.mu.Lock()
	r.works = append(r.works, w)
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
	r.entered <- w
	if !r.instant {
		select {
		case <-r.hold:
		case <-r.bound:
		}
	}
	return land.AheadSkipped, nil
}

// release lets every held review return; the test cleanup calls it too, so a
// test that fails before releasing them leaves no review running.
func (r *poolReviewer) release() { r.once.Do(func() { close(r.hold) }) }

// reviewed returns the works the reviewer was asked for, in the order its
// reviews entered it, which Go does not promise to be the offer's order.
func (r *poolReviewer) reviewed() []land.Work {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]land.Work(nil), r.works...)
}

// reviewCtxs returns the context each review runs under, one per reviewed work.
func (r *poolReviewer) reviewCtxs() []context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]context.Context(nil), r.ctxs...)
}

// reviewedSet names the reviewed beads sorted, so a test pins what was offered
// without depending on the order the reviews entered the reviewer.
func reviewedSet(works []land.Work) string {
	ids := make([]string, 0, len(works))
	for _, w := range works {
		ids = append(ids, w.BeadID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// commentsFailFor hides one bead's comments from a worker's store: reviewWorks
// and process get a read error for that bead alone, so a test can see one
// unreadable submission answered while the other beads still land.
type commentsFailFor struct {
	Beads
	id string
}

func (c commentsFailFor) Comments(id string) ([]beads.Comment, error) {
	if id == c.id {
		return nil, beads.ErrNotFound
	}
	return c.Beads.Comments(id)
}

// TestReviewPoolWorkerOffersAfterList pins that a pass hands the review pool
// the ready beads it just listed, once, in the landing order the pass will take
// them, under the context the pass runs on: the reviews run on that context, not
// on a landing's, so a verdict outlives the landing it was made for. The pass
// waits for each bead's own review before landing it (gt-m9gfv9), which is why
// the pass here runs on its own goroutine while the test holds the reviews. A
// works error offers nothing and never stops the pass landing the beads it can
// (gt-mdv8eq).
func TestReviewPoolWorkerOffersAfterList(t *testing.T) {
	t.Parallel()

	t.Run("the listed beads are offered in landing order", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// Seeded newest first, so the list's order is not the landing order:
		// the P0 leads whatever the store returns.
		h.seedReadyAt(t, "gt-late", 2, "2026-09-30T13:00:00Z")
		h.seedReadyAt(t, "gt-hot", 0, "2026-09-30T11:00:00Z")

		bound := waitCtx(t)
		rev := newPoolReviewer(bound)
		t.Cleanup(rev.release)
		// Hints is asked for each work a start would review, in the order the
		// pool is given them, so it records what the worker offered and in what
		// order; the reviewer's own entry order belongs to Go's scheduler.
		hints := &fakeAheadHints{}
		pool := &ReviewPool{Reviewer: rev, Hints: hints, Max: 2}
		h.w.Pool = pool

		// The pass waits for gt-hot's review before landing it, so it runs on its
		// own goroutine while this test holds both reviews open (gt-m9gfv9).
		done := make(chan Report, 1)
		go func() { done <- h.w.Pass(context.Background()) }()
		for n := 0; n < 2; n++ {
			recvBounded(t, bound, rev.entered, "a review started from the first list")
		}
		// A review runs under the context the pass was called with, which the
		// pass does not cancel while it runs and a landing does not cancel: both
		// are alive while the pass waits for the bead it is about to land.
		for i, rctx := range rev.reviewCtxs() {
			if err := rctx.Err(); err != nil {
				t.Fatalf("review %d is done (%v) while the pass still runs; want the pass's run context, which outlives a landing", i, err)
			}
		}

		rev.release()
		rep := recvBounded(t, bound, done, "the pass returning once the reviews ended")
		if want := (Report{Landed: 2}); rep != want {
			t.Fatalf("pass report = %+v; want %+v", rep, want)
		}
		if got, want := strings.Join(hints.askedBeads(), ","), "gt-hot,gt-late"; got != want {
			t.Fatalf("the pool was offered %q; want the landing order %q", got, want)
		}
		if got, want := reviewedSet(rev.reviewed()), "gt-hot,gt-late"; got != want {
			t.Fatalf("the reviewer saw %q; want both ready beads %q", got, want)
		}
		recvBounded(t, bound, idleOf(pool), "the pool going idle once the reviews are released")
	})

	t.Run("a works error offers nothing and the pass still lands", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		var logs []string
		h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
		// gt-bad sorts first and names no submission, so building the works
		// fails on it before any work is offered.
		h.bd.Seed(beads.Issue{ID: "gt-bad", Title: "work", Status: "hooked", Type: "task",
			Priority: 0, Labels: []string{land.LabelReadyToLand}, UpdatedAt: seededRowAt})
		h.seedReadyAt(t, "gt-good", 2, "2026-09-30T13:00:00Z")
		h.w.Beads = commentsFailFor{Beads: h.w.Beads, id: "gt-bad"}

		// The real Land takes the ready label off. Without that, gt-good would
		// be listed and offered again after it lands, and that review would
		// race the count below (gt-hlsdiw).
		h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
			if err := h.bd.Update(work.BeadID, beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
				t.Errorf("removing %s from %s: %v", land.LabelReadyToLand, work.BeadID, err)
			}
			return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
		}

		rev := newPoolReviewer(waitCtx(t))
		t.Cleanup(rev.release)
		h.w.Pool = &ReviewPool{Reviewer: rev, Max: 2}

		rep := h.w.Pass(context.Background())
		if got := len(h.lander.calls); got != 1 || h.lander.calls[0].BeadID != "gt-good" {
			t.Fatalf("landed %+v; want gt-good alone: a works error offers no review but never stops the pass", h.lander.calls)
		}
		if want := (Report{Landed: 1, Failed: 1}); rep != want {
			t.Fatalf("pass report = %+v; want %+v", rep, want)
		}
		if got := len(rev.reviewed()); got != 0 {
			t.Fatalf("the reviewer was asked for %d works over a works error; want none", got)
		}
		var said []string
		for _, line := range logs {
			if strings.Contains(line, "the review pool") {
				said = append(said, line)
			}
		}
		if len(said) != 1 {
			t.Fatalf("the works error was logged on %d lines; want exactly one: %v", len(said), logs)
		}
	})
}

// TestReviewPoolWorkerOffersAfterRelist pins that the pass offers the ready
// board to the pool again after each re-list, leaving out the bead about to be
// landed: a bead made ready while a landing ran is reviewed, while the bead the
// fresh list is about to land is not (gt-20kjw5). The reviews end as they enter,
// since the pass waits for the review of the bead it lands (gt-m9gfv9).
func TestReviewPoolWorkerOffersAfterRelist(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-first", 2, "2026-09-30T10:00:00Z")
	// The landing removes the ready label, as the real one does; the fake
	// lander leaves it, and the re-list would keep returning the landed bead.
	h.lander.fn = func(n int, work land.Work) (land.Result, error) {
		if n == 1 {
			// gt-second is the bead the next re-list will land, gt-mid one the
			// operator made ready while the first landing ran.
			h.seedReadyAt(t, "gt-second", 2, "2026-09-30T10:01:00Z")
			h.seedReadyAt(t, "gt-mid", 2, "2026-09-30T10:02:00Z")
		}
		if err := h.bd.Update(work.BeadID, beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
			t.Fatalf("removing %s from %s: %v", land.LabelReadyToLand, work.BeadID, err)
		}
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}

	bound := waitCtx(t)
	rev := newPoolReviewer(bound)
	rev.instant = true
	t.Cleanup(rev.release)
	pool := &ReviewPool{Reviewer: rev, Max: 2}
	h.w.Pool = pool

	rep := h.w.Pass(context.Background())
	if want := (Report{Landed: 3}); rep != want {
		t.Fatalf("pass report = %+v; want %+v", rep, want)
	}
	if got, want := landedOrder(h.lander.calls), "gt-first,gt-second,gt-mid"; got != want {
		t.Fatalf("landed %s; want %s", got, want)
	}
	rev.release()
	recvBounded(t, bound, idleOf(pool), "the pool going idle once the reviews are released")
	if got, want := reviewedSet(rev.reviewed()), "gt-first,gt-mid"; got != want {
		t.Fatalf("the pool reviewed %q; want %q: the re-list offers the bead made ready mid-pass and leaves out the one it is about to land", got, want)
	}
}

// TestReviewPoolWorkerAwaitsBeforeLanding pins that the landing waits for the
// review pool's review of the submission it is about to land, the head the
// bead's own READY TO LAND block names: with the review held, the pass has
// neither finished nor called Land, and once the review ends the pass lands the
// bead. A bead the pass cannot resolve — one whose comments cannot be read, or
// one naming no submission — is not waited for, and process reports it as it
// did before the pool existed (gt-m9gfv9).
func TestReviewPoolWorkerAwaitsBeforeLanding(t *testing.T) {
	t.Parallel()

	t.Run("the landing waits for the bead's review", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			// settle is synctest's barrier, aliased because the presubmit wait check reads a direct call as unbounded (gt-10cacr).
			settle := synctest.Wait
			h := newHarness(t)
			h.seedReady(t, "gt-first")

			bound := waitCtx(t)
			rev := newPoolReviewer(bound)
			t.Cleanup(rev.release)
			h.w.Pool = &ReviewPool{Reviewer: rev, Max: 1}

			done := make(chan Report, 1)
			go func() { done <- h.w.Pass(context.Background()) }()

			recvBounded(t, bound, rev.entered, "the review of gt-first starting")
			// The offer has started the review and the pass has reached the bead,
			// so with the review still held the pass is blocked in Await when the
			// bubble settles.
			settle()
			select {
			case rep := <-done:
				t.Fatalf("the pass finished (%+v) while gt-first's review was still running; want it to wait for the verdict", rep)
			default:
			}
			if got := landedOrder(h.lander.calls); got != "" {
				t.Fatalf("landed %s while gt-first's review was still running; want no landing until the review ends", got)
			}

			rev.release()
			rep := recvBounded(t, bound, done, "the pass returning once the review ended")
			if want := (Report{Landed: 1}); rep != want {
				t.Fatalf("pass report = %+v; want %+v", rep, want)
			}
		})
	})

	t.Run("a bead the pass cannot resolve is not waited for", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name       string
			unreadable bool
			want       Report
		}{
			{name: "names no submission", want: Report{Skipped: 1}},
			{name: "comments cannot be read", unreadable: true, want: Report{Failed: 1}},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				h := newHarness(t)
				// gt-bare carries the ready label and no READY TO LAND block, so
				// resolveWork finds no submission for it.
				h.bd.Seed(beads.Issue{ID: "gt-bare", Title: "work", Status: "hooked", Type: "task",
					Labels: []string{land.LabelReadyToLand}, UpdatedAt: seededRowAt})
				if tc.unreadable {
					h.w.Beads = commentsFailFor{Beads: h.w.Beads, id: "gt-bare"}
				}
				rev := newPoolReviewer(waitCtx(t))
				t.Cleanup(rev.release)
				h.w.Pool = &ReviewPool{Reviewer: rev, Max: 1}

				rep := h.w.Pass(context.Background())
				if rep != tc.want {
					t.Fatalf("pass report = %+v; want %+v: process handles a bead the pass cannot resolve as it did before the pool", rep, tc.want)
				}
				if got := landedOrder(h.lander.calls); got != "" {
					t.Fatalf("landed %s; want none", got)
				}
				if got := reviewedSet(rev.reviewed()); got != "" {
					t.Fatalf("the reviewer was asked for %q; want nothing: a bead with no resolved submission is not reviewed ahead", got)
				}
			})
		}
	})
}

// TestReviewPoolWorkerAwaitEndsOnCancel pins that a pass whose context is
// cancelled while it waits for a review ends the wait and returns without
// landing the bead: the daemon stopped, so nothing new is started. It runs in a
// synctest bubble, whose barrier shows the pass blocked in the wait before the
// cancel (gt-m9gfv9).
func TestReviewPoolWorkerAwaitEndsOnCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// settle is synctest's barrier, aliased because the presubmit wait check reads a direct call as unbounded (gt-10cacr).
		settle := synctest.Wait
		h := newHarness(t)
		h.seedReady(t, "gt-first")

		// The review's hold is bound by the pass context too, so the cancel that
		// ends the wait ends the review with it.
		passCtx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		rev := newPoolReviewer(passCtx)
		t.Cleanup(rev.release)
		h.w.Pool = &ReviewPool{Reviewer: rev, Max: 1}

		bound := waitCtx(t)
		done := make(chan Report, 1)
		go func() { done <- h.w.Pass(passCtx) }()

		recvBounded(t, bound, rev.entered, "the review of gt-first starting")
		settle()
		select {
		case rep := <-done:
			t.Fatalf("the pass finished (%+v) before the context was cancelled; want it waiting for the review", rep)
		default:
		}

		cancel()
		recvBounded(t, bound, done, "the pass returning once its context is cancelled")
		if got := landedOrder(h.lander.calls); got != "" {
			t.Fatalf("landed %s after its context was cancelled; want none", got)
		}
	})
}

// TestReviewPoolWorkerNilPoolIsToday pins that a worker with no review pool
// passes exactly as it did before the pool existed: a pass over three ready
// beads does not panic, lands them in the landing order (a P0 first, then the
// earliest submission) with the landing requests written out here, and reports
// three landed (gt-mxy9yb).
func TestReviewPoolWorkerNilPoolIsToday(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Pool != nil {
		t.Fatalf("Worker Pool = %#v; want nil: this test pins the pass with no pool set", h.w.Pool)
	}
	// Seeded in neither landing order nor its reverse: the P0 comes last and
	// the two older submissions arrive out of order.
	h.seedReadyAt(t, "gt-late", 2, "2026-09-30T11:00:00Z")
	h.seedReadyAt(t, "gt-hot", 0, "2026-09-30T13:00:00Z")
	h.seedReadyAt(t, "gt-early", 2, "2026-09-30T09:00:00Z")

	rep := h.w.Pass(context.Background())

	if want := (Report{Landed: 3}); rep != want {
		t.Fatalf("pass report = %+v; want %+v", rep, want)
	}
	want := []struct {
		bead      string
		rig       string
		branch    string
		head      string
		target    string
		worker    string
		priority  int
		submitted string
	}{
		{bead: "gt-hot", rig: "gastown", branch: branch, head: tipHead, target: "main", worker: "opal", priority: 0, submitted: "2026-09-30T13:00:00Z"},
		{bead: "gt-early", rig: "gastown", branch: branch, head: tipHead, target: "main", worker: "opal", priority: 2, submitted: "2026-09-30T09:00:00Z"},
		{bead: "gt-late", rig: "gastown", branch: branch, head: tipHead, target: "main", worker: "opal", priority: 2, submitted: "2026-09-30T11:00:00Z"},
	}
	if len(h.lander.calls) != len(want) {
		t.Fatalf("landed %d beads; want %d: %+v", len(h.lander.calls), len(want), h.lander.calls)
	}
	for i, want := range want {
		got := h.lander.calls[i]
		if got.BeadID != want.bead || got.Rig != want.rig || got.Branch != want.branch ||
			got.Head != want.head || got.Target != want.target || got.Worker != want.worker {
			t.Fatalf("landing %d = %+v; want bead %s of %s at %s onto %s by %s in %s",
				i, got, want.bead, want.branch, want.head, want.target, want.worker, want.rig)
		}
		if got.Priority == nil || *got.Priority != want.priority {
			t.Fatalf("landing %d priority = %v; want %d", i, got.Priority, want.priority)
		}
		submitted, err := time.Parse(time.RFC3339, want.submitted)
		if err != nil {
			t.Fatalf("parsing the expected submission time %q: %v", want.submitted, err)
		}
		if !got.Submitted.Equal(submitted) {
			t.Fatalf("landing %d submitted = %v; want %s", i, got.Submitted, want.submitted)
		}
	}
}

// sweepHints is the Stored hint source these tests install: a bead in rejecting
// is hinted rejecting, a bead in fail answers that error, and other beads are
// not rejected. rejecting may change while a pass runs.
type sweepHints struct {
	rejecting map[string]bool
	fail      map[string]error
}

func (s *sweepHints) Hint(bead string) (string, bool, error) {
	if err := s.fail[bead]; err != nil {
		return "", false, err
	}
	return "", s.rejecting[bead], nil
}

// headSweepHints is a Stored hint source whose review for a bead names the next
// head each time it is read, so a record the pool replaced mid-pass looks like
// one: the last head repeats once the sequence runs out. A bead with no
// sequence has no stored review.
type headSweepHints struct {
	heads map[string][]string
	reads map[string]int
}

func (s *headSweepHints) Hint(bead string) (string, bool, error) {
	seq := s.heads[bead]
	if len(seq) == 0 {
		return "", false, nil
	}
	i := s.reads[bead]
	if i >= len(seq) {
		i = len(seq) - 1
	}
	s.reads[bead] = i + 1
	return seq[i], true, nil
}

// landingCalls names each Land call as bead:stored-only, in call order.
func landingCalls(calls []land.Work) string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, fmt.Sprintf("%s:%v", call.BeadID, call.StoredOnly))
	}
	return strings.Join(out, ",")
}

// TestReviewPoolWorkerSweepsRejectionsFirst pins that the pass consumes a ready
// bead's stored rejection before it lands the beads queued ahead of it: the
// third bead's stored-only landing comes first (gt-9nco9r).
func TestReviewPoolWorkerSweepsRejectionsFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-first", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-second", 2, "2026-09-30T11:00:00Z")
	h.seedReadyAt(t, "gt-third", 2, "2026-09-30T12:00:00Z")
	h.w.Stored = &sweepHints{rejecting: map[string]bool{"gt-third": true}}
	h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
		if !work.StoredOnly {
			return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
		}
		// The real rejection sets the bead open and takes the label off.
		if err := h.bd.Update(work.BeadID, beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
			t.Fatalf("removing %s from %s: %v", land.LabelReadyToLand, work.BeadID, err)
		}
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Reason: "om asked for changes", Rework: true}
	}

	h.w.Pass(context.Background())

	if got, want := landingCalls(h.lander.calls), "gt-third:true,gt-first:false,gt-second:false"; got != want {
		t.Fatalf("landings = %s; want %s: the stored rejection is consumed before the next landing and the order of ordinary landings is unchanged", got, want)
	}
}

// TestReviewPoolWorkerNilStoredIsToday pins that a worker with no hint source
// lands exactly as it did before the sweep existed: a pass over three ready
// beads makes the same ordinary Land calls as one whose hints reject
// nothing (gt-9nco9r).
func TestReviewPoolWorkerNilStoredIsToday(t *testing.T) {
	t.Parallel()
	run := func(hints bool) string {
		h := newHarness(t)
		if hints {
			h.w.Stored = &sweepHints{}
		}
		h.seedReadyAt(t, "gt-late", 2, "2026-09-30T11:00:00Z")
		h.seedReadyAt(t, "gt-hot", 0, "2026-09-30T13:00:00Z")
		h.seedReadyAt(t, "gt-early", 2, "2026-09-30T09:00:00Z")
		rep := h.w.Pass(context.Background())
		if want := (Report{Landed: 3}); rep != want {
			t.Fatalf("pass report = %+v; want %+v", rep, want)
		}
		return landingCalls(h.lander.calls)
	}

	nilStored, hinted := run(false), run(true)
	if nilStored != hinted {
		t.Fatalf("nil Stored landed %s; hints that reject nothing landed %s; want today's calls either way", nilStored, hinted)
	}
}

// TestReviewPoolWorkerSweepSkipsWhatTheLoopSkips pins that the sweep leaves a
// bead alone when the landing loop would: not actionable, carrying the retry
// label the loop clears, parked, a repair pending, its bookkeeping still queued,
// or held by its outcome memo. Each is hinted rejecting, so only the skip keeps
// it from a stored-only landing (gt-9nco9r).
func TestReviewPoolWorkerSweepSkipsWhatTheLoopSkips(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*beads.Issue)
		hold   func(*testing.T, *harness, string)
	}{
		{name: "not actionable", change: func(dropped *beads.Issue) {
			dropped.Status = string(beads.StatusDeferred)
		}},
		{name: "landing retry label", change: func(dropped *beads.Issue) {
			dropped.Labels = append(dropped.Labels, LabelLandingRetry)
		}},
		{name: "parked", hold: func(_ *testing.T, h *harness, id string) {
			st := h.w.bead(id)
			st.parked, st.until = true, h.w.now().Add(time.Hour)
		}},
		{name: "pending repair", hold: func(_ *testing.T, h *harness, id string) {
			h.w.pendingRepair = map[string]land.Work{id: {BeadID: id, Branch: branch, Head: noteHead, Target: "main", Worker: "opal"}}
			h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
				// A race leaves the repair queued, so the bead stays pending
				// through the sweep.
				if work.BeadID == id {
					return land.Result{}, &land.RaceError{}
				}
				return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
			}
		}},
		{name: "bookkeeping still queued", hold: func(t *testing.T, h *harness, id string) {
			ctx := waitCtx(t)
			q, release := holdQueue(ctx, t, id)
			h.w.Books = q
			t.Cleanup(func() { releaseBeadJob(ctx, t, q, id, release) })
		}},
		{name: "outcome memo held", hold: func(_ *testing.T, h *harness, id string) {
			h.w.outcomeMemo().record(id, seededRowAt, h.w.now().Add(time.Hour))
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedReadyAt(t, "gt-kept", 2, "2026-09-30T09:00:00Z")
			dropped := readySubmission("gt-dropped", branch, noteHead)
			dropped.UpdatedAt = seededRowAt
			if tc.change != nil {
				tc.change(dropped)
			}
			h.bd.Seed(*dropped)
			if tc.hold != nil {
				tc.hold(t, h, dropped.ID)
			}
			h.w.Stored = &sweepHints{rejecting: map[string]bool{"gt-dropped": true}}

			h.w.Pass(context.Background())

			kept := false
			for _, call := range h.lander.calls {
				if call.StoredOnly {
					t.Fatalf("swept %s stored-only; want it left alone: it is a bead the loop skips (%s)", call.BeadID, tc.name)
				}
				kept = kept || call.BeadID == "gt-kept"
			}
			if !kept {
				t.Fatalf("gt-kept did not land; the pass made %d calls: %+v", len(h.lander.calls), h.lander.calls)
			}
		})
	}
}

// TestReviewPoolWorkerSweepSkipsALabellessBead pins that the sweep leaves a
// bead alone once its ready label is gone. The list filters on the label, so
// the sweep is called directly with a bead a read before the label came off
// would hold (gt-9nco9r).
func TestReviewPoolWorkerSweepSkipsALabellessBead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	labelless := readySubmission("gt-abc", branch, noteHead)
	labelless.Labels = nil
	h.w.Stored = &sweepHints{rejecting: map[string]bool{"gt-abc": true}}

	rep := Report{}
	h.w.sweepStored(context.Background(), []*beads.Issue{labelless}, map[string]bool{}, map[string]bool{}, &rep)
	if got := landingCalls(h.lander.calls); got != "" {
		t.Fatalf("landed %s; want nothing: a bead with no ready label is not swept", got)
	}
	if want := (Report{}); rep != want {
		t.Fatalf("report = %+v; want %+v", rep, want)
	}
}

// TestReviewPoolWorkerSweepSkipsHandled pins that a bead this pass has already
// looked at is not swept in a later round when the store then hints it rejecting (gt-9nco9r).
func TestReviewPoolWorkerSweepSkipsHandled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	hints := &sweepHints{}
	h.w.Stored = hints
	// The bead's own attempt leaves it ready with nothing holding it, and the
	// rejection appears only after that attempt.
	h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
		hints.rejecting = map[string]bool{work.BeadID: true}
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Reason: "om asked for changes", Rework: true}
	}

	h.w.Pass(context.Background())

	if got, want := landingCalls(h.lander.calls), "gt-abc:false"; got != want {
		t.Fatalf("landings = %s; want %s: a bead the pass already looked at is not swept", got, want)
	}
}

// TestReviewPoolWorkerSweepRejectionIsNotTriedTwice pins that a bead the sweep
// rejected is not attempted again in the same pass while it stays ready: the
// fake lander removes no label, so only the handled record keeps the loop from
// landing the same rejected submission again (gt-9nco9r).
func TestReviewPoolWorkerSweepRejectionIsNotTriedTwice(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.Stored = &sweepHints{rejecting: map[string]bool{"gt-abc": true}}
	h.lander.fn = func(_ int, _ land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Reason: "om asked for changes", Rework: true}
	}

	rep := h.w.Pass(context.Background())

	if got, want := landingCalls(h.lander.calls), "gt-abc:true"; got != want {
		t.Fatalf("landings = %s; want %s: a bead the sweep rejected is not attempted again while it stays ready", got, want)
	}
	if rep.Rejected != 1 {
		t.Fatalf("pass report = %+v; want one rejection", rep)
	}
}

// TestReviewPoolWorkerSweepThenLandsAtItsTurn pins that a swept bead whose
// stored-only pass answers ErrNoStoredVerdict — the record no longer applies —
// is not marked handled: it takes its ordinary turn later in the same pass,
// with no backoff and no failure counted for it, and, though it stays ready and
// rejecting through three rounds, it is not swept a second time
// (gt-eyrufr, gt-ixvxt5, gt-9nco9r).
func TestReviewPoolWorkerSweepThenLandsAtItsTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-early", 2, "2026-09-30T09:00:00Z")
	h.seedReadyAt(t, "gt-mid", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-late", 2, "2026-09-30T11:00:00Z")
	h.w.Stored = &sweepHints{rejecting: map[string]bool{"gt-late": true}}
	// Read at the bead's ordinary turn: its landing succeeds, and a successful
	// landing ends the bead's failure run and drops its state, so a read after
	// the pass would prove nothing about what the sweep left behind.
	var (
		atTurnFailures int
		atTurnUntil    time.Time
	)
	h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
		if work.StoredOnly {
			return land.Result{}, land.ErrNoStoredVerdict
		}
		if work.BeadID == "gt-late" {
			if st := h.w.state["gt-late"]; st != nil {
				atTurnFailures, atTurnUntil = st.failures, st.until
			}
		}
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}

	h.w.Pass(context.Background())

	if got, want := landingCalls(h.lander.calls), "gt-late:true,gt-early:false,gt-mid:false,gt-late:false"; got != want {
		t.Fatalf("landings = %s; want %s: a stored-only pass with nothing to consume sweeps one bead and head once and leaves it its ordinary turn", got, want)
	}
	if atTurnFailures != 0 {
		t.Fatalf("the swept bead carried %d failures at its ordinary turn; want none: nothing was stored to consume", atTurnFailures)
	}
	if !atTurnUntil.IsZero() {
		t.Fatalf("the swept bead carried a backoff until %v at its ordinary turn; want none: nothing was stored to consume", atTurnUntil)
	}
}

// TestReviewPoolWorkerSweepsNewHeadOnce pins that the sweep follows the head the
// hint names: a bead unhandled after a stored-only attempt for head A is swept
// once more when the pool has replaced its record with one for head B, and not
// again for head B (gt-k5s380).
func TestReviewPoolWorkerSweepsNewHeadOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The rejecting bead is last in landing order, so the earlier beads' turns
	// give the pool time to replace its stored review mid-pass.
	h.seedReadyAt(t, "gt-early", 2, "2026-09-30T09:00:00Z")
	h.seedReadyAt(t, "gt-mid", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-abc", 2, "2026-09-30T11:00:00Z")
	h.w.Stored = &headSweepHints{
		heads: map[string][]string{"gt-abc": {noteHead, tipHead}},
		reads: map[string]int{},
	}
	h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
		if work.StoredOnly {
			return land.Result{}, land.ErrNoStoredVerdict
		}
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}

	h.w.Pass(context.Background())

	// Two stored-only attempts for gt-abc — one per head — then its ordinary
	// turn. Keyed by bead alone, the second head is skipped.
	if got, want := landingCalls(h.lander.calls), "gt-abc:true,gt-early:false,gt-abc:true,gt-mid:false,gt-abc:false"; got != want {
		t.Fatalf("landings = %s; want %s: a new head for an unhandled bead is swept once and the same head is not swept again", got, want)
	}
}

// TestReviewPoolWorkerSweepStopsOnDrain pins that a drain stops the sweep
// between two rejected beads and the pass with it: no landing after the drain (gt-9nco9r).
func TestReviewPoolWorkerSweepStopsOnDrain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-first", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-second", 2, "2026-09-30T11:00:00Z")
	h.w.Stored = &sweepHints{rejecting: map[string]bool{"gt-first": true, "gt-second": true}}
	draining := false
	h.w.Draining = func() bool { return draining }
	h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
		draining = true
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Reason: "om asked for changes", Rework: true}
	}

	h.w.Pass(context.Background())

	if len(h.lander.calls) != 1 || !h.lander.calls[0].StoredOnly {
		t.Fatalf("landings = %s; want one stored-only landing: a drain stops the sweep and the pass", landingCalls(h.lander.calls))
	}
}

// TestReviewPoolWorkerSweepRecordsOutcomeMemo pins that a swept landing leaves
// the outcome memo the loop leaves around an ordinary landing. A stored-only
// attempt that fails at read-back waits without parking the bead, so the memo is
// the only hold a later pass reads; with it, the bead stays ready and the second
// pass does not land it again (gt-9nco9r).
func TestReviewPoolWorkerSweepRecordsOutcomeMemo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.Stored = &sweepHints{rejecting: map[string]bool{"gt-abc": true}}
	h.lander.fn = func(_ int, _ land.Work) (land.Result, error) {
		return land.Result{}, &land.InfraError{Stage: "read-back", Err: fmt.Errorf("%w: tip is x", land.ErrReadBack)}
	}

	h.w.Pass(context.Background())
	h.w.Pass(context.Background())

	if got, want := landingCalls(h.lander.calls), "gt-abc:true"; got != want {
		t.Fatalf("landings = %s; want %s: the outcome memo holds the bead until its wait comes due", got, want)
	}
}

// TestReviewPoolWorkerHintErrorWaits pins that a hint that cannot be read is
// never read as "not rejecting": the error is logged in one line naming the
// bead, the bead gets no stored-only landing and keeps its ordinary turn, and
// the sweep goes on to the next hinted bead (gt-end632).
func TestReviewPoolWorkerHintErrorWaits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The list names the later seed first, so gt-bad heads the pass's list: the
	// sweep meets its unreadable hint before gt-other's rejection, and a sweep
	// that stops there never reaches the bead it must still sweep.
	h.seedReadyAt(t, "gt-other", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-bad", 2, "2026-09-30T09:00:00Z")
	h.w.Stored = &sweepHints{
		rejecting: map[string]bool{"gt-other": true},
		fail:      map[string]error{"gt-bad": errors.New("record unreadable")},
	}
	h.lander.fn = func(_ int, work land.Work) (land.Result, error) {
		if work.StoredOnly {
			return land.Result{}, &land.Rejection{Kind: land.RejectReview, Reason: "om asked for changes", Rework: true}
		}
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}
	var logs []string
	h.w.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	rep := h.w.Pass(context.Background())

	// The next hinted bead is swept stored-only, then gt-bad lands at its own
	// turn: an unreadable hint is no stored-only landing for gt-bad, and it does
	// not stop the sweep.
	if got, want := landingCalls(h.lander.calls), "gt-other:true,gt-bad:false"; got != want {
		t.Fatalf("landings = %s; want %s: the failing-hint bead keeps its turn and the sweep goes on", got, want)
	}
	if want := (Report{Landed: 1, Rejected: 1}); rep != want {
		t.Fatalf("pass report = %+v; want %+v", rep, want)
	}
	named := false
	for _, line := range logs {
		named = named || (strings.Contains(line, "gt-bad") && strings.Contains(line, "record unreadable"))
	}
	if !named {
		t.Fatalf("logs %q; want one naming gt-bad and the read error", logs)
	}
}
