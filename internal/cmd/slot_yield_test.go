package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/slot"
)

// gateRunningReport is the town's pool with the refinery on reserved slot 0 and
// the pool yielding to it (gt-22hdp.29).
func gateRunningReport() slot.Report {
	gate := &slot.Owner{Role: "gastown/refinery", PID: 4242, AcquiredAt: time.Now().Add(-5 * time.Minute), Slot: 0}
	return slot.Report{
		Held:           true,
		Owner:          gate,
		Slots:          []slot.SlotState{{Index: 0, Held: true, Owner: gate}, {Index: 1}, {Index: 2}, {Index: 3}},
		HeldCount:      1,
		Total:          4,
		Reserved:       2,
		YieldingToGate: true,
		GateHolder:     gate,
	}
}

// TestPrintSlotStatusText_ShowsWaitingGateRunning: gt slot status says why a
// new crew or agent suite would wait, and on whom.
func TestPrintSlotStatusText_ShowsWaitingGateRunning(t *testing.T) {
	t.Parallel()
	out := slotStatusTextOf(t, gateRunningReport())
	if !strings.Contains(out, "waiting: gate running") || !strings.Contains(out, "gastown/refinery (pid 4242, slot 0)") {
		t.Errorf("status with a gate running does not explain the wait:\n%s", out)
	}

	idle := gateRunningReport()
	idle.YieldingToGate, idle.GateHolder = false, nil
	if out := slotStatusTextOf(t, idle); strings.Contains(out, "gate running") {
		t.Errorf("status claims a yield the report does not carry:\n%s", out)
	}
}

