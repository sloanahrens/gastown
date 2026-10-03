package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// A refusal is not a failure: the nuke did not happen, so the gate must return
// an error the exit-code mapper turns into NukeRefusedExitCode (3), and the
// human refusal text must be exactly what displaySafetyCheckBlockedTo prints —
// the same bytes this path printed before it was extracted (gt-vuiii).
func TestNukeSafetyRefusalExitsThree(t *testing.T) {
	t.Parallel()
	targets := []polecatTarget{{rigName: "gastown", polecatName: "basalt"}}
	blocked := []*SafetyCheckResult{{
		Polecat: "gastown/basalt",
		Blocked: true,
		Reasons: []string{"has 2 unpushed commit(s)", "has work on hook (gt-abc)"},
	}}
	check := func(polecatTarget) *SafetyCheckResult { return blocked[0] }

	var out bytes.Buffer
	err := nukeSafetyRefusal(&out, targets, false, false, check)
	if err == nil {
		t.Fatal("a blocked nuke returned nil, want a refusal error")
	}
	if got := exitCodeForError(err); got != NukeRefusedExitCode {
		t.Errorf("exit code = %d, want %d (NukeRefusedExitCode)", got, NukeRefusedExitCode)
	}
	if NukeRefusedExitCode == 1 {
		t.Errorf("NukeRefusedExitCode = 1, which is indistinguishable from an ordinary failure")
	}

	// The blocked-list output must be byte-identical to the canonical renderer.
	var want bytes.Buffer
	displaySafetyCheckBlockedTo(&want, blocked)
	if out.String() != want.String() {
		t.Errorf("refusal output drifted from displaySafetyCheckBlockedTo:\ngot  %q\nwant %q", out.String(), want.String())
	}
	wantErr := fmt.Sprintf("blocked: 1 polecat(s) failed nuke safety checks: %s", formatSafetyCheckBlockers(blocked))
	if err.Error() != wantErr {
		t.Errorf("refusal message = %q, want %q", err.Error(), wantErr)
	}
}

// --force and --dry-run bypass the gate: neither may consult the safety check
// or return a coding error, so both still exit 0 (gt-vuiii).
func TestNukeSafetyGateBypassedByForceAndDryRun(t *testing.T) {
	t.Parallel()
	targets := []polecatTarget{{rigName: "gastown", polecatName: "basalt"}}
	for _, tc := range []struct {
		name          string
		force, dryRun bool
	}{
		{name: "--force bypasses the gate", force: true},
		{name: "--dry-run bypasses the gate", dryRun: true},
		{name: "--force --dry-run bypasses the gate", force: true, dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			asked := 0
			check := func(polecatTarget) *SafetyCheckResult { asked++; return &SafetyCheckResult{Blocked: true} }
			var out bytes.Buffer
			if err := nukeSafetyRefusal(&out, targets, tc.force, tc.dryRun, check); err != nil {
				t.Fatalf("gate refused under %s: %v", tc.name, err)
			}
			if asked != 0 {
				t.Errorf("safety check consulted %d time(s), want 0", asked)
			}
			if out.Len() != 0 {
				t.Errorf("gate wrote %q, want nothing", out.String())
			}
		})
	}
}

// A clean pass returns nil and prints nothing, so a nuke with nothing blocked
// proceeds exactly as before.
func TestNukeSafetyGatePassesWhenNothingIsBlocked(t *testing.T) {
	t.Parallel()
	targets := []polecatTarget{{rigName: "gastown", polecatName: "basalt"}, {rigName: "gastown", polecatName: "granite"}}
	asked := 0
	check := func(polecatTarget) *SafetyCheckResult { asked++; return &SafetyCheckResult{} }
	var out bytes.Buffer
	if err := nukeSafetyRefusal(&out, targets, false, false, check); err != nil {
		t.Fatalf("gate refused a clean pass: %v", err)
	}
	if asked != len(targets) {
		t.Errorf("safety check consulted %d time(s), want %d", asked, len(targets))
	}
	if out.Len() != 0 {
		t.Errorf("gate wrote %q, want nothing", out.String())
	}
}

// Every non-refusal failure of the nuke still exits 1, so only the gate's coded
// error is read as a refusal. This is the contract the daemon relies on to map
// exit status to its verdict.
func TestNukeNonRefusalFailureExitsOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{name: "unknown polecat", err: errors.New("rig not found: nosuchrig")},
		{name: "nuke failed", err: fmt.Errorf("%d nuke(s) failed", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeForError(tc.err); got != 1 {
				t.Errorf("exit code for %v = %d, want 1", tc.err, got)
			}
		})
	}
}
