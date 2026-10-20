package polecat

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// respawnStatePath and legacyRespawnStatePath spell the two paths out rather
// than calling the production helpers: a helper that drifts off .runtime/ or
// off the legacy witness/ file has to fail a test, not move it (gt-la690).
func respawnStatePath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "bead-respawn-counts.json")
}

func legacyRespawnStatePath(townRoot string) string {
	return filepath.Join(townRoot, "witness", "bead-respawn-counts.json")
}

// writeRespawnStateFile plants a state file the production code did not write,
// for fixtures that start from counts already on disk.
func writeRespawnStateFile(t *testing.T, path string, state *beadRespawnState) {
	t.Helper()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
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

func TestRecordBeadRespawn_Increments(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	count := mustRecord(t, tmpDir, "bead-1")
	if count != 1 {
		t.Errorf("first RecordBeadRespawn = %d, want 1", count)
	}

	count = mustRecord(t, tmpDir, "bead-1")
	if count != 2 {
		t.Errorf("second RecordBeadRespawn = %d, want 2", count)
	}

	if _, err := os.Stat(respawnStatePath(tmpDir)); err != nil {
		t.Errorf("state not at .runtime/bead-respawn-counts.json: %v", err)
	}
	if _, err := os.Stat(legacyRespawnStatePath(tmpDir)); err == nil {
		t.Error("wrote the legacy witness/ state file")
	}
}

func TestShouldBlockRespawn_Threshold(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

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

	if ShouldBlockRespawn(tmpDir, "nonexistent") {
		t.Error("ShouldBlockRespawn = true for unknown bead")
	}
}

// TestRespawnReadsTheLegacyStateBeforeAnyMigration: the first read of a town
// built before the move happens before anything writes the new file, and it
// must already see the witness/ counts — reading them as absent hands every
// bead its respawn budget back (gt-la690).
func TestRespawnReadsTheLegacyStateBeforeAnyMigration(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	writeRespawnStateFile(t, legacyRespawnStatePath(tmpDir), &beadRespawnState{
		Beads: map[string]*beadRespawnRecord{
			"bead-blocked": {BeadID: "bead-blocked", Count: config.DefaultRecoveryMaxBeadRespawns},
			"bead-legacy":  {BeadID: "bead-legacy", Count: 2},
		},
	})

	if !ShouldBlockRespawn(tmpDir, "bead-blocked") {
		t.Error("ShouldBlockRespawn = false for a legacy count at the threshold")
	}

	state, err := loadBeadRespawnState(tmpDir)
	if err != nil {
		t.Fatalf("loadBeadRespawnState: %v", err)
	}
	if rec := state.Beads["bead-legacy"]; rec == nil || rec.Count != 2 {
		t.Errorf("legacy count read as %+v, want bead-legacy 2", rec)
	}

	// A read moves nothing: only a completed write clears the old file.
	if _, err := os.Stat(legacyRespawnStatePath(tmpDir)); err != nil {
		t.Errorf("the read-only path deleted the legacy state: %v", err)
	}
	if _, err := os.Stat(respawnStatePath(tmpDir)); err == nil {
		t.Error("the read-only path wrote the new state file")
	}
}

// TestRespawnMigrationMergesAndClearsTheLegacyFile: both files can hold real
// increments — the town ran the old and the new binary across the move — so the
// first write merges them per bead and only then clears witness/ (gt-la690).
func TestRespawnMigrationMergesAndClearsTheLegacyFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	older := time.Unix(1000, 0).UTC()
	newer := time.Unix(2000, 0).UTC()

	writeRespawnStateFile(t, respawnStatePath(tmpDir), &beadRespawnState{
		Beads: map[string]*beadRespawnRecord{
			"bead-a": {BeadID: "bead-a", Count: 2, LastRespawn: newer},
			"bead-b": {BeadID: "bead-b", Count: 1, LastRespawn: older},
		},
	})
	legacy := legacyRespawnStatePath(tmpDir)
	writeRespawnStateFile(t, legacy, &beadRespawnState{
		Beads: map[string]*beadRespawnRecord{
			"bead-a": {BeadID: "bead-a", Count: 5, LastRespawn: older},
			"bead-c": {BeadID: "bead-c", Count: 4, LastRespawn: older},
		},
	})
	if err := os.WriteFile(legacy+".flock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Another file in witness/, so the directory itself is not the migration's
	// to remove.
	keep := filepath.Join(tmpDir, "witness", "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}

	// bead-b is on both sides of the merge and incremented here: the write that
	// migrates is also the write that records.
	if count := mustRecord(t, tmpDir, "bead-b"); count != 2 {
		t.Errorf("RecordBeadRespawn on the migrating write = %d, want 2", count)
	}

	state, err := loadBeadRespawnState(tmpDir)
	if err != nil {
		t.Fatalf("loadBeadRespawnState after migration: %v", err)
	}
	if rec := state.Beads["bead-a"]; rec == nil || rec.Count != 5 || !rec.LastRespawn.Equal(newer) {
		t.Errorf("bead-a after merge = %+v, want count 5 and the later last_respawn", rec)
	}
	if rec := state.Beads["bead-c"]; rec == nil || rec.Count != 4 || !rec.LastRespawn.Equal(older) {
		t.Errorf("bead-c after merge = %+v, want the legacy record carried over", rec)
	}
	if rec := state.Beads["bead-b"]; rec == nil || rec.Count != 2 {
		t.Errorf("bead-b after merge = %+v, want count 2", rec)
	}

	if _, err := os.Stat(legacy); err == nil {
		t.Error("the legacy state file survived the migration")
	}
	if _, err := os.Stat(legacy + ".flock"); err == nil {
		t.Error("the legacy flock file survived the migration")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the migration removed a file it does not own: %v", err)
	}

	// The second write reads the new file alone and counts on from the merge.
	if count := mustRecord(t, tmpDir, "bead-b"); count != 3 {
		t.Errorf("RecordBeadRespawn after migration = %d, want 3", count)
	}
}

// TestRespawnMigrationRemovesTheEmptyWitnessDir: witness/ is empty once the
// state file and its flock are gone, and the migration is what makes the
// retired directory deletable (gt-la690).
func TestRespawnMigrationRemovesTheEmptyWitnessDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	writeRespawnStateFile(t, legacyRespawnStatePath(tmpDir), &beadRespawnState{
		Beads: map[string]*beadRespawnRecord{
			"bead-legacy": {BeadID: "bead-legacy", Count: 2},
		},
	})

	if count := mustRecord(t, tmpDir, "bead-legacy"); count != 3 {
		t.Errorf("RecordBeadRespawn over a legacy-only town = %d, want 3", count)
	}

	if _, err := os.Stat(filepath.Dir(legacyRespawnStatePath(tmpDir))); err == nil {
		t.Error("the empty witness directory survived the migration")
	}
}

// TestRespawnCorruptStateDeletesNothing: a file that will not parse stops the
// read-modify-write and leaves both files exactly as they were. Merging what
// could be read and deleting the rest would drop the counts that did not parse
// — the bead-budget reset the migration exists to prevent (gt-la690).
//
// The corrupt legacy file behaves differently on purpose — the reset moves that
// one aside (TestResetBeadRespawnCountQuarantinesCorruptLegacyFile) — so its
// refusals are asserted in TestRespawnCorruptLegacyStateRefusesWriters.
func TestRespawnCorruptStateDeletesNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	if err := os.MkdirAll(filepath.Dir(respawnStatePath(townRoot)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(respawnStatePath(townRoot), []byte(`{"beads":{"bead-1":{"count":3`), 0600); err != nil {
		t.Fatal(err)
	}
	writeRespawnStateFile(t, legacyRespawnStatePath(townRoot), &beadRespawnState{Beads: map[string]*beadRespawnRecord{
		"bead-1": {BeadID: "bead-1", Count: 2},
	}})

	before := map[string][]byte{}
	for _, path := range []string{respawnStatePath(townRoot), legacyRespawnStatePath(townRoot)} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}

	if _, err := loadBeadRespawnState(townRoot); err == nil {
		t.Error("loadBeadRespawnState = nil error, want a parse error")
	}
	if _, err := RecordBeadRespawn(townRoot, "bead-1"); err == nil {
		t.Error("RecordBeadRespawn = nil error, want a refusal")
	}
	if err := ResetBeadRespawnCount(townRoot, "bead-1"); err == nil {
		t.Error("ResetBeadRespawnCount = nil error, want a refusal")
	}

	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s was deleted: %v", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s was rewritten: got %s, want %s", path, got, want)
		}
	}
}

