package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
)

func parkTestPolecat(t *testing.T, townRoot, rigName, name string) {
	t.Helper()
	if err := agentpause.Pause(townRoot, rigName, constants.RolePolecat, name,
		"operator parked", "human", "idle"); err != nil {
		t.Fatalf("Pause(%s): %v", name, err)
	}
}

func cleanIdleAgentFields() *beads.AgentFields {
	return &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: "clean"}
}

// TestCapacitySnapshotCountsParkedSlotAsParkedNotReusable guards gt-q6nrm: the
// reuse path skips a parked polecat (gt-0r29l), but capacity still counted the
// same clean idle slot as reusable_idle, so an operator read a slot that would
// be refused as available.
func TestCapacitySnapshotCountsParkedSlotAsParkedNotReusable(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	parkTestPolecat(t, townRoot, "gastown", "synth")

	parked := polecatCapacitySnapshot{}
	applyAgentFieldsToCapacitySnapshot(&parked, townRoot, "gastown", "synth", cleanIdleAgentFields(), nil, nil)
	if parked.Parked != 1 || parked.ReusableIdle != 0 || parked.RecoveryBlocked != 0 || parked.capacityUsed != 0 {
		t.Fatalf("parked snapshot = %+v, want Parked=1 and nothing else counted", parked)
	}

	// The marker is per polecat: a sibling with the same clean facts is still free.
	sibling := polecatCapacitySnapshot{}
	applyAgentFieldsToCapacitySnapshot(&sibling, townRoot, "gastown", "other", cleanIdleAgentFields(), nil, nil)
	if sibling.ReusableIdle != 1 || sibling.Parked != 0 {
		t.Fatalf("unparked sibling snapshot = %+v, want ReusableIdle=1", sibling)
	}
}

// TestCapacitySnapshotParkDoesNotHideRecovery: a parked slot that also holds
// unrecovered work is still a recovery problem; the park must not relabel it.
func TestCapacitySnapshotParkDoesNotHideRecovery(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	parkTestPolecat(t, townRoot, "gastown", "synth")

	snapshot := polecatCapacitySnapshot{}
	fields := &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: "has_uncommitted"}
	applyAgentFieldsToCapacitySnapshot(&snapshot, townRoot, "gastown", "synth", fields, nil, nil)
	if snapshot.RecoveryBlocked != 1 || snapshot.Parked != 0 {
		t.Fatalf("snapshot = %+v, want RecoveryBlocked=1 Parked=0", snapshot)
	}
}

// TestPolecatListInventoryEnvReadsParkMarker: the list env resolves the town
// root from the rig path and reads the marker, so `gt polecat list` shows the
// same parked verdict capacity does.
func TestPolecatListInventoryEnvReadsParkMarker(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	parkTestPolecat(t, townRoot, "gastown", "synth")

	env := polecatListInventoryEnv(rigPath, "gastown", "synth", polecatMRIndex{}, nil, polecatSpawnFacts{})
	if !strings.HasPrefix(env.Parked, "parked (") || !strings.Contains(env.Parked, "operator parked") {
		t.Fatalf("env.Parked = %q, want the park reason", env.Parked)
	}
	if other := polecatListInventoryEnv(rigPath, "gastown", "other", polecatMRIndex{}, nil, polecatSpawnFacts{}); other.Parked != "" {
		t.Fatalf("unparked polecat env.Parked = %q, want empty", other.Parked)
	}
}

// TestPolecatListRowShowsParkedAsNotReusable is the list half of gt-q6nrm.
func TestPolecatListRowShowsParkedAsNotReusable(t *testing.T) {
	t.Parallel()
	fields := cleanIdleAgentFields()

	free := buildPolecatSeatItem("gastown", "synth", fields, nil, nil, polecatSessionSet{}, polecatInventoryEnv{})
	if !free.Reusable {
		t.Fatalf("control row = %+v, want reusable", free)
	}

	parked := buildPolecatSeatItem("gastown", "synth", fields, nil, nil, polecatSessionSet{},
		polecatInventoryEnv{Parked: "parked (operator parked)"})
	if parked.Reusable {
		t.Fatalf("parked row is reusable: %+v", parked)
	}
	if parked.ReuseStatus != polecat.WorkstateReuseStatusParked || parked.Reason != polecat.WorkstateReasonParked {
		t.Fatalf("parked row reuse_status=%q reason=%q, want %q/%q",
			parked.ReuseStatus, parked.Reason, polecat.WorkstateReuseStatusParked, polecat.WorkstateReasonParked)
	}
	if len(parked.Blockers) != 1 || parked.Blockers[0] != "parked (operator parked)" {
		t.Fatalf("parked row blockers = %v, want the park reason", parked.Blockers)
	}
	if parked.NeedsRecovery || !parked.SafeToNuke {
		t.Fatalf("parked row needs_recovery=%v safe_to_nuke=%v; a park is a reuse gate, not a recovery state",
			parked.NeedsRecovery, parked.SafeToNuke)
	}
	if line := polecatReuseDetailLine(parked); !strings.Contains(line, "reuse: idle-parked") {
		t.Fatalf("reuse detail line = %q, want it to say idle-parked", line)
	}
}
