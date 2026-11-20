package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
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

// A finished seat whose agent bead never recorded a cleanup_status (it was
// hand-slung and never ran gt done) is not blocked by that gap alone: its hook
// bead is closed, so the hook is stale, its work is terminal, and the live git
// probe is clean, so the nuke gate reads it as clear (gt-pojsio).
func TestNukeSafetyClearsAFinishedSeatWithAMissingCleanupStatus(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	r := &rig.Rig{Name: "gastown", Path: filepath.Join(town, "gastown")}
	fake := mrlessBeads{beadsfake.New()}
	agentID := polecatBeadIDForRig(r, "gastown", "ember")
	if _, err := fake.Create(beads.CreateOptions{
		ID: agentID, Title: agentID, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(agentID, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: string(beads.AgentStateSpawning),
			Agent: sandboxSeatAgent, Branch: "polecat/ember/gt-a+x", LastSourceIssue: "gt-work", HookBead: "gt-work",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.Create(beads.CreateOptions{ID: "gt-work", Title: "work", Priority: 2}); err != nil {
		t.Fatal(err)
	}
	closed := "closed"
	if err := fake.Update("gt-work", beads.UpdateOptions{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	env := sandboxSeatEnv{
		probe: func(string, string) (polecat.LiveGitState, bool) {
			return polecat.SandboxSeatGitState("", false, nil, "gt-ember"), true
		},
		hostGitState: gatewayOnlySeat,
		remoteBranch: func(string, string) (bool, error) { return false, nil },
	}
	get := func(string) (*polecat.Polecat, error) {
		return &polecat.Polecat{Name: "ember", Branch: "polecat/ember/gt-a+x", ClonePath: filepath.Join(town, "gastown", "polecats", "ember", "gastown")}, nil
	}

	result := checkPolecatSafetyWith("gastown", "ember", r, get, fake, env)

	if result.Blocked || len(result.Reasons) != 0 {
		t.Fatalf("nuke refused a finished seat with a missing cleanup_status: blocked=%v reasons=%q", result.Blocked, strings.Join(result.Reasons, " | "))
	}
}