// TestRespawnCorruptLegacyStateRefusesWriters: readers and writers other than
// the reset refuse a corrupt legacy file and edit neither file, keeping its
// bytes for the manual recovery the quarantine exists to allow (gt-la690).
func TestRespawnCorruptLegacyStateRefusesWriters(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	writeRespawnStateFile(t, respawnStatePath(townRoot), &beadRespawnState{Beads: map[string]*beadRespawnRecord{
		"bead-1": {BeadID: "bead-1", Count: 2},
	}})
	legacy := legacyRespawnStatePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"beads":{"bead-1":{"count":3`), 0600); err != nil {
		t.Fatal(err)
	}

	before := map[string][]byte{}
	for _, path := range []string{respawnStatePath(townRoot), legacy} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}

	if _, err := loadBeadRespawnState(townRoot); err == nil {
		t.Error("loadBeadRespawnState = nil error, want a parse error")
	}
	if _, err := RecordBeadRespawn(townRoot, "bead-1"); err == nil {
		t.Error("RecordBeadRespawn = nil error, want a refusal")
	}

	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s was deleted: %v", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s was rewritten: got %s, want %s", path, got, want)
		}
	}
}

// TestResetBeadRespawnCountQuarantinesCorruptLegacyFile: a truncated legacy
// file blocks every bead and every writer refuses on it, so `gt sling
// respawn-reset` is the way out: it moves the file aside, resets the bead and
// leaves the live counts of the others alone. The refusal on the way in names
// that command (gt-jw3ed).
func TestResetBeadRespawnCountQuarantinesCorruptLegacyFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeRespawnStateFile(t, respawnStatePath(townRoot), &beadRespawnState{Beads: map[string]*beadRespawnRecord{
		"bead-1": {BeadID: "bead-1", Count: config.DefaultRecoveryMaxBeadRespawns},
		"bead-2": {BeadID: "bead-2", Count: 2},
	}})
	truncated := []byte(`{"beads":{"bead-1":{"count":3`)
	legacy := legacyRespawnStatePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, truncated, 0600); err != nil {
		t.Fatal(err)
	}

	_, err := loadBeadRespawnState(townRoot)
	if err == nil || !strings.Contains(err.Error(), "gt sling respawn-reset") {
		t.Errorf("loadBeadRespawnState error = %v, want one naming gt sling respawn-reset", err)
	}
	if !ShouldBlockRespawn(townRoot, "bead-3") {
		t.Fatal("ShouldBlockRespawn = false on a corrupt legacy file, want true until the reset runs")
	}

	if err := ResetBeadRespawnCount(townRoot, "bead-1"); err != nil {
		t.Fatalf("ResetBeadRespawnCount on a corrupt legacy file: %v", err)
	}

	if _, err := os.Stat(legacy); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("corrupt legacy file still in place: %v", err)
	}
	aside, err := filepath.Glob(legacy + ".corrupt-*")
	if err != nil || len(aside) != 1 {
		t.Fatalf("quarantined files = %v (err %v), want exactly one", aside, err)
	}
	if got, err := os.ReadFile(aside[0]); err != nil || string(got) != string(truncated) {
		t.Errorf("quarantined file = %q (err %v), want the original bytes", got, err)
	}
	if ShouldBlockRespawn(townRoot, "bead-1") {
		t.Error("ShouldBlockRespawn(bead-1) = true after the reset, want false")
	}
	state, err := loadBeadRespawnState(townRoot)
	if err != nil {
		t.Fatalf("loadBeadRespawnState after the reset: %v", err)
	}
	if rec := state.Beads["bead-2"]; rec == nil || rec.Count != 2 {
		t.Errorf("bead-2 record = %+v, want its count of 2 kept", rec)
	}
}

