package keepalive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testEpoch is the fixed instant the tests date their keepalives from.
var testEpoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func TestTouchInWorkspace(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	touchAt(tmpDir, "gt status", testEpoch.In(time.FixedZone("CDT", -5*3600)))

	state := Read(tmpDir)
	if state == nil {
		t.Fatal("expected state to be non-nil")
	}
	if state.LastCommand != "gt status" {
		t.Errorf("expected last_command 'gt status', got %q", state.LastCommand)
	}
	if !state.Timestamp.Equal(testEpoch) || state.Timestamp.Location() != time.UTC {
		t.Errorf("timestamp = %v, want %v in UTC", state.Timestamp, testEpoch)
	}
}

// TestTouchInWorkspaceStampsWallClock checks the exported wrapper writes a
// non-zero UTC timestamp.
func TestTouchInWorkspaceStampsWallClock(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	TouchInWorkspace(tmpDir, "gt status")
	state := Read(tmpDir)
	if state == nil || state.Timestamp.IsZero() || state.Timestamp.Location() != time.UTC {
		t.Fatalf("Read() = %+v, want a non-zero UTC timestamp", state)
	}
}

func TestReadNonExistent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	state := Read(tmpDir)
	if state != nil {
		t.Error("expected nil state for non-existent file")
	}
}

func TestReadCorrupt(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".runtime", "keepalive.json"), []byte("{"), 0644); err != nil {
		t.Fatal(err)
	}
	if state := Read(tmpDir); state != nil {
		t.Errorf("Read() of corrupt file = %+v, want nil", state)
	}
}

func TestStateAge(t *testing.T) {
	t.Parallel()
	// Test nil state returns the sentinel age
	var nilState *State
	if got := nilState.ageAt(testEpoch); got != 365*24*time.Hour {
		t.Errorf("nil state age = %v, want 365 days", got)
	}
	if nilState.Age() != 365*24*time.Hour {
		t.Error("nil state Age() should be the 365-day sentinel")
	}

	freshState := &State{Timestamp: testEpoch.Add(-30 * time.Second)}
	if age := freshState.ageAt(testEpoch); age != 30*time.Second {
		t.Errorf("expected 30s age, got %v", age)
	}

	olderState := &State{Timestamp: testEpoch.Add(-5 * time.Minute)}
	if age := olderState.ageAt(testEpoch); age != 5*time.Minute {
		t.Errorf("expected 5m age, got %v", age)
	}

	// Age measures from the wall clock: a stamp in the past has positive age.
	if age := freshState.Age(); age <= 0 {
		t.Errorf("Age() of a past stamp = %v, want > 0", age)
	}

	// NOTE: IsFresh(), IsStale(), IsVeryStale() were removed as part of ZFC cleanup.
	// Staleness classification belongs in Deacon molecule, not Go code.
}

func TestDirectoryCreation(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "some", "nested", "workspace")

	// Touch should create .runtime directory
	touchAt(workDir, "gt test", testEpoch)

	// Verify directory was created
	runtimeDir := filepath.Join(workDir, ".runtime")
	if _, err := os.Stat(runtimeDir); os.IsNotExist(err) {
		t.Error("expected .runtime directory to be created")
	}
}

// Example functions demonstrate keepalive usage patterns.

func ExampleTouchInWorkspace() {
	// TouchInWorkspace signals agent activity in a specific workspace.
	// This is the core function - use it when you know the workspace root.

	workspaceRoot := "/path/to/workspace"

	// Signal that "gt status" was run
	TouchInWorkspace(workspaceRoot, "gt status")

	// Signal a command with arguments
	TouchInWorkspace(workspaceRoot, "gt sling bd-abc123 ai-platform")

	// All errors are silently ignored (best-effort design).
	// This is intentional - keepalive failures should never break commands.
}

func ExampleRead() {
	// Read retrieves the current keepalive state for a workspace.
	// Returns nil if no keepalive file exists or it can't be read.

	workspaceRoot := "/path/to/workspace"
	state := Read(workspaceRoot)

	if state == nil {
		// No keepalive found - agent may not have run any commands yet
		return
	}

	// Access the last command that was run
	_ = state.LastCommand // e.g., "gt status"

	// Access when the command was run
	_ = state.Timestamp // time.Time in UTC
}

func ExampleState_Age() {
	// Age() returns how long ago the keepalive was updated.
	// This is useful for detecting idle or stuck agents.

	workspaceRoot := "/path/to/workspace"
	state := Read(workspaceRoot)

	// Age() is nil-safe - returns ~1 year for nil state
	age := state.Age()

	// Check if agent was active recently (within 5 minutes)
	if age < 5*time.Minute {
		// Agent is active
		_ = "active"
	}

	// Check if agent might be stuck (no activity for 30+ minutes)
	if age > 30*time.Minute {
		// Agent may need attention
		_ = "possibly stuck"
	}
}
