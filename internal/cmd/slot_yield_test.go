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

// gateRunningReport is the town's pool with the landing worker on reserved slot 0 and
// the pool yielding to it (gt-22hdp.29).
func gateRunningReport() slot.Report {
	gate := &slot.Owner{Role: "gastown/landing", PID: 4242, AcquiredAt: time.Now().Add(-5 * time.Minute), Slot: 0}
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
	if !strings.Contains(out, "waiting: gate running") || !strings.Contains(out, "gastown/landing (pid 4242, slot 0)") {
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
	if !got.YieldingToGate || got.GateHolder == nil || got.GateHolder.Role != "gastown/landing" {
		t.Fatalf("json = %s, want yielding_to_gate and the gate holder", buf.String())
	}
}

// TestSlotHistoryReason_GateRunning: a history entry for a wait spent yielding
// names the gate.
func TestSlotHistoryReason_GateRunning(t *testing.T) {
	t.Parallel()
	e := slot.HistoryEntry{Reason: slot.WaitReasonGateRunning, HolderRole: "gastown/landing", HolderPID: 4242}
	if got := slotHistoryReason(e); got != "gate_running: gastown/landing (pid 4242)" {
		t.Fatalf("slotHistoryReason = %q", got)
	}
	if got := slotHistoryReason(slot.HistoryEntry{Reason: slot.WaitReasonGateRunning}); got != "gate_running" {
		t.Fatalf("slotHistoryReason without a holder = %q", got)
	}
}

// TestSlotHistoryReason_FullSuiteHeld: a history entry for a wait behind the
// whole-tree cap names the run it ceded to (gt-dhcmp).
func TestSlotHistoryReason_FullSuiteHeld(t *testing.T) {
	t.Parallel()
	e := slot.HistoryEntry{Reason: slot.WaitReasonFullSuiteHeld, HolderRole: "gastown/tier-sweep", HolderPID: 4242}
	if got := slotHistoryReason(e); got != "full_suite_held: gastown/tier-sweep (pid 4242)" {
		t.Fatalf("slotHistoryReason = %q", got)
	}
	if got := slotHistoryReason(slot.HistoryEntry{Reason: slot.WaitReasonFullSuiteHeld}); got != "full_suite_held" {
		t.Fatalf("slotHistoryReason without a holder = %q", got)
	}
}
