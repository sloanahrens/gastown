package landworker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
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
// running after the pass that offered it has returned. Its one wait is bounded
// by the test's own context, so a review never outlives the test that made it.
type poolReviewer struct {
	hold    chan struct{}
	bound   <-chan struct{}
	once    sync.Once
	entered chan land.Work

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
	select {
	case <-r.hold:
	case <-r.bound:
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
// them, under the context the pass runs on: the reviews are still running after
// the pass and its landings have ended, and their context is not cancelled, so
// a verdict outlives a landing and a pass. A works error offers nothing and
// never stops the pass landing the beads it can (gt-mdv8eq).
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

		rep := h.w.Pass(context.Background())
		if want := (Report{Landed: 2}); rep != want {
			t.Fatalf("pass report = %+v; want %+v", rep, want)
		}
		for n := 0; n < 2; n++ {
			recvBounded(t, bound, rev.entered, "a review started from the first list")
		}
		if got, want := strings.Join(hints.askedBeads(), ","), "gt-hot,gt-late"; got != want {
			t.Fatalf("the pool was offered %q; want the landing order %q", got, want)
		}
		if got, want := reviewedSet(rev.reviewed()), "gt-hot,gt-late"; got != want {
			t.Fatalf("the reviewer saw %q; want both ready beads %q", got, want)
		}
		// The pass has landed both beads, so each landing has ended; a review
		// is still running and its context is alive, parented on the context
		// the pass ran under rather than on a landing's or the pass's own.
		for i, rctx := range rev.reviewCtxs() {
			if err := rctx.Err(); err != nil {
				t.Fatalf("review %d runs after the pass and its landings ended but its context is done (%v); want the pass's run context, which outlives both", i, err)
			}
		}
		rev.release()
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
