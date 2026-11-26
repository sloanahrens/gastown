package landworker

import (
	"context"
	"errors"
	"slices"
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

// deferredFakes logs the reap (Remote listing) and sp-actual (Landings read).
type deferredFakes struct {
	*fakeRemote
	*fakeLandings
	steps []string
}

func (f *deferredFakes) ListRemoteRefs(prefix string) ([]RemoteRef, error) {
	f.steps = append(f.steps, "reap")
	return f.fakeRemote.ListRemoteRefs(prefix)
}

func (f *deferredFakes) LatestForBead(id string) (land.LandingRecord, bool, error) {
	f.steps = append(f.steps, "sp-actual")
	return f.fakeLandings.LatestForBead(id)
}

type deferredLander struct {
	*fakeLander
	steps     *[]string
	finishErr error
}

func (l *deferredLander) FinishLanding(land.Work, land.Result) error {
	*l.steps = append(*l.steps, "finish")
	return l.finishErr
}

func deferredHarness(t *testing.T, finishErr error) (*harness, *deferredFakes) {
	t.Helper()
	h := newHarness(t)
	f := &deferredFakes{fakeRemote: h.remote, fakeLandings: h.files}
	h.w.Remote, h.w.Landings = f, f
	h.w.Lander = &deferredLander{fakeLander: h.lander, steps: &f.steps, finishErr: finishErr}
	h.w.RefreshAuthorSeat = func(string) error { f.steps = append(f.steps, "seat"); return nil }
	h.w.AfterLanded = func(land.Work) { f.steps = append(f.steps, "after") }
	return h, f
}

// blockingDeferredLander holds a deferred landing's finish open, so a test can
// see the pass return while the record is still being written.
type blockingDeferredLander struct {
	*deferredLander
	ctx              context.Context
	entered, release chan struct{}
}

func (l *blockingDeferredLander) FinishLanding(land.Work, land.Result) error {
	*l.steps = append(*l.steps, "finish")
	close(l.entered)
	hold(l.ctx, l.release)
	return l.finishErr
}

// triggerRecorder records the bead of each inline post-landing trigger.
type triggerRecorder struct{ signs []string }

func (r *triggerRecorder) Trigger(_ context.Context, pl PostLand) {
	r.signs = append(r.signs, pl.BeadID)
}
func (r *triggerRecorder) PollPending(context.Context) {}

// passSteps records the landings reads a landing makes; the pass's own
// pre-landing record read is not one of its steps.
type passSteps struct {
	*fakeLandings
	steps  *[]string
	landed bool
}

func (p *passSteps) LatestForBead(id string) (land.LandingRecord, bool, error) {
	if p.landed {
		*p.steps = append(*p.steps, "sp-actual")
	}
	return p.fakeLandings.LatestForBead(id)
}

// deferredWorker is a harness about to land gt-abc with res; q may be nil.
func deferredWorker(t *testing.T, q *BookQueue, res land.Result) (*harness, *deferredFakes, *triggerRecorder) {
	t.Helper()
	h, f := deferredHarness(t, nil)
	steps := &passSteps{fakeLandings: f.fakeLandings, steps: &f.steps}
	h.w.Landings = steps
	h.lander.fn = func(int, land.Work) (land.Result, error) { steps.landed = true; return res, nil }
	trig := &triggerRecorder{}
	h.w.PostLand, h.w.Books = trig, q
	h.seedReady(t, "gt-abc")
	return h, f, trig
}

// TestBookQueueWorkerDefers: a deferred landing's four steps run as one job the
// pass does not wait for when a queue is set, and run inline when it is not;
// every other landing runs its steps inline as before (gt-c3vl2u).
func TestBookQueueWorkerDefers(t *testing.T) {
	t.Parallel()
	rec := land.LandingRecord{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: noteHead,
		Target: "main", LandedCommit: "cccccccccc", Route: "daemon"}
	deferred := land.Result{LandedCommit: "cccccccccc", PatchID: "pppppppppp", Deferred: &rec}

	t.Run("the pass returns while the queued finish is blocked", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		q := &BookQueue{}
		h, f, trig := deferredWorker(t, q, deferred)
		bl := &blockingDeferredLander{deferredLander: h.w.Lander.(*deferredLander), ctx: ctx,
			entered: make(chan struct{}), release: make(chan struct{})}
		h.w.Lander = bl

		passDone := make(chan Report, 1)
		go func() { passDone <- h.w.Pass(ctx) }()
		// A pass that finishes the record itself blocks here instead, so this
		// bounded wait names the missing queue Add.
		rep := recvBounded(t, ctx, passDone, "the pass returning while the finish is blocked")
		recvBounded(t, ctx, bl.entered, "the queued job entering FinishLanding")

		if rep.Landed != 1 {
			t.Fatalf("report %v; want the landing counted before the record is finished", rep)
		}
		if !slices.Equal(h.cleared, []string{"gt-abc@opal"}) || !slices.Equal(trig.signs, []string{"gt-abc"}) {
			t.Fatalf("cleared %v, triggers %v; want the intent cleared and the verdict started inline", h.cleared, trig.signs)
		}
		if !slices.Equal(f.steps, []string{"finish"}) {
			t.Fatalf("steps %v while the finish is blocked; want only the finish attempt", f.steps)
		}
		releaseBeadJob(ctx, t, q, "gt-abc", bl.release)
		if want := []string{"finish", "reap", "seat", "sp-actual", "after"}; !slices.Equal(f.steps, want) {
			t.Fatalf("steps %v; want %v", f.steps, want)
		}
	})

	t.Run("with no queue the finish runs inline and is never dropped", func(t *testing.T) {
		t.Parallel()
		h, f, _ := deferredWorker(t, nil, deferred)
		if rep := h.w.Pass(waitCtx(t)); rep.Landed != 1 {
			t.Fatalf("report %v; want the landing counted", rep)
		}
		if want := []string{"finish", "reap", "seat", "sp-actual", "after"}; !slices.Equal(f.steps, want) {
			t.Fatalf("steps %v; want %v", f.steps, want)
		}
	})

	t.Run("a landing with no deferred record runs its steps inline as before", func(t *testing.T) {
		t.Parallel()
		q := &BookQueue{}
		h, f, trig := deferredWorker(t, q, land.Result{LandedCommit: "cccccccccc", PatchID: "pppppppppp"})
		if rep := h.w.Pass(waitCtx(t)); rep.Landed != 1 {
			t.Fatalf("report %v; want the landing counted", rep)
		}
		if want := []string{"reap", "seat", "sp-actual", "after"}; !slices.Equal(f.steps, want) || !slices.Equal(trig.signs, []string{"gt-abc"}) {
			t.Fatalf("steps %v, triggers %v; want %v and the verdict started", f.steps, trig.signs, want)
		}
		if q.Holds("gt-abc") {
			t.Fatal("gt-abc was queued though its landing recorded both halves in one pass")
		}
	})
}

