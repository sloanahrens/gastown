package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

func TestOutcomeMemoHoldsOnlyARowItCanProveIsUnchanged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	until := now.Add(humanWaitBackoff)
	for _, tc := range []struct {
		name  string
		entry memoEntry
		has   bool
		row   string
		at    time.Time
		want  bool
	}{
		{name: "the row its outcome left, inside the hold", entry: memoEntry{updatedAt: "r1", until: until}, has: true, row: "r1", at: now, want: true},
		{name: "the row its outcome left, once the hold is due", entry: memoEntry{updatedAt: "r1", until: until}, has: true, row: "r1", at: until, want: false},
		{name: "a row that moved", entry: memoEntry{updatedAt: "r1", until: until}, has: true, row: "r2", at: now, want: false},
		{name: "a row with no updated_at", entry: memoEntry{updatedAt: "", until: until}, has: true, row: "", at: now, want: false},
		{name: "a bead nothing was recorded for", has: false, row: "r1", at: now, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newOutcomeMemos(func() time.Time { return tc.at })
			if tc.has {
				m.entries["gt-abc"] = tc.entry
			}
			if got := m.held("gt-abc", tc.row); got != tc.want {
				t.Fatalf("held = %v; want %v", got, tc.want)
			}
		})
	}
}

// countingStore counts the bd calls a pass makes, by verb as the census does,
// and moves a bead's updated_at on a comment when bd would (gt-r9pj9).
type countingStore struct {
	*beadsfake.Fake
	bumpOnComment bool

	mu    sync.Mutex
	calls map[string]int
}

func newCountingStore() *countingStore {
	return &countingStore{Fake: beadsfake.New(beadsfake.WithPrefix("gt")), calls: map[string]int{}}
}

func (s *countingStore) count(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[op]++
}

// countOf is how many of ops the passes since the last reset called.
func (s *countingStore) countOf(ops ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, op := range ops {
		n += s.calls[op]
	}
	return n
}

func (s *countingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = map[string]int{}
}

func (s *countingStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	s.count("List")
	return s.Fake.List(opts)
}

func (s *countingStore) Show(id string) (*beads.Issue, error) {
	s.count("Show")
	return s.Fake.Show(id)
}

func (s *countingStore) Children(id string) ([]*beads.Issue, error) {
	s.count("Children")
	return s.Fake.Children(id)
}

func (s *countingStore) Comments(id string) ([]beads.Comment, error) {
	s.count("Comments")
	return s.Fake.Comments(id)
}

func (s *countingStore) AddComment(id, text string) error {
	s.count("AddComment")
	if err := s.Fake.AddComment(id, text); err != nil {
		return err
	}
	if s.bumpOnComment {
		// bd journals a comment against its bead, so the row moves.
		return s.Fake.Update(id, beads.UpdateOptions{})
	}
	return nil
}

// readingLander is a landing that reads the bead, its comments and its
// molecule the way a real one starts, so the store's counts show a landing
// was attempted, then answers with a fixed rejection.
type readingLander struct {
	store  *countingStore
	reject *land.Rejection
	calls  int
}

func (l *readingLander) Land(_ context.Context, w land.Work) (land.Result, error) {
	l.calls++
	if _, err := l.store.Show(w.BeadID); err != nil {
		return land.Result{}, err
	}
	if _, err := l.store.Children(w.BeadID); err != nil {
		return land.Result{}, err
	}
	if _, err := l.store.Comments(w.BeadID); err != nil {
		return land.Result{}, err
	}
	return land.Result{}, l.reject
}

// refuseEveryPass sets the harness up as a rig whose landing always ends in a
// refusal that leaves the bead ready, with the store counting every read.
func refuseEveryPass(t *testing.T) (*harness, *countingStore, *readingLander) {
	t.Helper()
	h := newHarness(t)
	cs := newCountingStore()
	h.bd = cs.Fake
	h.w.Beads = cs
	l := &readingLander{store: cs, reject: &land.Rejection{Kind: land.RejectReview, Reason: "om review returned no verdict", Rework: false}}
	h.w.Lander = l
	return h, cs, l
}

// A held bead whose row has not moved is one the worker already judged: a
// second pass pays for the list and nothing else (gt-r9pj9).
func TestPassReadsAHeldBeadOnlyOnceWhileItsRowStandsStill(t *testing.T) {
	t.Parallel()
	h, cs, l := refuseEveryPass(t)
	h.seedReady(t, "gt-abc")

	h.w.Pass(context.Background())
	if l.calls != 1 {
		t.Fatalf("landed %d times on the first pass; want the refusal attempted once", l.calls)
	}
	if n := cs.countOf("Show", "Children", "Comments"); n == 0 {
		t.Fatal("the first pass read nothing of the bead it judged")
	}

	for pass := 2; pass <= 5; pass++ {
		cs.reset()
		if rep := h.w.Pass(context.Background()); rep.Skipped != 1 {
			t.Fatalf("pass %d reported %v; want the held bead skipped", pass, rep)
		}
		if n := cs.countOf("Show", "Children", "Comments"); n != 0 {
			t.Fatalf("pass %d read the held bead %d times; want no read of the row it already judged", pass, n)
		}
		if l.calls != 1 {
			t.Fatalf("pass %d landed the held bead again (%d attempts)", pass, l.calls)
		}
	}
}

