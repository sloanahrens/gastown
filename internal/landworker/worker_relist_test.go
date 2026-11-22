package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// landedOrder is the bead IDs of the landings the fake lander recorded, in the
// order the worker attempted them.
func landedOrder(calls []land.Work) string {
	var ids []string
	for _, c := range calls {
		ids = append(ids, c.BeadID)
	}
	return strings.Join(ids, ",")
}

// TestLandReadyRelistP0MadeReadyMidPassLandsSecond: the pass lists the ready
// board, and a P0 made ready while the first P2 lands is listed again and lands
// second, before the P2 the pass listed with it. A list read once would land
// both P2s first (gt-wgvu6p).
func TestLandReadyRelistP0MadeReadyMidPassLandsSecond(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-p2a", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-p2b", 2, "2026-09-30T10:01:00Z")
	h.lander.fn = func(n int, _ land.Work) (land.Result, error) {
		if n == 1 {
			// The P0 the operator labelled ready while the first landing ran.
			h.seedReadyAt(t, "gt-p0", 0, "2026-09-30T10:02:00Z")
		}
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}
	// A hang guard: only a pass that never lets the loop end reaches it.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rep := h.w.Pass(ctx)
	if got := landedOrder(h.lander.calls); got != "gt-p2a,gt-p0,gt-p2b" {
		t.Fatalf("landed %s; want gt-p2a,gt-p0,gt-p2b: the P0 made ready mid-pass lands before the other P2", got)
	}
	if rep.Landed != 3 {
		t.Fatalf("report %v; want three landings", rep)
	}
}

// TestLandReadyRelistStillReadyBeadAttemptedOnce: a bead that still carries the
// ready label after its attempt is looked at once in the pass, so the loop
// ends. Without the handled set the pass would land it again and again until
// the context guard expires (gt-wgvu6p).
func TestLandReadyRelistStillReadyBeadAttemptedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-p2a", 2, "2026-09-30T10:00:00Z")
	// A hang guard: only a pass whose loop never ends reaches it. The second
	// landing, which only a pass without a handled set makes, ends the pass
	// here rather than looping until the guard expires.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h.lander.fn = func(n int, _ land.Work) (land.Result, error) {
		if n > 1 {
			cancel()
		}
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}
	rep := h.w.Pass(ctx)
	if rep.Landed != 1 || len(h.lander.calls) != 1 {
		t.Fatalf("report %v with %s; want the still-ready bead attempted once", rep, landedOrder(h.lander.calls))
	}
}

// TestLandReadyRelistStillReadyBeadStopsBeforeTheGuard: a pass whose bead
// stays ready after its landing ends after that one attempt, under the handled
// set, so neither of the harness's loop guards is reached (gt-ek7hmt).
func TestLandReadyRelistStillReadyBeadStopsBeforeTheGuard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-p2a", 2, "2026-09-30T10:00:00Z")
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || len(h.lander.calls) != 1 {
		t.Fatalf("report %v with %s; want the still-ready bead landed once", rep, landedOrder(h.lander.calls))
	}
	if h.guard.failed || h.loopFailed {
		t.Fatalf("the loop guards were reached by a pass that ends after one landing (lists %d, drain reads %d); want neither", h.guard.lists, h.loopReads)
	}
	// The pass's one bead costs one list of the ready board and one re-list
	// after its landing.
	if h.guard.lists != 2 {
		t.Fatalf("listed the ready board %d times; want the first list and the re-list after the landing", h.guard.lists)
	}
}

// recordingTB keeps the harness's report instead of failing the test that
// drives the guard, so a row can assert the line the guard names.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// TestGuardedBeadsReportsALoopThatDoesNotEnd: past readyListLimit lists of the
// ready board the guard reports the loop once and returns an error on every
// later list, which is what the pass's relist branch turns into a failure and
// a return (gt-ek7hmt).
func TestGuardedBeadsReportsALoopThatDoesNotEnd(t *testing.T) {
	t.Parallel()
	ready := beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1}
	want := fmt.Sprintf("the pass listed the ready board %d times: its loop does not end", readyListLimit)
	rec := &recordingTB{TB: t}
	b := &guardedBeads{Fake: beadsfake.New(beadsfake.WithPrefix("gt")), t: rec}

	for i := 0; i < readyListLimit; i++ {
		if _, err := b.List(ready); err != nil {
			t.Fatalf("list %d of the limit returned %v; want no error until the limit is passed", i+1, err)
		}
	}
	if len(rec.errors) != 0 {
		t.Fatalf("the guard reported %v at the limit; want nothing until it is passed", rec.errors)
	}

	if _, err := b.List(ready); err == nil || err.Error() != want {
		t.Fatalf("list past the limit = %v; want the guard's error %q", err, want)
	}
	if len(rec.errors) != 1 || rec.errors[0] != want {
		t.Fatalf("the guard reported %v; want %q once", rec.errors, want)
	}

	if _, err := b.List(ready); err == nil {
		t.Fatal("the second list past the limit returned no error; want the pass's relist to keep failing")
	}
	if len(rec.errors) != 1 {
		t.Fatalf("the guard reported %d times after a second list past the limit; want once", len(rec.errors))
	}

	// A list of another board is not the pass's loop and never counts.
	if _, err := b.List(beads.ListOptions{Label: LabelRevert, Priority: -1}); err != nil {
		t.Fatalf("a list of %s returned %v; want it untouched by the guard", LabelRevert, err)
	}
}

