package polecat

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestRecordBeadRespawn_Increments(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	// Create the witness subdirectory so the state file path is valid.
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}

	count := mustRecord(t, tmpDir, "bead-1")
	if count != 1 {
		t.Errorf("first RecordBeadRespawn = %d, want 1", count)
	}

	count = mustRecord(t, tmpDir, "bead-1")
	if count != 2 {
		t.Errorf("second RecordBeadRespawn = %d, want 2", count)
	}
}

// mustRecord is RecordBeadRespawn for the fixtures that expect it to land.
func mustRecord(t *testing.T, townRoot, beadID string) int {
	t.Helper()
	count, err := RecordBeadRespawn(townRoot, beadID)
	if err != nil {
		t.Fatalf("RecordBeadRespawn(%s): %v", beadID, err)
	}
	return count
}

func TestShouldBlockRespawn_Threshold(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}

	// Below threshold.
	for i := 0; i < config.DefaultRecoveryMaxBeadRespawns-1; i++ {
		mustRecord(t, tmpDir, "bead-2")
	}
	if ShouldBlockRespawn(tmpDir, "bead-2") {
		t.Error("ShouldBlockRespawn = true before reaching threshold")
	}

	// At threshold.
	mustRecord(t, tmpDir, "bead-2")
	if !ShouldBlockRespawn(tmpDir, "bead-2") {
		t.Error("ShouldBlockRespawn = false at threshold")
	}
}

func TestResetBeadRespawnCount(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}

	mustRecord(t, tmpDir, "bead-3")
	mustRecord(t, tmpDir, "bead-3")

	if err := ResetBeadRespawnCount(tmpDir, "bead-3"); err != nil {
		t.Fatalf("ResetBeadRespawnCount error: %v", err)
	}

	if ShouldBlockRespawn(tmpDir, "bead-3") {
		t.Error("ShouldBlockRespawn = true after reset")
	}

	// Re-increment should start from 1.
	count := mustRecord(t, tmpDir, "bead-3")
	if count != 1 {
		t.Errorf("RecordBeadRespawn after reset = %d, want 1", count)
	}
}

func TestRecordBeadRespawn_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := RecordBeadRespawn(tmpDir, "bead-race"); err != nil {
				t.Errorf("RecordBeadRespawn: %v", err)
			}
		}()
	}
	wg.Wait()

	// After all goroutines, the count must equal the number of increments.
	state, err := loadBeadRespawnState(tmpDir)
	if err != nil {
		t.Fatalf("loadBeadRespawnState: %v", err)
	}
	rec, ok := state.Beads["bead-race"]
	if !ok {
		t.Fatal("bead-race record not found")
	}
	if rec.Count != goroutines {
		t.Errorf("concurrent count = %d, want %d", rec.Count, goroutines)
	}
}

func TestShouldBlockRespawn_UnknownBead(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}

	if ShouldBlockRespawn(tmpDir, "nonexistent") {
		t.Error("ShouldBlockRespawn = true for unknown bead")
	}
}

// TestRespawnStateTreatsATruncatedFileAsCorruption: a state file that does not
// parse is reported, never read as an empty state. Reading it as empty re-armed
// the breaker for every bead at once — one truncated write and a town's whole
// respawn budget was gone (gt-u3hc1).
func TestRespawnStateTreatsATruncatedFileAsCorruption(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
		mustRecord(t, tmpDir, "bead-corrupt")
	}
	if !ShouldBlockRespawn(tmpDir, "bead-corrupt") {
		t.Fatal("breaker not armed by the fixture")
	}

	// The half-written file a crash between truncate and write leaves behind.
	stateFile := filepath.Join(tmpDir, "witness", "bead-respawn-counts.json")
	if err := os.WriteFile(stateFile, []byte(`{"beads":{"bead-corrupt":{"count":3`), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadBeadRespawnState(tmpDir); err == nil {
		t.Error("loadBeadRespawnState on a truncated file = nil error, want a parse error")
	}
	if _, err := RecordBeadRespawn(tmpDir, "bead-corrupt"); err == nil {
		t.Error("RecordBeadRespawn on a truncated file = nil error, want a refusal")
	}
	if !ShouldBlockRespawn(tmpDir, "bead-corrupt") {
		t.Error("ShouldBlockRespawn on a truncated file = false: corruption re-armed the breaker")
	}
}

