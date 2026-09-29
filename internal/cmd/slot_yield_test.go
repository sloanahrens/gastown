package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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
