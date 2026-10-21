package done

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/slot"
)

// slotProbeGate records the presubmit pool as the gate sees it while it runs.
type slotProbeGate struct {
	townRoot string
	during   slot.Report
	err      error
}

func (g *slotProbeGate) Run(_ context.Context, _ string) land.GateResult {
	g.during, g.err = slot.StatusPresubmit(g.townRoot, slot.PresubmitSize(g.townRoot))
	return land.GateResult{Passed: true, Steps: []land.StepResult{{Name: "presubmit"}}}
}

// gt done's local gate runs while it holds a presubmit slot, and the slot is
// released once the gate has finished.
func TestRunDoneLocalGateHoldsPresubmitSlot(t *testing.T) {
	h := newSubmitHarness(t)
	h.r.sender = "gastown/polecats/refuge"
	probe := &slotProbeGate{townRoot: h.townRoot}
	h.r.deps.localGate = func(string, string, string) (land.Gate, error) { return probe, nil }

	if err := runDoneLocalGate(h.r, "abc1234"); err != nil {
		t.Fatalf("runDoneLocalGate: %v", err)
	}
	if probe.err != nil {
		t.Fatalf("StatusPresubmit during the gate: %v", probe.err)
	}
	if probe.during.HeldCount != 1 {
		t.Fatalf("presubmit slots held during the gate = %d, want 1", probe.during.HeldCount)
	}
	if probe.during.Owner == nil || probe.during.Owner.Role != h.r.sender {
		t.Errorf("presubmit holder = %+v, want role %q", probe.during.Owner, h.r.sender)
	}

	after, err := slot.StatusPresubmit(h.townRoot, slot.PresubmitSize(h.townRoot))
	if err != nil {
		t.Fatalf("StatusPresubmit after the gate: %v", err)
	}
	if after.HeldCount != 0 {
		t.Errorf("presubmit slots held after the gate = %d, want 0", after.HeldCount)
	}
}

// A presubmit wait is spent from the gate budget: once that budget is gone
// the gate does not start and no slot is taken.
func TestAcquirePresubmitSlotRefusesSpentBudget(t *testing.T) {
	h := newSubmitHarness(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if _, err := acquirePresubmitSlot(ctx, h.r); err != context.DeadlineExceeded {
		t.Fatalf("acquirePresubmitSlot with a spent budget: err = %v, want %v", err, context.DeadlineExceeded)
	}
	rep, err := slot.StatusPresubmit(h.townRoot, slot.PresubmitSize(h.townRoot))
	if err != nil {
		t.Fatalf("StatusPresubmit: %v", err)
	}
	if rep.HeldCount != 0 {
		t.Errorf("a spent budget still took %d presubmit slot(s)", rep.HeldCount)
	}
}
