package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deacon"
)

// deaconRedispatch is what `gt deacon redispatch <bead-id>` runs, and it is
// the deacon's RECOVERED_BEAD handling: the patrol formula's inbox-check step
// runs that command once per RECOVERED_BEAD message. These tests cover the
// wiring between the command and deacon.RedispatchRecoveredBead — the reads it
// assembles into a RecoveredBeadRecord — rather than the decisions themselves,
// which the deacon package tests.

// stubRedispatchTools puts a `bd` and a `gt` on PATH that log every
// invocation, so a test can see whether the bead was slung, escalated about,
// or left alone. `bd show` answers with a scriptable JSON payload.
func stubRedispatchTools(t *testing.T, bdShowPayload string) func() []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "calls.log")

	script := `#!/bin/sh
TOOL="$(basename "$0")"
printf '%s\t' "$TOOL" >> "` + logPath + `"
printf '%s ' "$@" | tr '\n' ' ' >> "` + logPath + `"
printf '\n' >> "` + logPath + `"
if [ "$TOOL" = "bd" ] && [ "$1" = "show" ]; then
  printf '%s\n' '` + bdShowPayload + `'
fi
exit 0
`
	for _, tool := range []string{"bd", "gt"} {
		if err := os.WriteFile(filepath.Join(binDir, tool), []byte(script), 0755); err != nil {
			t.Fatalf("write %s stub: %v", tool, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("read call log: %v", err)
		}
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
}

// stubHold makes the dispatch-hold lookup answer with a fixed reason without a
// beads store, which a hermetic test has none of. It restores the production
// lookup when the test ends.
func stubHold(t *testing.T, reason string) {
	t.Helper()
	previous := redispatchHoldLookupFn
	redispatchHoldLookupFn = func(string) (func(beadID string) string, func()) {
		return func(string) string { return reason }, func() {}
	}
	t.Cleanup(func() { redispatchHoldLookupFn = previous })
}

// editorialNotesJSON wraps notes carrying an om review receipt in the shape
// `bd show --json` returns them, so the command recovers the receipt the way
// it does in production (refinery.formatMergeRejectionNote, om-gate T10).
func editorialNotesJSON(notes string) string {
	return `[{"id":"gt-wiring","status":"open","notes":` + jsonString(notes) + `}]`
}

// TestDeaconRedispatch_HeldBeadIsNotSlung is the wiring half of gt-wuqn's
// second acceptance criterion: a bead its own record holds is skipped at the
// command, from the hold lookup all the way out. It fails if the hold is read
// but dropped before the handler sees it — the wiring would still compile.
func TestDeaconRedispatch_HeldBeadIsNotSlung(t *testing.T) {
	const hold = "label needs-mayor-review"
	// Notes that would route editorially and escalate: only the hold stops
	// this bead reaching the mayor.
	notes := "MERGE REJECTION (attempt 2): editorial - review found 1 major\nScore: 0.6000\nUnresolved: abc123def456"
	calls := stubRedispatchTools(t, editorialNotesJSON(notes))
	stubHold(t, hold)
	townRoot := t.TempDir()

	result := deaconRedispatch(townRoot, "gt-wiring")

	if result.Action != "skipped" {
		t.Fatalf("Action = %q, want %q (message: %s)", result.Action, "skipped", result.Message)
	}
	if !strings.Contains(result.Message, hold) {
		t.Errorf("Message = %q, want it to name the hold %q", result.Message, hold)
	}
	if logged := calls(); len(logged) != 0 {
		t.Errorf("a held bead was acted on: %v", logged)
	}
}

// TestDeaconRedispatch_EditorialRejectionRoutesThroughConvergence is gt-wuqn's
// first acceptance criterion at the command: a RECOVERED_BEAD whose notes
// carry an om receipt reaches the editorial convergence gate, which stops the
// resubmit and labels the bead needs_human rather than re-slinging it on
// attempt count alone.
func TestDeaconRedispatch_EditorialRejectionRoutesThroughConvergence(t *testing.T) {
	notes := "MERGE REJECTION (attempt 2): editorial - review found 1 major\nScore: 0.6000\nUnresolved: abc123def456"
	calls := stubRedispatchTools(t, editorialNotesJSON(notes))
	stubHold(t, "")
	townRoot := t.TempDir()

	// One resubmit in, whose prior attempt scored the same 0.6.
	state := &deacon.RedispatchState{Beads: map[string]*deacon.BeadRedispatchState{
		"gt-wiring": {
			BeadID:       "gt-wiring",
			AttemptCount: 1,
			LastReceipt:  &deacon.ReceiptSummary{Score: 0.6},
		},
	}}
	if err := deacon.SaveRedispatchState(townRoot, state); err != nil {
		t.Fatalf("SaveRedispatchState: %v", err)
	}

	result := deaconRedispatch(townRoot, "gt-wiring")

	if result.Action != "escalated" {
		t.Fatalf("Action = %q, want %q (message: %s)", result.Action, "escalated", result.Message)
	}
	if !strings.Contains(result.Message, "not converging: unresolved abc123def456") {
		t.Errorf("Message = %q, want the convergence reason", result.Message)
	}

	logged := calls()
	for _, call := range logged {
		if strings.HasPrefix(call, "gt\tsling ") {
			t.Errorf("a non-converging editorial resubmit was re-slung: %s", call)
		}
	}
	if !hasCallContaining(logged, "mail send mayor/", "needs_human") {
		t.Errorf("no needs_human escalation to the mayor; calls: %v", logged)
	}
	if !hasCallContaining(logged, "update gt-wiring", "--add-label needs_human") {
		t.Errorf("the bead was not labeled needs_human; calls: %v", logged)
	}
}

func hasCallContaining(calls []string, fragments ...string) bool {
	for _, call := range calls {
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(call, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// TestOpenRedispatchHoldLookup_FailsClosedOnUnreadableBead guards the posture
// the lookup needs rather than the rule it applies: a bead whose record this
// process cannot read must resolve to a hold, never to a clear, because an
// unreadable record is exactly the case where a held bead cannot be ruled
// out. It asserts the posture, not the wording — a store that will not open at
// all and a store that answers with no such bead reach the same hold by
// different reasons.
func TestOpenRedispatchHoldLookup_FailsClosedOnUnreadableBead(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	holdFor, release := openRedispatchHoldLookup(townRoot)
	defer release()

	if reason := holdFor("gt-unreadable"); reason == "" {
		t.Error("hold lookup cleared a bead whose record it could not read")
	} else {
		t.Logf("hold reason: %s", reason)
	}
}