// TestResetBeadRespawnCountReportsAFailedQuarantine: when the corrupt legacy
// file cannot be moved, the reset fails and changes nothing — the file keeps
// its bytes, the bead keeps its count — and the error names both the parse
// failure and the failed move, so the operator knows the town is still blocked
// and why (gt-601lm).
func TestResetBeadRespawnCountReportsAFailedQuarantine(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	live := &beadRespawnState{Beads: map[string]*beadRespawnRecord{
		"bead-1": {BeadID: "bead-1", Count: 2},
	}}
	writeRespawnStateFile(t, respawnStatePath(townRoot), live)
	legacy := legacyRespawnStatePath(townRoot)
	truncated := []byte(`{"beads":{"bead-1":{"count":3`)
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, truncated, 0600); err != nil {
		t.Fatal(err)
	}
	// A read-only witness directory makes the rename fail the way a full or
	// read-only filesystem does, which is the failure this branch reports.
	witnessDir := filepath.Dir(legacy)
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit
	if err := os.Chmod(witnessDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
		_ = os.Chmod(witnessDir, 0o755)
	})

	err := ResetBeadRespawnCount(townRoot, "bead-1")
	if err == nil {
		t.Fatal("ResetBeadRespawnCount = nil error, want the failed quarantine reported")
	}
	for _, want := range []string{"parsing respawn state", "moving corrupt respawn state aside"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ResetBeadRespawnCount error = %v, want it to name %q", err, want)
		}
	}

	if got, rerr := os.ReadFile(legacy); rerr != nil || string(got) != string(truncated) {
		t.Errorf("corrupt legacy file = %q (err %v), want its bytes untouched", got, rerr)
	}
	if aside, _ := filepath.Glob(legacy + ".corrupt-*"); len(aside) != 0 {
		t.Errorf("quarantined files = %v, want none when the move failed", aside)
	}
	state, rerr := readBeadRespawnStateFile(respawnStatePath(townRoot))
	if rerr != nil {
		t.Fatalf("live state: %v", rerr)
	}
	if rec := state.Beads["bead-1"]; rec == nil || rec.Count != 2 {
		t.Errorf("bead-1 record = %+v, want its count of 2 left alone", rec)
	}
}