// TestRespawnStateFailsClosedWhenTheLockCannotBeTaken: a lock that cannot be
// taken stops the read-modify-write instead of running without it. The old code
// guarded on `flockErr == nil` alone and so took the unlocked path on every
// failure, which is how concurrent patrols lost each other's counts (gt-u3hc1).
func TestRespawnStateFailsClosedWhenTheLockCannotBeTaken(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := saveBeadRespawnState(tmpDir, emptyBeadRespawnState()); err != nil {
		t.Fatal(err)
	}
	// A directory where the lock file belongs: flockAcquire opens it read-write
	// and gets EISDIR. Unlike a mode, that fails for every uid.
	if err := os.MkdirAll(beadRespawnStateFile(tmpDir)+".flock", 0755); err != nil {
		t.Fatal(err)
	}

	if count, err := RecordBeadRespawn(tmpDir, "bead-1"); err == nil {
		t.Errorf("RecordBeadRespawn with an untakeable lock = %d, nil; want a refusal", count)
	}
	if err := ResetBeadRespawnCount(tmpDir, "bead-1"); err == nil {
		t.Error("ResetBeadRespawnCount with an untakeable lock = nil error, want a refusal")
	}
	if !ShouldBlockRespawn(tmpDir, "bead-1") {
		t.Error("ShouldBlockRespawn with an untakeable lock = false, want the fail-closed true")
	}
}

// TestSaveBeadRespawnStateReplacesInPlaceOfTruncating: the state is written
// beside its destination and renamed over it. os.WriteFile truncates in place,
// so a crash mid-write leaves the unparseable file that reads as empty — and a
// rename also replaces a destination no one can open for writing (gt-u3hc1).
func TestSaveBeadRespawnStateReplacesInPlaceOfTruncating(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "witness"), 0755); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(tmpDir, "witness", "bead-respawn-counts.json")
	if err := os.WriteFile(stateFile, []byte("{}"), 0400); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(stateFile)
	if err != nil {
		t.Fatal(err)
	}

	state := emptyBeadRespawnState()
	state.Beads["bead-1"] = &beadRespawnRecord{BeadID: "bead-1", Count: 2}
	if err := saveBeadRespawnState(tmpDir, state); err != nil {
		t.Fatalf("saveBeadRespawnState over a read-only state file: %v", err)
	}

	after, err := os.Stat(stateFile)
	if err != nil {
		t.Fatalf("stat after save: %v", err)
	}
	if os.SameFile(before, after) {
		t.Error("the state file was truncated and rewritten in place, not replaced by a rename")
	}
	reloaded, err := loadBeadRespawnState(tmpDir)
	if err != nil {
		t.Fatalf("loadBeadRespawnState after save: %v", err)
	}
	if rec := reloaded.Beads["bead-1"]; rec == nil || rec.Count != 2 {
		t.Errorf("reloaded bead-1 = %+v, want count 2", rec)
	}
	leftovers, err := filepath.Glob(filepath.Join(tmpDir, "witness", ".bead-respawn-counts-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// TestRespawnStateCreatesTheWitnessDir: the flock file's open creates no parent
// directories, so a first run in a fresh town failed to take the lock at all.
// The lock file standing beside the state file is what shows the lock was
// taken rather than skipped (gt-u3hc1).
func TestRespawnStateCreatesTheWitnessDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir() // no witness/ subdirectory

	mustRecord(t, tmpDir, "bead-fresh")

	flock := filepath.Join(tmpDir, "witness", "bead-respawn-counts.json.flock")
	if _, err := os.Stat(flock); err != nil {
		t.Errorf("no lock file after the first record, so the lock was never taken: %v", err)
	}
	if ShouldBlockRespawn(tmpDir, "bead-fresh") {
		t.Error("ShouldBlockRespawn = true after a single respawn")
	}
}
