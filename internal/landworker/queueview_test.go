package landworker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// queueAt is the instant the view tests publish and read at.
var queueAt = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// queueClock is a view clock a test moves.
type queueClock struct{ at time.Time }

func (c *queueClock) now() time.Time { return c.at }

func TestLandingQueueViewServesWhatEachRigPublished(t *testing.T) {
	t.Parallel()
	view := NewLandingQueueView(func() time.Time { return queueAt })
	view.Publish("alpha", 3)
	view.Publish("beta", 0)
	got := view.Counts(queueAt)
	if len(got) != 2 || got["alpha"] != 3 || got["beta"] != 0 {
		t.Fatalf("Counts = %v; want alpha 3 and beta 0", got)
	}
	// A later pass replaces the count; the view never keeps two.
	view.Publish("alpha", 1)
	if got := view.Counts(queueAt); got["alpha"] != 1 {
		t.Fatalf("alpha = %d after a second publish; want 1", got["alpha"])
	}
}

// The bound is one landing pass plus its own duration: a count exactly that old
// still answers, and a second older does not (gt-hw6ih).
func TestLandingQueueViewDropsACountPastItsAge(t *testing.T) {
	t.Parallel()
	view := NewLandingQueueView(func() time.Time { return queueAt })
	view.Publish("alpha", 3)
	if got := view.Counts(queueAt.Add(LandingQueueMaxAge)); got["alpha"] != 3 {
		t.Fatalf("alpha = %v at the bound; want the published 3", got["alpha"])
	}
	got := view.Counts(queueAt.Add(LandingQueueMaxAge + time.Nanosecond))
	if len(got) != 0 {
		t.Fatalf("Counts = %v past the bound; want no count, so the reader lists the board", got)
	}
}

// A rig whose clock stamp is ahead of the reader's is a clock disagreement, not
// a stale reading: dropping it would spend a board read on every sample.
func TestLandingQueueViewKeepsACountStampedAfterTheRead(t *testing.T) {
	t.Parallel()
	clock := &queueClock{at: queueAt.Add(time.Second)}
	view := NewLandingQueueView(clock.now)
	view.Publish("alpha", 3)
	if got := view.Counts(queueAt); got["alpha"] != 3 {
		t.Fatalf("alpha = %v; want the count a second-older clock stamped", got["alpha"])
	}
}

func TestLandingQueueViewAnswersNothingBeforeAPublish(t *testing.T) {
	t.Parallel()
	view := NewLandingQueueView(func() time.Time { return queueAt })
	if got := view.Counts(queueAt); len(got) != 0 {
		t.Fatalf("Counts = %v on a fresh view; want none", got)
	}
}

// The daemon wires a view it may never build a worker for, so the nil view is
// the one a reader meets when the landing patrol is off.
func TestNilLandingQueueViewKeepsNothing(t *testing.T) {
	t.Parallel()
	var view *LandingQueueView
	view.Publish("alpha", 3)
	if got := view.Counts(queueAt); len(got) != 0 {
		t.Fatalf("Counts = %v on a nil view; want none", got)
	}
}

func TestLandingQueueViewHoldsEveryRigsPublishAtOnce(t *testing.T) {
	t.Parallel()
	view := NewLandingQueueView(func() time.Time { return queueAt })
	const rigs = 8
	var wg sync.WaitGroup
	for i := 0; i < rigs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			view.Publish(fmt.Sprintf("rig%d", i), i)
			view.Counts(queueAt)
		}(i)
	}
	wg.Wait()
	got := view.Counts(queueAt)
	if len(got) != rigs {
		t.Fatalf("Counts holds %d rigs; want the %d published", len(got), rigs)
	}
	for i := 0; i < rigs; i++ {
		if rig := fmt.Sprintf("rig%d", i); got[rig] != i {
			t.Errorf("%s = %d; want %d", rig, got[rig], i)
		}
	}
}

// A pass hands the queue the count of the board it just listed, before it
// lands anything (gt-hw6ih).
func TestPassPublishesTheReadyCount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.seedReadyAt(t, "gt-def", 2, "2026-09-30T11:00:00Z")
	view := NewLandingQueueView(func() time.Time { return h.now })
	h.w.Queue = view
	h.w.Pass(context.Background())
	if got := view.Counts(h.now); got["gastown"] != 2 {
		t.Fatalf("published %v; want gastown 2, the board the pass listed", got)
	}
}

// failingListBeads is the worker's store whose list of the board fails.
type failingListBeads struct {
	*beadsfake.Fake
	err error
}

func (b failingListBeads) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, b.err }

// A board a pass could not read publishes nothing: zero is a queue that
// emptied, and this is a queue nobody read (gt-hw6ih).
func TestPassFailedListPublishesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.Beads = failingListBeads{Fake: h.bd, err: errors.New("no Dolt server")}
	view := NewLandingQueueView(func() time.Time { return h.now })
	h.w.Queue = view
	if rep := h.w.Pass(context.Background()); rep.Failed == 0 {
		t.Fatalf("report %v; want the failed list counted", rep)
	}
	if got := view.Counts(h.now); len(got) != 0 {
		t.Fatalf("published %v after a failed list; want nothing published", got)
	}
}
