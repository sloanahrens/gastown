package landworker

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// resubmitBeads re-seeds one bead with a newer READY TO LAND block the first
// time the pass re-lists after an attempt: the seat reworked a rejected bead
// and resubmitted it while the pass still ran (gt-jx9f8g).
type resubmitBeads struct {
	*beadsfake.Fake
	lists  int
	reseed func()
}

func (b *resubmitBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	b.lists++
	if b.lists == 2 && b.reseed != nil {
		b.reseed()
		b.reseed = nil
	}
	return b.Fake.List(opts)
}

// TestPassPicksAResubmittedBeadAgain: a P2 the pass attempted comes back ready
// with a newer READY block, so it is a new submission and lands again. Both
// beads are P2, so the older submission leads: gt-z (10:01) lands between the
// two submissions of gt-x (10:00 and 10:05). Keyed by id alone, the handled set
// would pass the resubmitted bead over and the pass would land only gt-x,gt-z
// (gt-jx9f8g).
func TestPassPicksAResubmittedBeadAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-x", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-z", 2, "2026-09-30T10:01:00Z")
	h.w.Beads = &resubmitBeads{Fake: h.bd, reseed: func() {
		h.seedReadyAt(t, "gt-x", 2, "2026-09-30T10:05:00Z")
	}}
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}
	h.w.Pass(t.Context())
	if got := landedOrder(h.lander.calls); got != "gt-x,gt-z,gt-x" {
		t.Fatalf("landed %s; want gt-x,gt-z,gt-x: the resubmitted P2 is picked again after the older P2", got)
	}
}

// TestPassDoesNotRetryTheSameSubmission: a bead that stays ready with the same
// READY block after its attempt is the same submission, so the pass does not
// try it a second time (gt-jx9f8g, gt-wgvu6p).
func TestPassDoesNotRetryTheSameSubmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-x", 2, "2026-09-30T10:00:00Z")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{LandedCommit: "cccccccc", PatchID: "pppppppp"}, nil
	}
	h.w.Pass(t.Context())
	if got := landedOrder(h.lander.calls); got != "gt-x" {
		t.Fatalf("landed %s; want gt-x once: a bead still ready with the same READY block is one submission", got)
	}
	if h.guard.failed {
		t.Fatalf("the loop guard was reached (lists %d); want the pass to end after one attempt", h.guard.lists)
	}
}