// IsRevert is the read a deferred landing's bookkeeping job makes before it
// stores RevertLanded for the worker's own goroutine; the fake itself is
// declared in revert_test.go with the rest of the revert hooks (gt-dci9k8).
func (f *fakeRevertHooks) IsRevert(string) bool { return f.isRevert }

// queueIdle waits until every job the queue held when it was called has
// returned: the marker job added behind them runs last, so its channel closing
// is the queue reaching that point.
func queueIdle(ctx context.Context, t *testing.T, q *BookQueue) {
	t.Helper()
	done := make(chan struct{})
	q.Add("queue-idle", func() { close(done) })
	recvBounded(t, ctx, done, "the queued bookkeeping returning")
}

// TestBookQueueWorkerRevert: a deferred landing's revert call is stored by its
// bookkeeping job and runs on the worker's own goroutine at the next pass, so
// RevertLanded never runs on the queue goroutine; a deferred landing that is no
// revert never calls it, and an inline landing is told inline as before
// (gt-dci9k8).
func TestBookQueueWorkerRevert(t *testing.T) {
	t.Parallel()
	rec := land.LandingRecord{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: noteHead,
		Target: "main", LandedCommit: "cccccccccc", Route: "daemon"}
	deferred := land.Result{LandedCommit: "cccccccccc", PatchID: "pppppppppp", Deferred: &rec}
	inline := land.Result{LandedCommit: "cccccccccc", PatchID: "pppppppppp"}

	// nextPass drops the ready label a landing would have taken off and runs
	// the next pass, which is where a call the queue stored runs.
	nextPass := func(t *testing.T, ctx context.Context, h *harness) {
		t.Helper()
		if err := h.bd.Update("gt-abc", beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
			t.Fatal(err)
		}
		h.w.Pass(ctx)
	}

	t.Run("a deferred revert's RevertLanded runs once, on the next pass", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		q := &BookQueue{}
		h, _, _ := deferredWorker(t, q, deferred)
		hooks := &fakeRevertHooks{isRevert: true}
		h.w.Reverts = hooks

		if rep := h.w.Pass(ctx); rep.Landed != 1 {
			t.Fatalf("report %v; want the landing counted", rep)
		}
		queueIdle(ctx, t, q)
		if len(hooks.landed) != 0 {
			t.Fatalf("RevertLanded ran %v while the queue job ran; want it stored for the worker's goroutine", hooks.landed)
		}

		nextPass(t, ctx, h)
		if want := []string{"gt-abc@cccccccccc"}; !slices.Equal(hooks.landed, want) {
			t.Fatalf("RevertLanded ran %v; want %v once, on the next pass", hooks.landed, want)
		}
	})

	t.Run("a deferred landing that is no revert never calls it", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		q := &BookQueue{}
		h, _, _ := deferredWorker(t, q, deferred)
		hooks := &fakeRevertHooks{isRevert: false}
		h.w.Reverts = hooks

		h.w.Pass(ctx)
		queueIdle(ctx, t, q)
		nextPass(t, ctx, h)
		if len(hooks.landed) != 0 {
			t.Fatalf("RevertLanded ran %v for a landing that is no revert", hooks.landed)
		}
	})

	t.Run("an inline landing calls it inline as before", func(t *testing.T) {
		t.Parallel()
		ctx := waitCtx(t)
		q := &BookQueue{}
		h, _, _ := deferredWorker(t, q, inline)
		hooks := &fakeRevertHooks{isRevert: true}
		h.w.Reverts = hooks

		if rep := h.w.Pass(ctx); rep.Landed != 1 {
			t.Fatalf("report %v; want the landing counted", rep)
		}
		if want := []string{"gt-abc@cccccccccc"}; !slices.Equal(hooks.landed, want) {
			t.Fatalf("RevertLanded ran %v; want %v inline, as before", hooks.landed, want)
		}
		if q.Holds("gt-abc") {
			t.Fatal("gt-abc was queued though its landing was not deferred")
		}
	})
}

