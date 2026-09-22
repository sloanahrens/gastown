package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
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
// reason detail per entry, an open hold the pool still counts, and a caller
// that gave up.
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

	// The pool the history is read against: slot 0 is still the open entry's
	// holder, so that entry is an open hold rather than a phantom one.
	rep := slot.Report{Total: 1, HeldCount: 1, Held: true, Slots: []slot.SlotState{
		{Index: 0, Held: true, Owner: &slot.Owner{Role: "gastown/refinery", PID: 62965}},
	}}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printSlotHistory(cmd, rep, history)
	out := buf.String()

	for _, want := range []string{
		"p50", "p95", "max", // the distribution an operator reads first
		"held open (pid 62965 holds slot 0 now)", // the hold the pool still counts
		"gave up",                                // the caller that timed out
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
	printSlotHistory(cmd, slot.Report{Total: 1}, history)

	lines := bytes.Count([]byte(buf.String()), []byte("\n"))
	if lines != 1+slotHistoryShown {
		t.Errorf("printSlotHistory rendered %d lines for %d entries, want %d", lines, len(history), 1+slotHistoryShown)
	}
}

// slotHistoryHelperEnvVar and slotHistoryTownRootEnvVar trigger
// TestHelperHoldBatchSlotUntilKilled when it is re-executed as a subprocess of
// TestPrintSlotHistory_AbandonedAfterHolderIsKilled. Deliberately NOT
// GT_-prefixed (gt-tuiy attempt 4): this package's TestMain
// (hermetic_main_test.go) scrubs every GT_*/BD_*/BEADS_* variable from the
// process environment before running any test, including in the child, since
// the child is the same test binary re-executed and runs the same TestMain.
const (
	slotHistoryHelperEnvVar   = "SLOT_HISTORY_HELPER"
	slotHistoryTownRootEnvVar = "SLOT_HISTORY_TOWN_ROOT"
)

// TestPrintSlotHistory_AbandonedAfterHolderIsKilled is gt-2tqe's acceptance,
// end to end: a batch holder takes the slot, dies by SIGKILL with no Release
// (so no held_s is ever written), and the line `gt slot status` prints for it
// must say abandoned — not "held open", which is how the phantom entry came to
// claim a holder the pool itself did not count while the header read 0/5 held.
func TestPrintSlotHistory_AbandonedAfterHolderIsKilled(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	bin, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}
	holder := exec.Command(bin, "-test.run=TestHelperHoldBatchSlotUntilKilled")
	holder.Env = append(os.Environ(), slotHistoryHelperEnvVar+"=1", slotHistoryTownRootEnvVar+"="+townRoot)
	if err := holder.Start(); err != nil {
		t.Fatalf("starting batch holder helper: %v", err)
	}
	defer func() { _ = holder.Process.Kill() }()

	// Wait for the ring entry, not merely the held lock: Acquire takes the
	// flock before it records the grant, so killing on the lock alone can land
	// inside that window and leave no entry to read. The entry is written while
	// the flock is held, so its arrival implies the holder is holding.
	waitForHistoryEntry(t, townRoot)

	if err := holder.Process.Kill(); err != nil { // SIGKILL — no Release, no held_s
		t.Fatalf("killing batch holder: %v", err)
	}
	_ = holder.Wait()
	waitForSlotFree(t, townRoot)

	history, err := slot.History(townRoot)
	if err != nil {
		t.Fatalf("slot.History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history holds %d entries, want the one grant: %+v", len(history), history)
	}
	if history[0].HeldS != nil {
		t.Fatalf("the hold was closed by a Release that should never have run: %+v", history[0])
	}
	rep, err := slot.StatusPoolLocksOnly(townRoot, containerGatePool(townRoot))
	if err != nil {
		t.Fatalf("slot.StatusPoolLocksOnly: %v", err)
	}
	if rep.HeldCount != 0 {
		t.Fatalf("pool still counts the killed holder: %+v", rep)
	}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printSlotHistory(cmd, rep, history)
	out := buf.String()

	if !strings.Contains(out, "gastown/refinery-batch slot 0: waited") {
		t.Fatalf("the killed batch holder left no history line:\n%s", out)
	}
	if !strings.Contains(out, "abandoned (") {
		t.Errorf("the killed holder's hold is not reported abandoned:\n%s", out)
	}
	if strings.Contains(out, "held open") {
		t.Errorf("the killed holder's hold still claims to be held open under a 0/%d-held header:\n%s", rep.Total, out)
	}

	var decoded struct {
		History []struct {
			Role       string `json:"role"`
			Resolution string `json:"resolution"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(slotStatusJSONOf(t, rep, history)), &decoded); err != nil {
		t.Fatalf("decoding slot status --json: %v", err)
	}
	if len(decoded.History) != 1 {
		t.Fatalf("json history = %+v, want the one killed hold", decoded.History)
	}
	if got := decoded.History[0].Resolution; got != string(slot.HoldAbandoned) {
		t.Errorf("json resolution = %q, want %q", got, slot.HoldAbandoned)
	}
}

// waitForHistoryEntry polls the ring file until the holder's grant is on
// record. Grant is a two-step in the holder — flock first, entry second — and
// only the second step is what the history renders, so a test that kills the
// holder must wait for it rather than for the lock.
func waitForHistoryEntry(t *testing.T, townRoot string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		history, err := slot.History(townRoot)
		if err != nil {
			t.Fatalf("slot.History: %v", err)
		}
		if len(history) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the batch holder never recorded its grant in the ring file")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForSlotFree polls the lock picture until the pool no longer counts any
// holder. The kill releases the flock through the kernel, which the parent sees
// as soon as its non-blocking probe runs.
func waitForSlotFree(t *testing.T, townRoot string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rep, err := slot.StatusPoolLocksOnly(townRoot, containerGatePool(townRoot))
		if err != nil {
			t.Fatalf("slot.StatusPoolLocksOnly: %v", err)
		}
		if rep.HeldCount == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool still counts a holder: %+v", rep)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHelperHoldBatchSlotUntilKilled is not a real test; it is spawned as a
// subprocess by TestPrintSlotHistory_AbandonedAfterHolderIsKilled to take the
// slot as the batch gate does and then hold it with no Release in any shutdown
// path. It stubs the container lister itself, since the parent's override is
// process-local.
func TestHelperHoldBatchSlotUntilKilled(t *testing.T) {
	if os.Getenv(slotHistoryHelperEnvVar) != "1" {
		t.Skip("not invoked as batch-slot holder helper")
	}
	stubNoContainers(t)
	townRoot := os.Getenv(slotHistoryTownRootEnvVar)
	h, err := acquireBatchGateSlot(townRoot, "gastown", true)
	if err != nil {
		t.Fatalf("helper acquireBatchGateSlot: %v", err)
	}
	if h == nil {
		t.Fatalf("helper acquireBatchGateSlot returned a nil handle with a gate configured")
	}
	time.Sleep(30 * time.Second) // outlived by the parent's SIGKILL
}

// TestPrintSlotHistory_Empty renders nothing for a town with no history, rather
// than a header with no entries under it.
func TestPrintSlotHistory_Empty(t *testing.T) {
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printSlotHistory(cmd, slot.Report{}, nil)
	if buf.Len() != 0 {
		t.Errorf("printSlotHistory(nil) wrote %q, want nothing", buf.String())
	}
}
