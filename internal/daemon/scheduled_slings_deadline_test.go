package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A gate_timeout longer than the sling budget is honored, and the sling still
// gets its full budget after a slow gate (gt-0frud).
func TestScheduledGate_OwnDeadlineIsNotCappedByTheCallersContext(t *testing.T) {
	t.Parallel()
	e := gatedEntry
	e.GateTimeoutStr = "10m"
	f := &fakeScheduledRunner{createID: "gt-run1", now: scheduledGateNow, gateAnswers: []gateAnswer{{}}}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	d.scheduledSlingTimeout = time.Hour

	// The caller's context is nearly spent, as after a slow gate or listing.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := d.runScheduledSlingEntry(ctx, e, scheduledGateNow); err != nil {
		t.Fatal(err)
	}
	if len(f.gateDeadlines) != 1 || f.gateDeadlines[0] < 9*time.Minute {
		t.Errorf("gate context deadlines = %v, want about the configured 10m", f.gateDeadlines)
	}
	if len(f.slingBudgets) != 1 || f.slingBudgets[0] < 59*time.Minute {
		t.Errorf("sling budgets = %v, want a fresh budget after the gate", f.slingBudgets)
	}
}

func TestScheduledGate_TimeoutMessageNamesTheConfiguredBound(t *testing.T) {
	t.Parallel()
	got := classifyScheduledGate([]byte("scanning"), context.DeadlineExceeded, nil, 10*time.Minute)
	if got.verdict != scheduledGateFailed || !strings.HasPrefix(got.detail, "timed out after 10m0s") {
		t.Errorf("outcome %+v, want a failure that names the 10m0s bound", got)
	}
}

// slingKilledByDeadline is a sling that hooked the bead (or not), then ran past
// the context's deadline.
func slingKilledByDeadline(hooked bool) func(context.Context, *fakeScheduledRunner, string) error {
	return func(ctx context.Context, f *fakeScheduledRunner, id string) error {
		if hooked {
			f.setStatus(id, "hooked")
		}
		<-ctx.Done()
		return errors.New("gt sling: signal: killed")
	}
}

func TestRunScheduledSlingEntry_SlingKilledAfterHookKeepsTheRunBeadOpen(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now(), slingHook: slingKilledByDeadline(true)}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	d.scheduledSlingTimeout = 20 * time.Millisecond

	err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now())
	if err == nil || !strings.Contains(err.Error(), "kept open") {
		t.Fatalf("error = %v, want the timed-out sling reported with the bead kept open", err)
	}
	if len(f.closedIDs) != 0 {
		t.Errorf("a hooked run bead must not be failure-closed, closed %v", f.closedIDs)
	}
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || len(f.slungIDs) != 1 {
		t.Errorf("next tick dispatched the run again: created %v slung %v", f.created, f.slungIDs)
	}
}

func TestRunScheduledSlingEntry_SlingKilledBeforeHookIsRetried(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now(), slingHook: slingKilledByDeadline(false)}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	d.scheduledSlingTimeout = 20 * time.Millisecond

	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err == nil {
		t.Fatal("a failed sling must be reported")
	}
	if len(f.closedIDs) != 1 || !strings.HasPrefix(f.closedWhy[0], scheduledSlingFailureMarker) {
		t.Errorf("a bead the sling never hooked is failure-closed so the next tick retries: closed %v %v", f.closedIDs, f.closedWhy)
	}
}

func TestRunScheduledSlingEntry_SlingKilledWithUnreadableRunKeepsTheBeadOpen(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	f.slingHook = func(ctx context.Context, f *fakeScheduledRunner, id string) error {
		<-ctx.Done()
		f.listErr = errors.New("dolt down")
		return errors.New("gt sling: signal: killed")
	}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	d.scheduledSlingTimeout = 20 * time.Millisecond

	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err == nil {
		t.Fatal("a failed sling must be reported")
	}
	if len(f.closedIDs) != 0 {
		t.Errorf("a run that cannot be read back is kept open, closed %v", f.closedIDs)
	}
}

// Keeping the bead open is the fail-safe for a run that may have started, but
// when the sling never hooked it the bead is a leftover, and a later tick has
// to recover it: the timeout path leaves exactly the open, unhooked bead that
// used to read as skip-open forever (gt-g16z2).
func TestRunScheduledSlingEntry_ATimedOutSlingThatNeverHookedIsRecovered(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	f.slingHook = func(ctx context.Context, f *fakeScheduledRunner, id string) error {
		<-ctx.Done()
		f.listErr = errors.New("dolt down")
		return errors.New("gt sling: signal: killed")
	}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	d.scheduledSlingTimeout = 20 * time.Millisecond

	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err == nil {
		t.Fatal("a failed sling must be reported")
	}

	// The read works again and the bead is still open and unhooked, as it is
	// when the sling died before hooking it.
	f.listErr = nil
	f.beads[0].CreatedAt = time.Now().Add(-d.scheduledStaleRunAfter() - time.Minute)
	err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now())
	if err == nil || !strings.Contains(err.Error(), "left open and unhooked") {
		t.Fatalf("error = %v, want the stalled bead recovered and reported", err)
	}
	if len(f.closedIDs) != 1 || f.closedIDs[0] != "gt-run1" || !strings.HasPrefix(f.closedWhy[0], scheduledSlingFailureMarker) {
		t.Fatalf("the stalled bead must be retired with the failure marker: closed %v %v", f.closedIDs, f.closedWhy)
	}

	f.slingHook, f.createID = nil, "gt-run2" // the retry slings a fresh bead
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.slungIDs) != 2 || f.slungIDs[1] != "gt-run2" {
		t.Errorf("the tick after the recovery must run the entry again, slung %v", f.slungIDs)
	}
}