// TestPrintSlotStatusJSON_CarriesTheGateYield pins the wire fields.
func TestPrintSlotStatusJSON_CarriesTheGateYield(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := printSlotStatusJSON(cmd, gateRunningReport(), nil); err != nil {
		t.Fatal(err)
	}
	var got struct {
		YieldingToGate bool        `json:"yielding_to_gate"`
		GateHolder     *slot.Owner `json:"gate_holder"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if !got.YieldingToGate || got.GateHolder == nil || got.GateHolder.Role != "gastown/refinery" {
		t.Fatalf("json = %s, want yielding_to_gate and the gate holder", buf.String())
	}
}

// TestSlotHistoryReason_GateRunning: a history entry for a wait spent yielding
// names the gate.
func TestSlotHistoryReason_GateRunning(t *testing.T) {
	t.Parallel()
	e := slot.HistoryEntry{Reason: slot.WaitReasonGateRunning, HolderRole: "gastown/refinery", HolderPID: 4242}
	if got := slotHistoryReason(e); got != "gate_running: gastown/refinery (pid 4242)" {
		t.Fatalf("slotHistoryReason = %q", got)
	}
	if got := slotHistoryReason(slot.HistoryEntry{Reason: slot.WaitReasonGateRunning}); got != "gate_running" {
		t.Fatalf("slotHistoryReason without a holder = %q", got)
	}
}

// TestSyncGateIntent: gt mq next is the refinery's per-MR touch point, so it
// registers the rig's pending gate when the refinery picks a ready MR and
// clears it when the queue is empty — no formula change needed. Anyone else
// running gt mq next (an operator, a dog) never registers one.
func TestSyncGateIntent(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	pool := slot.Pool{Slots: 4, ReservedForGate: 2, YieldToGate: true}
	pending := func() *slot.GateIntent {
		t.Helper()
		rep, err := slot.StatusPoolLocksOnly(town, pool)
		if err != nil {
			t.Fatal(err)
		}
		return rep.GatePending
	}

	syncGateIntent(town, "gastown", "gastown/crew/sloan", "gt-wisp-1")
	if p := pending(); p != nil {
		t.Fatalf("a crew caller registered a gate intent: %+v", p)
	}

	syncGateIntent(town, "gastown", "gastown/refinery", "gt-wisp-1")
	if p := pending(); p == nil || p.Role != "gastown/refinery" || p.Ref != "gt-wisp-1" {
		t.Fatalf("refinery picking an MR: intent = %+v", p)
	}

	syncGateIntent(town, "gastown", "gastown/crew/sloan", "")
	if p := pending(); p != nil {
		t.Fatalf("an empty queue left the intent registered: %+v", p)
	}
}

// TestPrintSlotStatusText_ShowsWaitingGatePending: with no gate holding a slot
// yet but one registered, status says new suites wait on the pending gate.
func TestPrintSlotStatusText_ShowsWaitingGatePending(t *testing.T) {
	t.Parallel()
	rep := slot.Report{
		Slots:          []slot.SlotState{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}},
		Total:          4,
		Reserved:       2,
		YieldingToGate: true,
		GatePending:    &slot.GateIntent{Role: "gastown/refinery", Ref: "gt-wisp-rpf", RegisteredAt: time.Now().Add(-2 * time.Minute), ExpiresAt: time.Now().Add(28 * time.Minute)},
	}
	out := slotStatusTextOf(t, rep)
	if !strings.Contains(out, "waiting: gate pending") || !strings.Contains(out, "gastown/refinery") || !strings.Contains(out, "gt-wisp-rpf") {
		t.Errorf("status with a pending gate does not explain the wait:\n%s", out)
	}
	if got := slotHistoryReason(slot.HistoryEntry{Reason: slot.WaitReasonGatePending, HolderRole: "gastown/refinery"}); got != "gate_pending: gastown/refinery" {
		t.Errorf("slotHistoryReason(gate_pending) = %q", got)
	}
}

// TestQueueHasReadyMR (I1): the check gt mq list uses to clear a stale gate
// intent once the rig's queue has no ready MR left. A blocked MR, a closed
// one or another rig's MR does not keep the intent alive.
func TestQueueHasReadyMR(t *testing.T) {
	t.Parallel()
	ready := &beads.Issue{ID: "gt-1", Status: "open", Description: "branch: b\nrig: gastown"}
	blocked := &beads.Issue{ID: "gt-2", Status: "open", BlockedByCount: 1, Description: "rig: gastown"}
	closed := &beads.Issue{ID: "gt-3", Status: "closed", Description: "rig: gastown"}
	otherRig := &beads.Issue{ID: "hm-1", Status: "open", Description: "rig: hm"}

	if !queueHasReadyMR([]*beads.Issue{blocked, ready}, "gastown") {
		t.Error("a ready MR was not seen")
	}
	if queueHasReadyMR([]*beads.Issue{blocked, closed, otherRig}, "gastown") {
		t.Error("blocked, closed and other-rig MRs counted as ready")
	}
	if queueHasReadyMR(nil, "gastown") {
		t.Error("an empty queue counted as ready")
	}
}

// TestMQListCoversQueue (I1): gt mq list may clear the intent only when its
// listing is the whole open queue; a --worker, --epic or non-open --status
// view says nothing about the rest of the queue.
func TestMQListCoversQueue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status, worker, epic string
		want                 bool
	}{
		{"", "", "", true},
		{"open", "", "", true},
		{"closed", "", "", false},
		{"all", "", "", false},
		{"", "amber", "", false},
		{"", "", "gt-epic", false},
	}
	for _, c := range cases {
		if got := mqListCoversQueue(c.status, c.worker, c.epic); got != c.want {
			t.Errorf("mqListCoversQueue(%q, %q, %q) = %v, want %v", c.status, c.worker, c.epic, got, c.want)
		}
	}
}

// TestSyncGateIntent_OnlyMergeGateRegisters (M3): the daemon's main-branch
// test is not on the merge path and never registers a gate intent.
func TestSyncGateIntent_OnlyMergeGateRegisters(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	pool := slot.Pool{Slots: 4, ReservedForGate: 2, YieldToGate: true}
	syncGateIntent(town, "gastown", "gastown/main-branch-test", "gt-wisp-1")
	rep, err := slot.StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GatePending != nil {
		t.Fatalf("main-branch-test registered an intent: %+v", rep.GatePending)
	}
}
