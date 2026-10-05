package landworker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// pushRefusedErr is the failure that motivated the rule: the candidate's push
// is refused by a hook, the worker backs off and retries, and nothing else
// says the landing is failing (gt-fn9e6.37).
func pushRefusedErr() error {
	return &land.InfraError{Stage: "push", Err: errors.New("the pre-push hook refused land/gt-abc")}
}

type fakeBackoff struct {
	mu     sync.Mutex
	states []land.BackoffState
}

func (f *fakeBackoff) Write(st land.BackoffState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, st)
	return nil
}

func (f *fakeBackoff) last(t *testing.T) land.BackoffState {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.states) == 0 {
		t.Fatal("the worker wrote no backoff snapshot")
	}
	return f.states[len(f.states)-1]
}

// escalateRecorder collects the escalations a pass raises, and the general
// failing-landing clears, in the order they arrive.
type escalateRecorder struct {
	mu       sync.Mutex
	failing  []string
	cleared  []string
	escalate []string
}

func (r *escalateRecorder) failingRaise(beadID, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing = append(r.failing, beadID+": "+message)
}

func (r *escalateRecorder) clear(beadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared = append(r.cleared, beadID)
}

func (r *escalateRecorder) specs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.failing...)
}

func (r *escalateRecorder) clears() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cleared...)
}

func (r *escalateRecorder) general() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.escalate...)
}

// TestPassRepeatedFailuresEscalateOnceAndClearOnLanding: a landing failing at
// any stage DefaultFailingEscalateAfter times in a row raises exactly one
// escalation naming the stage, the count and the error, and the bead's
// landing clears it (gt-fn9e6.44).
func TestPassRepeatedFailuresEscalateOnceAndClearOnLanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	rec := &escalateRecorder{}
	h.w.FailingEscalate = rec.failingRaise
	h.w.FailingClear = rec.clear
	h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, pushRefusedErr() }

	for i := 0; i < DefaultFailingEscalateAfter+2; i++ {
		h.w.Pass(context.Background())
		h.w.escWG.Wait()
		want := 0
		if i+1 >= DefaultFailingEscalateAfter {
			want = 1
		}
		if got := rec.specs(); len(got) != want {
			t.Fatalf("after %d failures: %d escalations (%v), want %d", i+1, len(got), got, want)
		}
		h.now = h.now.Add(infraBackoffMax)
	}
	got := rec.specs()[0]
	for _, want := range []string{"gt-abc", "3 times in a row", "push", "the pre-push hook refused land/gt-abc"} {
		if !strings.Contains(got, want) {
			t.Fatalf("escalation %q; want it to name %q", got, want)
		}
	}

	// The bead lands: the run of failures is over, so the escalation closes.
	h.lander.fn = nil
	h.w.Pass(context.Background())
	h.w.escWG.Wait()
	if c := rec.clears(); len(c) != 1 || c[0] != "gt-abc" {
		t.Fatalf("clears %v; want the landed bead cleared once", c)
	}
}

// TestPassRejectionEndsTheFailureRun: a rejection ends the run of failures as
// a landing does, so the escalation closes and a resubmitted bead's failures
// count from one again (gt-fn9e6.44).
func TestPassRejectionEndsTheFailureRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	rec := &escalateRecorder{}
	h.w.FailingEscalate = rec.failingRaise
	h.w.FailingClear = rec.clear
	h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, pushRefusedErr() }
	for i := 0; i < DefaultFailingEscalateAfter; i++ {
		h.w.Pass(context.Background())
		h.w.escWG.Wait()
		h.now = h.now.Add(infraBackoffMax)
	}
	if got := rec.specs(); len(got) != 1 {
		t.Fatalf("escalations %v; want one for the run of %d failures", got, DefaultFailingEscalateAfter)
	}

	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectGate, Reason: "gate failed on the merged tree", Rework: true}
	}
	h.w.Pass(context.Background())
	h.w.escWG.Wait()
	if c := rec.clears(); len(c) != 1 || c[0] != "gt-abc" {
		t.Fatalf("clears %v; want the rejected bead cleared once", c)
	}
}

