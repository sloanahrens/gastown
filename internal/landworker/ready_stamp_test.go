package landworker

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// TestReadyLabelWriteTouchesStamp: queueing a revert touches the rig's ready
// stamp once the label write succeeds, so an idle worker lists the bead on its
// next pass rather than waiting for its ceiling (gt-flo7a). A label write that
// failed leaves no stamp.
func TestReadyLabelWriteTouchesStamp(t *testing.T) {
	t.Parallel()

	seedRevert := func(h *redMainHarness) {
		h.bd.Seed(beads.Issue{ID: "gt-rv", Title: "the revert", Type: "task", Status: "open"})
	}

	t.Run("a queued revert stamps the rig", func(t *testing.T) {
		t.Parallel()
		h := newRedMainHarness(t)
		h.r.TownRoot = t.TempDir()
		seedRevert(h)

		if err := h.r.queueRevert("gt-rv"); err != nil {
			t.Fatalf("queueRevert: %v", err)
		}
		stamp, err := land.ReadyStamp(h.r.TownRoot, "gastown")
		if err != nil {
			t.Fatalf("ReadyStamp: %v", err)
		}
		if stamp.IsZero() {
			t.Fatal("the queued revert left no ready stamp")
		}
	})

	t.Run("a failed label write stamps nothing", func(t *testing.T) {
		t.Parallel()
		h := newRedMainHarness(t)
		h.r.TownRoot = t.TempDir()
		seedRevert(h)
		h.r.Beads = &failingWrites{RedMainBeads: h.bd, failLabel: true}

		if err := h.r.queueRevert("gt-rv"); err == nil {
			t.Fatal("queueRevert with a failing update = nil, want an error")
		}
		stamp, err := land.ReadyStamp(h.r.TownRoot, "gastown")
		if err != nil {
			t.Fatalf("ReadyStamp: %v", err)
		}
		if !stamp.IsZero() {
			t.Errorf("a failed label write left ready stamp %v, want none", stamp)
		}
	})
}
