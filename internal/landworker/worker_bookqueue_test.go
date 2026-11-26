package landworker

import (
	"context"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// holdQueue starts a queue whose job holds beadID until release closes, and
// waits until that job runs, so Holds(beadID) is true before the worker reads it.
func holdQueue(ctx context.Context, t *testing.T, beadID string) (*BookQueue, chan struct{}) {
	t.Helper()
	q := &BookQueue{}
	entered, release := make(chan struct{}), make(chan struct{})
	q.Add(beadID, func() { close(entered); hold(ctx, release) })
	recvBounded(t, ctx, entered, "the bookkeeping job for "+beadID+" starting")
	return q, release
}

// releaseBeadJob ends beadID's bookkeeping job and returns once Holds(beadID)
// is false. Jobs run in order, so the marker job added behind the held one only
// starts after the queue has cleared the held entry.
func releaseBeadJob(ctx context.Context, t *testing.T, q *BookQueue, beadID string, release chan struct{}) {
	t.Helper()
	close(release)
	after := make(chan struct{})
	q.Add(beadID+"-after", func() { close(after) })
	recvBounded(t, ctx, after, "the bookkeeping job for "+beadID+" returning")
}

// TestBookQueueWorkerSkips: a bead whose deferred bookkeeping is queued already
// landed, so the worker neither lands nor repairs it until the job returns
// (gt-ppcl1g).
func TestBookQueueWorkerSkips(t *testing.T) {
	t.Parallel()

	t.Run("a held bead on the ready list is not landed", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		h := newHarness(t)
		h.seedReady(t, "gt-abc")
		q, release := holdQueue(ctx, t, "gt-abc")
		h.w.Books = q

		rep := h.w.Pass(ctx)
		if len(h.lander.calls) != 0 || rep.Landed != 0 {
			t.Fatalf("landed %d beads (report %v) while gt-abc's bookkeeping was queued; want none", len(h.lander.calls), rep)
		}

		releaseBeadJob(ctx, t, q, "gt-abc", release)
		rep = h.w.Pass(ctx)
		if len(h.lander.calls) != 1 || rep.Landed != 1 {
			t.Fatalf("landed %d beads (report %v) after the queue let gt-abc go; want one landing", len(h.lander.calls), rep)
		}
	})

	t.Run("a held bead in pendingRepair is not repaired", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		h := newHarness(t)
		h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "landed work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal"})
		h.w.pendingRepair = map[string]land.Work{
			"gt-abc": {BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: noteHead, Target: "main", Worker: "opal"},
		}
		q, release := holdQueue(ctx, t, "gt-abc")
		h.w.Books = q

		rep := h.w.Pass(ctx)
		if len(h.lander.calls) != 0 || rep.Repaired != 0 {
			t.Fatalf("repaired %d beads (report %v) while gt-abc's bookkeeping was queued; want none", len(h.lander.calls), rep)
		}

		releaseBeadJob(ctx, t, q, "gt-abc", release)
		rep = h.w.Pass(ctx)
		if len(h.lander.calls) != 1 || rep.Repaired != 1 {
			t.Fatalf("repaired %d beads (report %v) after the queue let gt-abc go; want one repair", len(h.lander.calls), rep)
		}
	})

	t.Run("a held recent landing is not queued for repair", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		h := newHarness(t)
		h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "landed work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal"})
		rec := land.LandingRecord{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: noteHead, Target: "main", LandedCommit: "cccccccccc", Route: "daemon"}
		h.files.recs = []land.LandingRecord{rec}
		h.remote.contains[rec.LandedCommit] = true
		q, release := holdQueue(ctx, t, "gt-abc")
		h.w.Books = q

		rep := h.w.Pass(ctx)
		if len(h.lander.calls) != 0 || rep.Landed != 0 || rep.Repaired != 0 {
			t.Fatalf("landed or repaired %d beads (report %v) while gt-abc's bookkeeping was queued; want none", len(h.lander.calls), rep)
		}
		if _, queued := h.w.pendingRepair["gt-abc"]; queued {
			t.Fatal("gt-abc was queued for repair while its bookkeeping was queued; want the repair to wait for the job")
		}

		releaseBeadJob(ctx, t, q, "gt-abc", release)
		// The startup repair runs again, as a restarted worker would, now that
		// the queue has let the bead go.
		h.w.queueRecentRepairs()
		if _, queued := h.w.pendingRepair["gt-abc"]; !queued {
			t.Fatal("gt-abc was not queued for repair after the queue let it go; want its record finished")
		}
		rep = h.w.Pass(ctx)
		if len(h.lander.calls) != 1 || rep.Repaired != 1 {
			t.Fatalf("repaired %d beads (report %v) after the queue let gt-abc go; want one repair", len(h.lander.calls), rep)
		}
	})
}
