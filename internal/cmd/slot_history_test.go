package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
)

// slotStatusJSONOf renders printSlotStatusJSON into a string, the way
// `gt slot status --json` would. The command needs no workspace: only the
// report and the ring file's entries are rendered here.
func slotStatusJSONOf(t *testing.T, rep slot.Report, history []slot.HistoryEntry) string {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := printSlotStatusJSON(cmd, rep, history); err != nil {
		t.Fatalf("printSlotStatusJSON: %v", err)
	}
	return buf.String()
}

func secondsPtr(s float64) *float64 { return &s }

// TestPrintSlotStatusJSON_CarriesTheHistory is gt-dc81's acceptance for the
// status half: an operator must be able to compute wait percentiles from the
// JSON alone, with the wait reason and its evidence attached to each entry —
// the amendment's whole point is that a 29-minute wait says what it waited for.
func TestPrintSlotStatusJSON_CarriesTheHistory(t *testing.T) {
	history := []slot.HistoryEntry{
		{
			TS: "2026-09-10T20:39:00Z", Role: "gastown/refinery", Slot: 0, PID: 62965,
			WaitedS: 1740, HeldS: secondsPtr(723), TimeoutS: 3600,
			Reason: slot.WaitReasonTokenHeld, HolderRole: "gastown/polecats/mica", HolderPID: 41234,
		},
		{
			TS: "2026-09-10T21:08:00Z", Role: "gastown/amber", Slot: 0, PID: 5,
			WaitedS: 0.4, HeldS: secondsPtr(61), TimeoutS: 3600,
		},
		{
			TS: "2026-09-10T21:09:00Z", Role: "gastown/opal", Slot: 0, PID: 6,
			WaitedS: 60, TimeoutS: 60, TimedOut: true,
			Reason: slot.WaitReasonUnwrappedContainers, Containers: []string{"dolt/dolt-sql-server:2.2.0 stray-suite"},
		},
	}

	out := slotStatusJSONOf(t, slot.Report{Total: 1}, history)

	var decoded struct {
		History []struct {
			Role       string   `json:"role"`
			WaitedS    float64  `json:"waited_s"`
			HeldS      *float64 `json:"held_s"`
			TS         string   `json:"ts"`
			Reason     string   `json:"reason"`
			TimedOut   bool     `json:"timed_out"`
			HolderRole string   `json:"holder_role"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if len(decoded.History) != 3 {
		t.Fatalf("history entries = %d, want 3: %s", len(decoded.History), out)
	}
	first := decoded.History[0]
	if first.Role != "gastown/refinery" || first.WaitedS != 1740 || first.TS == "" {
		t.Errorf("first history entry lost its facts: %+v", first)
	}
	if first.HeldS == nil || *first.HeldS != 723 {
		t.Errorf("first history entry held_s = %v, want 723", first.HeldS)
	}
	if first.Reason != string(slot.WaitReasonTokenHeld) || first.HolderRole != "gastown/polecats/mica" {
		t.Errorf("first history entry lost its wait reason: %+v", first)
	}
	if decoded.History[1].Reason != "" {
		t.Errorf("an uncontended acquisition reported reason %q, want none", decoded.History[1].Reason)
	}
	if !decoded.History[2].TimedOut {
		t.Errorf("a caller that gave up is not marked timed_out: %+v", decoded.History[2])
	}
}

// TestPrintSlotStatusJSON_OmitsAnEmptyHistory keeps a town that has never run a
// container-backed suite from reporting a history of nothing.
func TestPrintSlotStatusJSON_OmitsAnEmptyHistory(t *testing.T) {
	out := slotStatusJSONOf(t, slot.Report{Total: 1}, nil)
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if _, present := decoded["history"]; present {
		t.Errorf("empty history was rendered anyway: %s", out)
	}
}

// TestPrintSlotHistory covers the plain-text half: the distribution line, the
// reason detail per entry, a hold whose owner was killed mid-suite, and a
// caller that gave up.
func TestPrintSlotHistory(t *testing.T) {
	open := slot.HistoryEntry{
		TS: "2026-09-10T20:39:00Z", Role: "gastown/refinery", Slot: 0, PID: 62965,
		WaitedS: 1740, TimeoutS: 3600,
		Reason: slot.WaitReasonTokenHeld, HolderRole: "gastown/polecats/mica", HolderPID: 41234,
	}
	history := []slot.HistoryEntry{
		{TS: "2026-09-10T21:00:00Z", Role: "gastown/amber", Slot: 0, PID: 5, WaitedS: 0, HeldS: secondsPtr(5)},
		{
			TS: "2026-09-10T21:05:00Z", Role: "gastown/pearl", Slot: 0, PID: 6, WaitedS: 12,
			Reason: slot.WaitReasonUnwrappedContainers, Containers: []string{"dolt/dolt-sql-server:2.2.0 stray-suite"},
		},
		{
			TS: "2026-09-10T21:06:00Z", Role: "gastown/agate", Slot: 0, PID: 7, WaitedS: 30,
			Reason: slot.WaitReasonDaemonUnreachable, DockerError: "docker ps did not respond within 5s",
		},
		{
			TS: "2026-09-10T21:07:00Z", Role: "gastown/opal", Slot: 1, PID: 8, WaitedS: 60,
			TimeoutS: 60, TimedOut: true, Reason: slot.WaitReasonUnwrappedContainers,
		},
		open,
	}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printSlotHistory(cmd, history)
	out := buf.String()

	for _, want := range []string{
		"p50", "p95", "max", // the distribution an operator reads first
		"held open (holder never released)", // the SIGKILLed holder
		"gave up",                           // the caller that timed out
		"token_held by gastown/polecats/mica (pid 41234)",
		"unwrapped_containers: dolt/dolt-sql-server:2.2.0 stray-suite",
		"daemon_unreachable: docker ps did not respond within 5s",
	} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("printSlotHistory output missing %q:\n%s", want, out)
		}
	}
	// The summary is computed over the whole ring, not just the entries shown.
	for _, want := range []string{"n=5", "max 29m0s"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("printSlotHistory summary missing %q:\n%s", want, out)
		}
	}
}

// TestPrintSlotHistory_ShowsAtMostTheRecentTail keeps the live picture (held,
// by whom) at the top of `gt slot status` for a ring file that has filled up.
func TestPrintSlotHistory_ShowsAtMostTheRecentTail(t *testing.T) {
	history := make([]slot.HistoryEntry, 0, slotHistoryShown+3)
	for i := 0; i < slotHistoryShown+3; i++ {
		history = append(history, slot.HistoryEntry{
			TS:   time.Now().Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			Role: "gastown/refinery", Slot: 0, PID: i, WaitedS: float64(i),
		})
	}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printSlotHistory(cmd, history)

	lines := bytes.Count([]byte(buf.String()), []byte("\n"))
	if lines != 1+slotHistoryShown {
		t.Errorf("printSlotHistory rendered %d lines for %d entries, want %d", lines, len(history), 1+slotHistoryShown)
	}
}

// TestPrintSlotHistory_Empty renders nothing for a town with no history, rather
// than a header with no entries under it.
func TestPrintSlotHistory_Empty(t *testing.T) {
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printSlotHistory(cmd, nil)
	if buf.Len() != 0 {
		t.Errorf("printSlotHistory(nil) wrote %q, want nothing", buf.String())
	}
}
