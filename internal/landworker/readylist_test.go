package landworker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// countingBeads counts the worker's lists of the ready board, the call an idle
// pass must not make.
type countingBeads struct {
	*beadsfake.Fake
	lists int
}

func (b *countingBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	b.lists++
	return b.Fake.List(opts)
}

// readyListHarness is a worker with a town root to stamp and a counted board.
func readyListHarness(t *testing.T) (*harness, *countingBeads) {
	t.Helper()
	h := newHarness(t)
	h.w.TownRoot = t.TempDir()
	c := &countingBeads{Fake: h.bd}
	h.w.Beads = c
	return h, c
}

// TestLandReadySkipsListWhileIdle: after an empty list, a pass inside three
// minutes with no newer stamp makes no List call and still publishes the count
// 0, so the queue column does not go stale (gt-flo7a).
func TestLandReadySkipsListWhileIdle(t *testing.T) {
	t.Parallel()
	h, bd := readyListHarness(t)
	view := NewLandingQueueView(func() time.Time { return h.now })
	h.w.Queue = view

	if rep := h.w.Pass(context.Background()); rep.Failed != 0 {
		t.Fatalf("first pass %v; want a clean empty list", rep)
	}
	if bd.lists != 1 {
		t.Fatalf("first pass listed the board %d times, want 1", bd.lists)
	}

	h.now = h.now.Add(time.Minute)
	if rep := h.w.Pass(context.Background()); rep.Failed != 0 {
		t.Fatalf("second pass %v; want a clean skip", rep)
	}
	if bd.lists != 1 {
		t.Errorf("idle pass listed the board %d times, want the first pass's 1", bd.lists)
	}
	if got := view.Counts(h.now); got["gastown"] != 0 {
		t.Errorf("published %v; want gastown 0 from the skipped pass", got)
	}
}

// TestLandReadyListsAgainAfterStampOrCeiling: every thing that makes a skipped
// list wrong asks for a list: a submission newer than the last list, a list
// past the ceiling, a last list that was not empty, and a stamp that cannot be
// read (fail open).
func TestLandReadyListsAgainAfterStampOrCeiling(t *testing.T) {
	t.Parallel()

	type row struct {
		name string
		// afterFirst runs once the first, empty pass has listed the board.
		afterFirst func(t *testing.T, h *harness)
		// moveNow advances the worker's clock before the second pass.
		moveNow time.Duration
		// seed first seeds a ready bead before the first pass, so the last list
		// was not empty.
		seed bool
	}

	rows := []row{
		{
			name: "a newer stamp",
			afterFirst: func(t *testing.T, h *harness) {
				if err := land.TouchReadyStamp(h.w.TownRoot, "gastown", h.now.Add(time.Second)); err != nil {
					t.Fatalf("TouchReadyStamp: %v", err)
				}
			},
			moveNow: time.Minute,
		},
		{name: "a list older than three minutes", moveNow: 3 * time.Minute},
		{name: "a non-empty last list", seed: true},
		{
			name: "an unreadable stamp",
			afterFirst: func(t *testing.T, h *harness) {
				// A regular file where the rig directory belongs: stat on the stamp
				// below it fails with ENOTDIR, which is not "no stamp".
				stampPath, err := land.ReadyStampPath(h.w.TownRoot, "gastown")
				if err != nil {
					t.Fatalf("ReadyStampPath: %v", err)
				}
				rigDir := filepath.Dir(stampPath)
				if err := os.MkdirAll(filepath.Dir(rigDir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(rigDir, []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			moveNow: time.Minute,
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			h, bd := readyListHarness(t)
			if r.seed {
				h.seedReady(t, "gt-abc")
			}
			if rep := h.w.Pass(context.Background()); rep.Failed != 0 {
				t.Fatalf("first pass %v; want a clean list", rep)
			}
			if bd.lists != 1 {
				t.Fatalf("first pass listed the board %d times, want 1", bd.lists)
			}
			if r.afterFirst != nil {
				r.afterFirst(t, h)
			}
			h.now = h.now.Add(r.moveNow)
			h.w.Pass(context.Background())
			if bd.lists != 2 {
				t.Errorf("second pass listed the board %d times in total, want it to list again", bd.lists)
			}
		})
	}
}
