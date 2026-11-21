package landworker

import (
	"context"
	"errors"
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