// TestPassLintTimeoutDoesNotAlsoEscalateGenerally: a bead that escalated for
// its lint stage has raised its escalation for this run of failures, so the
// general rule stays quiet at the same count (gt-fn9e6.44).
func TestPassLintTimeoutDoesNotAlsoEscalateGenerally(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	rec := &escalateRecorder{}
	h.w.Escalate = func(_ string, message string) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.escalate = append(rec.escalate, message)
	}
	h.w.FailingEscalate = rec.failingRaise
	h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, lintTimeoutErr() }

	for i := 0; i < DefaultLintTimeoutEscalateAfter+2; i++ {
		h.w.Pass(context.Background())
		h.w.escWG.Wait()
		h.now = h.now.Add(infraBackoffMax)
	}
	if got := rec.general(); len(got) != 1 || !strings.Contains(got[0], "lint") {
		t.Fatalf("lint escalations %v; want exactly the one naming the lint stage", got)
	}
	if got := rec.specs(); len(got) != 0 {
		t.Fatalf("general escalations %v; want none beside the lint one", got)
	}
}

// TestWriteBackoffNamesTheFailingLanding: the snapshot the dashboard and the
// health field read carries the bead, the stage, the run of failures, the
// next try and the error's first line, and drops the bead once it lands
// (gt-fn9e6.44).
func TestWriteBackoffNamesTheFailingLanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	backoff := &fakeBackoff{}
	h.w.Backoff = backoff
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.InfraError{Stage: "push", Err: errors.New("refused\nsecond line")}
	}

	h.w.Pass(context.Background())
	snap := backoff.last(t)
	if snap.Rig != "gastown" || !snap.At.Equal(h.now) {
		t.Fatalf("snapshot %+v; want the rig and the pass's clock", snap)
	}
	if len(snap.Beads) != 1 {
		t.Fatalf("snapshot %+v; want one failing landing", snap)
	}
	got := snap.Beads[0]
	if got.BeadID != "gt-abc" || got.Stage != "push" || got.Failures != 1 || !got.NextTry.Equal(h.now.Add(infraBackoffBase)) {
		t.Fatalf("record %+v; want gt-abc at push, 1 failure, next try in %s", got, infraBackoffBase)
	}
	if got.Error != "landing failed at push: refused" {
		t.Fatalf("error %q; want the error's first line alone", got.Error)
	}

	// A successful landing is not a failure waiting to be retried.
	h.lander.fn = nil
	h.now = h.now.Add(infraBackoffMax)
	h.w.Pass(context.Background())
	if snap := backoff.last(t); len(snap.Beads) != 0 {
		t.Fatalf("snapshot %+v; want the landed bead gone", snap)
	}
}

// TestWriteBackoffEmptyOnAHealthyRig: a rig whose landings all succeed writes
// an empty snapshot, which is what tells its readers nothing is failing.
func TestWriteBackoffEmptyOnAHealthyRig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	backoff := &fakeBackoff{}
	h.w.Backoff = backoff

	h.w.Pass(context.Background())
	snap := backoff.last(t)
	if snap.Rig != "gastown" || len(snap.Beads) != 0 {
		t.Fatalf("snapshot %+v; want an empty one for the rig", snap)
	}
}

// TestWriteBackoffHoldsOnlyWaitingBeads: a bead whose retry has come due is
// the next pass's work, not a landing that is still backing off.
func TestWriteBackoffHoldsOnlyWaitingBeads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	backoff := &fakeBackoff{}
	h.w.Backoff = backoff
	h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, pushRefusedErr() }
	h.w.Pass(context.Background())

	// Past the backoff: the retry is the next pass's work, not a landing
	// still waiting, so the record leaves the snapshot.
	h.now = h.now.Add(time.Hour)
	h.w.writeBackoff()
	if snap := backoff.last(t); len(snap.Beads) != 0 {
		t.Fatalf("snapshot %+v; want the due retry gone", snap)
	}
}