// A bead someone changed is read on the next pass, so a resubmission or a
// human's note never waits out the hold (gt-r9pj9).
func TestPassReadsAHeldBeadAgainOnceItsRowMoves(t *testing.T) {
	t.Parallel()
	h, cs, l := refuseEveryPass(t)
	h.seedReady(t, "gt-abc")
	// The refusal comment moves the row, as bd journals it, and the pass the
	// memo records is the one its own comment left: a later move is someone
	// else's, and is read.
	cs.bumpOnComment = true
	h.w.Pass(context.Background())
	if l.calls != 1 {
		t.Fatalf("landed %d times on the first pass; want the refusal attempted once", l.calls)
	}

	if err := h.bd.Update("gt-abc", beads.UpdateOptions{AddLabels: []string{"human-touched"}}); err != nil {
		t.Fatal(err)
	}
	cs.reset()
	h.w.Pass(context.Background())
	if l.calls != 2 {
		t.Fatalf("landed %d times; want the changed row read again", l.calls)
	}
	if n := cs.countOf("Show", "Children", "Comments"); n == 0 {
		t.Fatal("the pass that saw the row move read nothing of the bead")
	}
}

// A bead the landing finds not ready is held on the same wait a refusal gets,
// never re-read or re-landed on every pass (gt-r9pj9).
func TestPassHoldsABeadTheLandingFindsNotReady(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := newCountingStore()
	h.bd = cs.Fake
	h.w.Beads = cs
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, fmt.Errorf("%w: parked for a human", land.ErrNotReady)
	}
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 {
		t.Fatalf("landed %d times; want the not-ready answer once", len(h.lander.calls))
	}
	cs.reset()
	if rep := h.w.Pass(context.Background()); rep.Skipped != 1 || len(h.lander.calls) != 1 {
		t.Fatalf("second pass %v with %d landings; want the not-ready bead held", rep, len(h.lander.calls))
	}
	if n := cs.countOf("Show", "Children", "Comments"); n != 0 {
		t.Fatalf("the second pass made %d reads of the not-ready bead; want it held on the row it left", n)
	}
}

// The refusal a held bead carries is posted once, and the pass that posts it
// learns where its own comment left the row: the passes inside the hold pay
// neither a read of the bead nor another landing attempt (gt-r9pj9).
func TestPassPostsTheRefusalAnnotationOnce(t *testing.T) {
	t.Parallel()
	h, cs, l := refuseEveryPass(t)
	h.seedReady(t, "gt-abc")
	// A comment moves the row, as bd journals it: the memo must still not
	// repost the annotation the bead already carries, nor mistake its own
	// comment for a change worth re-reading the bead.
	cs.bumpOnComment = true

	h.w.Pass(context.Background())
	for pass := 2; pass <= 5; pass++ {
		cs.reset()
		h.w.Pass(context.Background())
		if n := cs.countOf("Show", "Children", "Comments", "AddComment"); n != 0 {
			t.Fatalf("pass %d made %d reads of the bead the pass before had judged; want none", pass, n)
		}
	}
	if got := h.comments(t, "gt-abc"); len(got) != 1 {
		t.Fatalf("%d refusal annotations across 5 passes; want the one the bead already carries", len(got))
	}
	if l.calls != 1 {
		t.Fatalf("landed %d times across 5 passes; want the first pass alone", l.calls)
	}
}

// An annotation is posted once per reason even when a bead's outcomes
// alternate: one remembered key would let an infrastructure notice and a
// refusal take turns re-posting each other (gt-r9pj9).
func TestPassDoesNotRepostAnAnnotationWhenOutcomesAlternate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	infra := &land.InfraError{Stage: "gate", Err: errors.New("make: not found")}
	refusal := &land.Rejection{Kind: land.RejectReview, Reason: "om review returned no verdict", Rework: false}
	h.lander.fn = func(n int, _ land.Work) (land.Result, error) {
		if n == infraAnnounceAfter+1 {
			return land.Result{}, refusal
		}
		return land.Result{}, infra
	}
	for i := 0; i < infraAnnounceAfter; i++ {
		h.w.Pass(context.Background())
		h.now = h.now.Add(infraBackoffMax)
	}
	// The refusal, once its own wait is due.
	h.w.Pass(context.Background())
	h.now = h.now.Add(humanWaitBackoff)
	// The same infrastructure failures again: the bead already carries the
	// notice, so it is not posted a second time.
	for i := 0; i < infraAnnounceAfter; i++ {
		h.w.Pass(context.Background())
		h.now = h.now.Add(infraBackoffMax)
	}

	var infraNotices int
	cs := h.comments(t, "gt-abc")
	for _, c := range cs {
		if strings.Contains(c, "infrastructure") {
			infraNotices++
		}
	}
	if infraNotices != 1 || len(cs) != 2 {
		t.Fatalf("comments %q; want one infrastructure notice and one refusal", cs)
	}
}

// The census-style count: three held beads over ten passes cost the list call
// each pass plus what the first pass read, and nothing else (gt-r9pj9).
func TestPassOverHeldBeadsCostsOnlyTheListAfterTheFirstRead(t *testing.T) {
	t.Parallel()
	h, cs, l := refuseEveryPass(t)
	for _, id := range []string{"gt-a", "gt-b", "gt-c"} {
		h.seedReady(t, id)
	}

	h.w.Pass(context.Background())
	first := cs.countOf("Show", "Children", "Comments", "AddComment")
	if l.calls != 3 || first == 0 {
		t.Fatalf("first pass: %d landings, %d reads; want three refused beads read", l.calls, first)
	}

	for pass := 2; pass <= 10; pass++ {
		cs.reset()
		h.w.Pass(context.Background())
		if got := cs.countOf("List"); got != 1 {
			t.Fatalf("pass %d listed %d times; want the one list poll", pass, got)
		}
		if got := cs.countOf("Show", "Children", "Comments", "AddComment"); got != 0 {
			t.Fatalf("pass %d made %d reads beside the list; want only the list", pass, got)
		}
	}
}
