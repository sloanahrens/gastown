package landworker

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// gateAheadIssue is a ready submission for the gate-ahead tests: its own
// branch, its Worker line naming submitter, and submitted at submitted.
func gateAheadIssue(id, submitter string, submitted time.Time) *beads.Issue {
	w := land.Work{Branch: "polecat/opal/" + id + "+x1", Head: noteHead, Target: "main", Worker: submitter, Submitted: submitted}
	return &beads.Issue{ID: id, Status: "hooked", Labels: []string{land.LabelReadyToLand},
		Notes: land.FormatReadyNote(w), UpdatedAt: submitted.UTC().Format(time.RFC3339)}
}

// seedGateAhead seeds ids as ready submissions a minute apart in the order
// given, and returns them by id.
func seedGateAhead(h *harness, base time.Time, ids ...string) map[string]*beads.Issue {
	seeded := map[string]*beads.Issue{}
	for i, id := range ids {
		is := gateAheadIssue(id, "opal", base.Add(time.Duration(i)*time.Minute))
		h.bd.Seed(*is)
		seeded[id] = is
	}
	return seeded
}

// TestNextGateAheadReturnsTheFirstBeadAfterTheFront pins that with enough
// ready beads nextGateAhead names the bead landing immediately after the
// front, whatever order the board is stored in, and writes nothing (gt-xlmcx2).
func TestNextGateAheadReturnsTheFirstBeadAfterTheFront(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// The store lists newest first, so the board order is the reverse of the
	// landing order: the answer cannot come from list order.
	seedGateAhead(h, time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC), "gt-front", "gt-next", "gt-later")
	work, ok := h.w.nextGateAhead(land.Work{BeadID: "gt-front"}, 2)
	if !ok || work.BeadID != "gt-next" || work.Rig != "gastown" || work.Branch != "polecat/opal/gt-next+x1" {
		t.Fatalf("nextGateAhead = %+v, %v; want gt-next's work on gastown", work, ok)
	}
	for _, id := range []string{"gt-front", "gt-next", "gt-later"} {
		if cs := h.comments(t, id); len(cs) != 0 {
			t.Fatalf("%s carries comments %q; nextGateAhead writes nothing", id, cs)
		}
	}
}

// TestNextGateAheadRefuses pins that nextGateAhead answers false when it can
// name no bead it may gate ahead: a failed list, a board shorter than
// minReady, a front the board does not hold, and a next bead the worker would
// not offer or may not land, never a good bead behind it in its place (gt-xlmcx2).
func TestNextGateAheadRefuses(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		minReady int
		seed     func(h *harness)
	}{
		{"the list fails", 2, func(h *harness) {
			seedGateAhead(h, base, "gt-front", "gt-next", "gt-later")
			h.w.Beads = &failingListStore{Fake: h.bd, err: errors.New("the store is down")}
		}},
		{"the board is shorter than minReady", 3, func(h *harness) {
			seedGateAhead(h, base, "gt-front", "gt-next")
		}},
		{"the front is not listed", 2, func(h *harness) {
			seedGateAhead(h, base, "gt-next", "gt-later")
		}},
		{"the next bead is held by its last outcome", 2, func(h *harness) {
			is := seedGateAhead(h, base, "gt-front", "gt-next", "gt-later")
			h.w.outcomeMemo().record("gt-next", is["gt-next"].UpdatedAt, h.w.now().Add(time.Hour))
		}},
		{"the next bead is parked", 2, func(h *harness) {
			seedGateAhead(h, base, "gt-front", "gt-next", "gt-later")
			st := h.w.bead("gt-next")
			st.parked, st.until = true, h.w.now().Add(time.Hour)
		}},
		{"the next bead's submission cannot be read", 2, func(h *harness) {
			seedGateAhead(h, base, "gt-front", "gt-next", "gt-later")
			h.w.Beads = commentsFailFor{Beads: h.w.Beads, id: "gt-next"}
		}},
		{"the next bead is refused", 2, func(h *harness) {
			seedGateAhead(h, base, "gt-front", "gt-later")
			h.bd.Seed(*gateAheadIssue("gt-next", humanActor, base.Add(30*time.Second)))
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			tc.seed(h)
			work, ok := h.w.nextGateAhead(land.Work{BeadID: "gt-front"}, tc.minReady)
			if ok || work != (land.Work{}) {
				t.Fatalf("nextGateAhead returned %+v, %v for %q; want no bead ahead", work, ok, tc.name)
			}
		})
	}
}
