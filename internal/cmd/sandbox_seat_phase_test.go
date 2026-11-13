package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// checkRecoveryOfListedSandboxSeat runs check-recovery for the sandbox seat
// ember, whose gateway still lists its sandbox (the VM restart left its record
// and its sandbox behind), with the gateway's phase answered by phase.
func checkRecoveryOfListedSandboxSeat(t *testing.T, phase sandboxPhaseFunc) RecoveryStatus {
	t.Helper()
	town := sandboxSeatTown(t)
	r := rigWithBareRepo(t, town)
	db := mrlessBeads{beadsfake.New()}
	agentID := polecatBeadIDForRig(r, "gastown", "ember")
	if _, err := db.Create(beads.CreateOptions{
		ID: agentID, Title: agentID, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(agentID, &beads.AgentFields{
			RoleType: "polecat", Rig: "gastown", AgentState: string(beads.AgentStateDone),
			CleanupStatus: string(polecat.CleanupClean), Agent: sandboxSeatAgent, Branch: "polecat/ember/gt-a+x",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	p := &polecat.Polecat{
		Name: "ember", State: polecat.StateDone, Branch: "polecat/ember/gt-a+x",
		ClonePath: filepath.Join(town, "gastown", "polecats", "ember", "gastown"),
	}
	listed := func(string, string) (bool, error) { return true, nil }
	probe := sandboxSeatProbeWithPhase(town, listed, phase)
	return checkRecoveryForPolecatWith(sandboxSeatEnv{probe: probe, hostGitState: gatewayOnlySeat}, db, nil, r, "gastown", "ember", p, false)
}

// A sandbox the gateway lists is not necessarily running: after a VM restart it
// can read Stopped, and check-recovery must say so rather than name a running
// sandbox (gt-dtel4n). Only a Running or Ready phase keeps the running text; a
// phase the gateway cannot give never reads as running. Whatever the phase, the
// unknown git state keeps the seat out of reuse and nuke.
func TestCheckRecoveryNamesAStoppedSandbox(t *testing.T) {
	t.Parallel()
	const runningText = "git state lives in the running sandbox gt-ember"
	const stoppedText = "git state lives in the stopped sandbox gt-ember; it cannot be read until the sandbox starts, and the patrol deletes it once the seat's work has landed"
	tests := []struct {
		name        string
		phase       string
		phaseErr    error
		wantBlocker string // the blocker check-recovery must report
		wantPhase   string // the sandbox_phase JSON field; "" means absent
	}{
		{"stopped_names_the_stop", "Stopped", nil, stoppedText, "Stopped"},
		{"running_keeps_the_old_text", "Running", nil, runningText, "Running"},
		{"ready_keeps_the_old_text", "Ready", nil, runningText, "Ready"},
		{"errored_names_its_phase", "Error", nil,
			"git state of sandbox gt-ember is unknown: the gateway reports it in phase Error", "Error"},
		{"silent_gateway_names_the_failure", "", errors.New("connection refused"),
			"cannot ask the gateway for the sandbox's phase: connection refused", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			phase := func(gateway, name string) (sandbox.Status, error) {
				asked = append(asked, gateway+"|"+name)
				return sandbox.Status{Name: name, Phase: tt.phase}, tt.phaseErr
			}

			status := checkRecoveryOfListedSandboxSeat(t, phase)

			if len(asked) != 1 || asked[0] != "gw1|gt-ember" {
				t.Errorf("gateway asked for the phase %v, want one question about gt-ember on gw1", asked)
			}
			if status.GitStateSource != polecat.GitStateSourceUnknown {
				t.Errorf("git state source = %q, want unknown: a listed sandbox is not measured", status.GitStateSource)
			}
			if status.Reusable || status.SafeToNuke {
				t.Errorf("a seat with unknown git state is reusable=%v safe_to_nuke=%v", status.Reusable, status.SafeToNuke)
			}
			blockers := strings.Join(status.Blockers, "\n")
			if !strings.Contains(blockers, tt.wantBlocker) {
				t.Errorf("blockers = %q, want %q", blockers, tt.wantBlocker)
			}
			if tt.phase != "Running" && tt.phase != "Ready" && strings.Contains(blockers, "running sandbox") {
				t.Errorf("blockers = %q say running for phase %q", blockers, tt.phase)
			}

			var out bytes.Buffer
			renderCheckRecoveryText(&out, status)
			if !strings.Contains(out.String(), tt.wantBlocker) {
				t.Errorf("text output does not print the blocker %q:\n%s", tt.wantBlocker, out.String())
			}

			raw, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			got, present := fields["sandbox_phase"]
			switch {
			case tt.wantPhase == "" && present:
				t.Errorf("sandbox_phase = %v, want absent when the gateway did not say", got)
			case tt.wantPhase != "" && got != tt.wantPhase:
				t.Errorf("sandbox_phase = %v, want %q", got, tt.wantPhase)
			}
		})
	}
}