// TestHarnessDrainGuardReportsALoopThatDoesNotEnd: the backstop guard covers
// the loops that skip a bead without listing again: past passIterationLimit
// reads it reports the loop once and answers draining, which ends the pass at
// its next iteration (gt-ek7hmt).
func TestHarnessDrainGuardReportsALoopThatDoesNotEnd(t *testing.T) {
	t.Parallel()
	want := fmt.Sprintf("the pass read its drain flag %d times: its loop does not end", passIterationLimit)
	rec := &recordingTB{TB: t}
	h := newHarness(rec)

	for i := 0; i < passIterationLimit; i++ {
		if h.w.Draining() {
			t.Fatalf("drain read %d of the limit answered true; want the pass to run until the limit is passed", i+1)
		}
	}
	if len(rec.errors) != 0 {
		t.Fatalf("the guard reported %v at the limit; want nothing until it is passed", rec.errors)
	}

	if !h.w.Draining() {
		t.Fatal("the read past the limit answered not draining; the pass's loop would not end")
	}
	if len(rec.errors) != 1 || rec.errors[0] != want {
		t.Fatalf("the guard reported %v; want %q once", rec.errors, want)
	}
	if again := h.w.Draining(); !again || len(rec.errors) != 1 {
		t.Fatalf("a later read answered %v and the guard reported %d times; want draining and once", again, len(rec.errors))
	}
}

// failAfterFirstListBeads lists the ready board once and fails every later
// list, the store a pass meets when it re-lists after a landing.
type failAfterFirstListBeads struct {
	*beadsfake.Fake
	lists int
}

func (b *failAfterFirstListBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	b.lists++
	if b.lists > 1 {
		return nil, errors.New("the store dropped the second list")
	}
	return b.Fake.List(opts)
}

// TestLandReadyRelistListFailureEndsPass: a re-list that fails after a landing
// ends the pass with one failure and no further landing, and lastListAt stays
// the first list's rather than being cleared or moved (gt-wgvu6p). Clearing it
// would let the next pass read an idle skip off a list that was never read.
func TestLandReadyRelistListFailureEndsPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-p2a", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-p2b", 2, "2026-09-30T10:01:00Z")
	h.w.Beads = &failAfterFirstListBeads{Fake: h.bd}
	listedAt := h.now
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		// The landing takes minutes: a failed re-list that moved lastListAt,
		// as the first list's failure branch deliberately does, would show it.
		h.now = h.now.Add(5 * time.Minute)
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}
	// A hang guard: only a pass that neither ends on the failed list nor stops
	// after it reaches the timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rep := h.w.Pass(ctx)
	if got := landedOrder(h.lander.calls); got != "gt-p2a" {
		t.Fatalf("landed %s; want only the landing in progress when the list failed", got)
	}
	if rep.Landed != 1 || rep.Failed != 1 {
		t.Fatalf("report %v; want one landing and one failure from the later list", rep)
	}
	if !h.w.lastListAt.Equal(listedAt) {
		t.Fatalf("lastListAt = %v; want the first list's %v", h.w.lastListAt, listedAt)
	}
}

// TestLandReadyRelistDrainStopsAfterLanding: a worker that starts draining
// while a landing runs finishes that landing and takes no second bead, though
// the re-list after it still holds one (gt-wgvu6p).
func TestLandReadyRelistDrainStopsAfterLanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-p2a", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-p2b", 2, "2026-09-30T10:01:00Z")
	draining := false
	h.w.Draining = func() bool { return draining }
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		draining = true // the restart marker appears mid-landing
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}
	// A hang guard: only a pass that keeps claiming past the drain reaches it.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rep := h.w.Pass(ctx)
	if got := landedOrder(h.lander.calls); got != "gt-p2a" {
		t.Fatalf("landed %s; want only the landing in progress when the drain began", got)
	}
	if rep.Landed != 1 || rep.Failed != 0 {
		t.Fatalf("report %v; want one landing and no failure", rep)
	}
}
