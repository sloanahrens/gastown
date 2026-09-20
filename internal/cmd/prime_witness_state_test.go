package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/witness"
)

// TestPrimeResetsWitnessPatrolState pins the gate on the reset: it fires only
// for the SessionStart hook of a fresh witness session. Compaction and resume
// continue the same session, and a bare `gt prime` is a context read, so none
// of those may reset a counter the running session is still using (gt-oabl).
func TestPrimeResetsWitnessPatrolState(t *testing.T) {
	tests := []struct {
		name     string
		role     Role
		hookMode bool
		source   string
		want     bool
	}{
		{"fresh witness session", RoleWitness, true, "startup", true},
		{"witness after clear", RoleWitness, true, "clear", true},
		{"witness after compaction", RoleWitness, true, "compact", false},
		{"witness resumed", RoleWitness, true, "resume", false},
		{"witness with unknown source", RoleWitness, true, "", false},
		{"bare gt prime for witness", RoleWitness, false, "startup", false},
		{"fresh session of another role", RolePolecat, true, "startup", false},
		{"refinery session", RoleRefinery, true, "startup", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := primeResetsWitnessPatrolState(tc.role, tc.hookMode, tc.source); got != tc.want {
				t.Errorf("primeResetsWitnessPatrolState(%q, %v, %q) = %v, want %v",
					tc.role, tc.hookMode, tc.source, got, tc.want)
			}
		})
	}
}

// staleWitnessTown builds a town with a rig whose witness is carrying the
// counter that stalled the om witness, and returns the rig name.
func staleWitnessTown(t *testing.T, state string) (townRoot, rig string) {
	t.Helper()
	townRoot = t.TempDir()
	rig = "om"
	dir := filepath.Join(townRoot, rig, "witness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(witness.PatrolStatePath(dir), []byte(state), 0o644); err != nil {
		t.Fatalf("write witness state: %v", err)
	}
	return townRoot, rig
}

// TestResetWitnessPatrolState_ReportsInheritedCounter drives the branch this
// fix exists for: a fresh witness session must not inherit 602 patrols.
func TestResetWitnessPatrolState_ReportsInheritedCounter(t *testing.T) {
	townRoot, rig := staleWitnessTown(t, `{
  "patrol_count": 602,
  "extraordinary_action": true,
  "session_note": "keep me"
}`)

	prevMode, prevSource := primeHookMode, primeHookSource
	primeHookMode, primeHookSource = true, "startup"
	t.Cleanup(func() { primeHookMode, primeHookSource = prevMode, prevSource })

	msg := resetWitnessPatrolState(RoleContext{Role: RoleWitness, Rig: rig, TownRoot: townRoot})
	if !strings.Contains(msg, "patrol_count") {
		t.Errorf("status line %q does not name what was reset", msg)
	}

	state := readWitnessState(t, townRoot, rig)
	if got := state["patrol_count"]; string(got) != "0" {
		t.Errorf("patrol_count = %s, want 0", got)
	}
	if got := state["extraordinary_action"]; string(got) != "false" {
		t.Errorf("extraordinary_action = %s, want false", got)
	}
	if got := state["session_note"]; string(got) != `"keep me"` {
		t.Errorf("session_note = %s, want the successor's continuity preserved", got)
	}
}

func TestResetWitnessPatrolState_SkipsContinuingSessions(t *testing.T) {
	townRoot, rig := staleWitnessTown(t, `{"patrol_count": 602, "extraordinary_action": true}`)

	prevMode, prevSource := primeHookMode, primeHookSource
	t.Cleanup(func() { primeHookMode, primeHookSource = prevMode, prevSource })

	// A compaction re-prime continues the session that owns this counter.
	primeHookMode, primeHookSource = true, "compact"
	if msg := resetWitnessPatrolState(RoleContext{Role: RoleWitness, Rig: rig, TownRoot: townRoot}); msg != "" {
		t.Errorf("compaction re-prime reset the counter: %q", msg)
	}

	state := readWitnessState(t, townRoot, rig)
	if got := state["patrol_count"]; string(got) != "602" {
		t.Errorf("patrol_count = %s, want the running session's 602 untouched", got)
	}
}

// TestResetWitnessPatrolState_WarnsButContinues: a state file we cannot parse
// must not stop a witness from starting.
func TestResetWitnessPatrolState_WarnsButContinues(t *testing.T) {
	corrupt := `{"patrol_count": 602,`
	townRoot, rig := staleWitnessTown(t, corrupt)

	prevMode, prevSource := primeHookMode, primeHookSource
	primeHookMode, primeHookSource = true, "startup"
	t.Cleanup(func() { primeHookMode, primeHookSource = prevMode, prevSource })

	msg := resetWitnessPatrolState(RoleContext{Role: RoleWitness, Rig: rig, TownRoot: townRoot})
	if !strings.Contains(msg, "not reset") {
		t.Errorf("status line %q does not report the failure", msg)
	}

	data, err := os.ReadFile(witness.PatrolStatePath(filepath.Join(townRoot, rig, "witness")))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if string(data) != corrupt {
		t.Errorf("unparseable state was modified: %s", data)
	}
}

// TestResetWitnessPatrolState_DryRunWritesNothing: --dry-run is introspection,
// and introspection must not mutate a live session's state.
func TestResetWitnessPatrolState_DryRunWritesNothing(t *testing.T) {
	townRoot, rig := staleWitnessTown(t, `{"patrol_count": 602, "extraordinary_action": true}`)

	prevMode, prevSource, prevDry := primeHookMode, primeHookSource, primeDryRun
	primeHookMode, primeHookSource, primeDryRun = true, "startup", true
	t.Cleanup(func() {
		primeHookMode, primeHookSource, primeDryRun = prevMode, prevSource, prevDry
	})

	if msg := resetWitnessPatrolState(RoleContext{Role: RoleWitness, Rig: rig, TownRoot: townRoot}); msg != "" {
		t.Errorf("dry run produced a status line: %q", msg)
	}
	state := readWitnessState(t, townRoot, rig)
	if got := state["patrol_count"]; string(got) != "602" {
		t.Errorf("patrol_count = %s, want 602 — dry run wrote to the state file", got)
	}
}

func TestResetWitnessPatrolState_SilentWhenNothingToDo(t *testing.T) {
	townRoot := t.TempDir()
	rig := "om"
	if err := os.MkdirAll(filepath.Join(townRoot, rig, "witness"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	prevMode, prevSource := primeHookMode, primeHookSource
	primeHookMode, primeHookSource = true, "startup"
	t.Cleanup(func() { primeHookMode, primeHookSource = prevMode, prevSource })

	if msg := resetWitnessPatrolState(RoleContext{Role: RoleWitness, Rig: rig, TownRoot: townRoot}); msg != "" {
		t.Errorf("no state file produced a status line: %q", msg)
	}
}

func readWitnessState(t *testing.T, townRoot, rig string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(witness.PatrolStatePath(filepath.Join(townRoot, rig, "witness")))
	if err != nil {
		t.Fatalf("read witness state: %v", err)
	}
	state := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse witness state: %v", err)
	}
	return state
}