// TestQuarantineLegacyRespawnStateToleratesAnAlreadyGoneFile: someone else can
// remove the corrupt file between the read that found it and the rename. The
// reset wants it gone, so a missing file is success with nothing to report
// rather than an error that leaves the town blocked a second time (gt-601lm).
func TestQuarantineLegacyRespawnStateToleratesAnAlreadyGoneFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	legacy := legacyRespawnStatePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}

	aside, err := quarantineLegacyRespawnState(townRoot)
	if err != nil {
		t.Fatalf("quarantineLegacyRespawnState on a missing file: %v", err)
	}
	if aside != "" {
		t.Errorf("aside = %q, want empty when there was nothing to move", aside)
	}
	if moved, _ := filepath.Glob(legacy + ".corrupt-*"); len(moved) != 0 {
		t.Errorf("quarantined files = %v, want none", moved)
	}
}

// TestRespawnStateTreatsATruncatedFileAsCorruption: a state file that does not
// parse is reported, never read as an empty state. Reading it as empty re-armed
// the breaker for every bead at once — one truncated write and a town's whole
// respawn budget was gone (gt-u3hc1).
func TestRespawnStateTreatsATruncatedFileAsCorruption(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	for i := 0; i < config.DefaultRecoveryMaxBeadRespawns; i++ {
		mustRecord(t, tmpDir, "bead-corrupt")
	}
	if !ShouldBlockRespawn(tmpDir, "bead-corrupt") {
		t.Fatal("breaker not armed by the fixture")
	}

	// The half-written file a crash between truncate and write leaves behind.
	stateFile := respawnStatePath(tmpDir)
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
	if err := saveBeadRespawnState(tmpDir, emptyBeadRespawnState()); err != nil {
		t.Fatal(err)
	}
	// A directory where the lock file belongs: flockAcquire opens it read-write
	// and gets EISDIR. Unlike a mode, that fails for every uid.
	if err := os.MkdirAll(respawnStatePath(tmpDir)+".flock", 0755); err != nil {
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
	stateFile := respawnStatePath(tmpDir)
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		t.Fatal(err)
	}
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
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(stateFile), ".bead-respawn-counts-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// TestRespawnStateCreatesTheStateDir: the flock file's open creates no parent
// directories, so a first run in a fresh town failed to take the lock at all.
// The lock file standing beside the state file is what shows the lock was
// taken rather than skipped (gt-u3hc1).
func TestRespawnStateCreatesTheStateDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir() // no .runtime/ subdirectory

	mustRecord(t, tmpDir, "bead-fresh")

	flock := respawnStatePath(tmpDir) + ".flock"
	if _, err := os.Stat(flock); err != nil {
		t.Errorf("no lock file after the first record, so the lock was never taken: %v", err)
	}
	if ShouldBlockRespawn(tmpDir, "bead-fresh") {
		t.Error("ShouldBlockRespawn = true after a single respawn")
	}
}

// TestRespawnLimitSparesReworkRounds: a bead slung four times, each sling after
// the landing worker refused its last submission, is still dispatchable
// (gt-tatcn).
func TestRespawnLimitSparesReworkRounds(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const bead = "bead-rework"
	base := time.Now().UTC()

	for round := 1; round <= 4; round++ {
		// Each round's submission lands after the sling that started it,
		// the state the bead is slung from after a refusal.
		cleared, err := ResetBeadRespawnForRound(townRoot, bead, base.Add(time.Duration(round)*time.Minute))
		if err != nil {
			t.Fatalf("round %d: ResetBeadRespawnForRound: %v", round, err)
		}
		if round == 1 && cleared {
			t.Fatalf("round %d cleared a count the bead had not spent yet", round)
		}
		if round > 1 && !cleared {
			t.Errorf("round %d cleared nothing; the round before it submitted", round)
		}
		if ShouldBlockRespawn(townRoot, bead) {
			t.Fatalf("round %d was refused: four rounds of work spent the bead's respawn budget", round)
		}
		mustRecord(t, townRoot, bead)
	}
}

