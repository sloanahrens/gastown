package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
)

// mapAgentStates is an in-memory agent_state mirror keyed by bead ID.
type mapAgentStates map[string]string

func (m mapAgentStates) read(beadID string) string { return m[beadID] }

func (m mapAgentStates) write(beadID, state string) error {
	m[beadID] = state
	return nil
}

// TestAgentResumeStaleMirrorCleared verifies the stale-mirror repair: a bead
// that still reads agent_state=paused with no agent-pause marker behind it is
// a stale race artifact, and resume clears it to idle.
func TestAgentResumeStaleMirrorCleared(t *testing.T) {
	t.Parallel()
	target, err := parseAgentAddr(cmdTestRegistry(), "mayor")
	if err != nil {
		t.Fatal(err)
	}
	states := mapAgentStates{target.BeadID: "paused"}
	var out strings.Builder
	if err := resumeAgent(&out, t.TempDir(), target, states); err != nil {
		t.Fatalf("resumeAgent: %v", err)
	}
	if got := states[target.BeadID]; got != "idle" {
		t.Errorf("agent_state = %q, want idle", got)
	}
	if !strings.Contains(out.String(), "cleared to idle") {
		t.Errorf("expected stale-mirror-clear notice, got: %q", out.String())
	}
}

// TestAgentResumeRestoresPriorState pins the paused path: resume removes the
// marker and writes back the agent_state the pause captured.
func TestAgentResumeRestoresPriorState(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	target, err := parseAgentAddr(cmdTestRegistry(), "gastown/flint")
	if err != nil {
		t.Fatal(err)
	}
	role, name := target.roleAndName()
	if err := agentpause.Pause(town, target.Rig, role, name, "hold", "human", "working"); err != nil {
		t.Fatal(err)
	}
	states := mapAgentStates{target.BeadID: "paused"}
	var out strings.Builder
	if err := resumeAgent(&out, town, target, states); err != nil {
		t.Fatalf("resumeAgent: %v", err)
	}
	if got := states[target.BeadID]; got != "working" {
		t.Errorf("agent_state = %q, want the captured prior state working", got)
	}
	if paused, _, _ := agentpause.IsPaused(town, target.Rig, role, name); paused {
		t.Error("pause marker still present after resume")
	}
}

type failingAgentStates struct{ mapAgentStates }

func (failingAgentStates) write(string, string) error { return errors.New("dolt down") }

// TestAgentResumeStaleMirrorWriteFailureIsAnError: the stale-mirror repair
// is the whole job when no marker exists, so a failed write is reported.
func TestAgentResumeStaleMirrorWriteFailureIsAnError(t *testing.T) {
	t.Parallel()
	target, err := parseAgentAddr(cmdTestRegistry(), "mayor")
	if err != nil {
		t.Fatal(err)
	}
	states := failingAgentStates{mapAgentStates{target.BeadID: "paused"}}
	if err := resumeAgent(&strings.Builder{}, t.TempDir(), target, states); err == nil {
		t.Fatal("resumeAgent = nil, want the write failure")
	}
}