// TestBookQueueFinishDeferred: the four landing steps run in order after the
// record is finished; a finish it cannot make queues the repair instead, and
// none of the four runs (gt-k78e8j).
func TestBookQueueFinishDeferred(t *testing.T) {
	t.Parallel()
	res := land.Result{LandedCommit: "cccccccccc"}
	work := land.Work{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: noteHead, Target: "main", Worker: "opal"}

	t.Run("the steps run after the record is finished, in order", func(t *testing.T) {
		t.Parallel()
		h, f := deferredHarness(t, nil)
		h.w.Books = &BookQueue{}
		h.w.finishDeferred(work, res)
		if want := []string{"finish", "reap", "seat", "sp-actual", "after"}; !slices.Equal(f.steps, want) {
			t.Fatalf("steps %v; want %v", f.steps, want)
		}
	})

	t.Run("a finish error runs none and RunLater queues the repair", func(t *testing.T) {
		t.Parallel()
		h, f := deferredHarness(t, errors.New("finish failed"))
		h.w.Books = &BookQueue{}
		h.w.finishDeferred(work, res)
		if !slices.Equal(f.steps, []string{"finish"}) {
			t.Fatalf("steps %v; want only the finish attempt", f.steps)
		}
		if len(h.w.pendingRepair) != 0 {
			t.Fatalf("pendingRepair %v before RunLater; want the repair stored with Later", h.w.pendingRepair)
		}
		h.w.Books.RunLater()
		if _, ok := h.w.pendingRepair["gt-abc"]; !ok {
			t.Fatalf("pendingRepair %v after RunLater; want gt-abc", h.w.pendingRepair)
		}
	})

	t.Run("a lander with no finisher queues the repair", func(t *testing.T) {
		t.Parallel()
		h, f := deferredHarness(t, nil)
		h.w.Lander = h.lander // the harness's fakeLander cannot finish a record
		h.w.Books = &BookQueue{}
		h.w.finishDeferred(work, res)
		if len(f.steps) != 0 {
			t.Fatalf("steps %v; want none", f.steps)
		}
		h.w.Books.RunLater()
		if _, ok := h.w.pendingRepair["gt-abc"]; !ok {
			t.Fatalf("pendingRepair %v; want gt-abc queued", h.w.pendingRepair)
		}
	})

	t.Run("with no queue the repair is written directly", func(t *testing.T) {
		t.Parallel()
		h, _ := deferredHarness(t, errors.New("finish failed"))
		h.w.finishDeferred(work, res)
		if _, ok := h.w.pendingRepair["gt-abc"]; !ok {
			t.Fatalf("pendingRepair %v; want the repair written directly", h.w.pendingRepair)
		}
	})
}