// TestRespawnLimitHoldsForAReworkBeadThatKeepsDying: the bead came back from a
// refusal and its polecat has died at start ever since. The refusal is not a
// licence to keep slinging it — the spawns since the submission are deaths, and
// the fourth sling is refused (gt-tatcn).
func TestRespawnLimitHoldsForAReworkBeadThatKeepsDying(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const bead = "bead-rework-dying"

	// The submission of the round the landing worker refused, before every
	// spawn below it.
	refused := time.Now().UTC().Add(-time.Minute)
	for attempt := 1; attempt <= config.DefaultRecoveryMaxBeadRespawns; attempt++ {
		cleared, err := ResetBeadRespawnForRound(townRoot, bead, refused)
		if err != nil {
			t.Fatalf("attempt %d: ResetBeadRespawnForRound: %v", attempt, err)
		}
		if cleared && attempt > 1 {
			t.Fatalf("attempt %d cleared the death the attempt before it recorded", attempt)
		}
		if ShouldBlockRespawn(townRoot, bead) {
			t.Fatalf("attempt %d was refused; the bead dies before it can spend its own limit", attempt)
		}
		mustRecord(t, townRoot, bead)
	}
	if !ShouldBlockRespawn(townRoot, bead) {
		t.Error("ShouldBlockRespawn after three deaths = false; want the rework bead refused")
	}
}

// TestRespawnLimitHoldsForStartupDeaths: a bead whose polecat dies before it
// can submit three times is still refused a fourth sling (gt-tatcn, gt-u3hc1).
func TestRespawnLimitHoldsForStartupDeaths(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const bead = "bead-dying"

	for attempt := 1; attempt <= config.DefaultRecoveryMaxBeadRespawns; attempt++ {
		// Nothing to clear: the bead carries no submission.
		cleared, err := ResetBeadRespawnForRound(townRoot, bead, time.Time{})
		if err != nil {
			t.Fatalf("attempt %d: ResetBeadRespawnForRound: %v", attempt, err)
		}
		if cleared {
			t.Fatalf("attempt %d cleared a count with no submission behind it", attempt)
		}
		if ShouldBlockRespawn(townRoot, bead) {
			t.Fatalf("attempt %d was refused; the bead dies before it can spend its own limit", attempt)
		}
		mustRecord(t, townRoot, bead)
	}
	if !ShouldBlockRespawn(townRoot, bead) {
		t.Error("ShouldBlockRespawn after three deaths = false; want the bead refused")
	}
}

// TestResetBeadRespawnForRound_StaleSubmissionIsNotARound: a submission older
// than the last respawn belongs to the round before it, and that round's sling
// never submitted — the death the limit counts (gt-tatcn).
func TestResetBeadRespawnForRound_StaleSubmissionIsNotARound(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const bead = "bead-stale"

	mustRecord(t, townRoot, bead)
	cleared, err := ResetBeadRespawnForRound(townRoot, bead, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ResetBeadRespawnForRound: %v", err)
	}
	if cleared {
		t.Error("a submission older than the last respawn cleared the count")
	}
	if count := mustRecord(t, townRoot, bead); count != 2 {
		t.Errorf("count after the sling = %d, want 2", count)
	}
}

// TestResetBeadRespawnForRound_UnreadableStateIsReported: a state file that
// cannot be read is an error, never a silent clear — the count is the only
// record that the bead is near its limit (gt-u3hc1).
func TestResetBeadRespawnForRound_UnreadableStateIsReported(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const bead = "bead-corrupt"

	mustRecord(t, townRoot, bead)
	// The half-written file a crash between truncate and write leaves behind.
	if err := os.WriteFile(respawnStatePath(townRoot), []byte(`{"beads":{"bead-corrupt":{"count":1`), 0600); err != nil {
		t.Fatal(err)
	}

	cleared, err := ResetBeadRespawnForRound(townRoot, bead, time.Now().UTC())
	if err == nil {
		t.Error("ResetBeadRespawnForRound on a truncated file = nil error, want a refusal")
	}
	if cleared {
		t.Error("ResetBeadRespawnForRound on a truncated file reported a cleared count")
	}
}
