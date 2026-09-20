package witness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeWitnessState writes raw JSON as a rig witness's state.json and returns
// its path. Raw bytes, not a struct: the witness owns this file's shape.
func writeWitnessState(t *testing.T, townRoot, rig, contents string) string {
	t.Helper()
	dir := WitnessStateDir(townRoot, rig)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, patrolStateFileName)
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestWitnessStatePathPrefersLegacyRigDir(t *testing.T) {
	townRoot := t.TempDir()

	// No witness/rig means the modern layout: <town>/<rig>/witness/state.json.
	want := filepath.Join(townRoot, "myrig", "witness", "state.json")
	if got := WitnessStatePath(townRoot, "myrig"); got != want {
		t.Errorf("WitnessStatePath = %q, want %q", got, want)
	}

	// A witness/rig directory means a legacy clone; the running witness writes
	// there, so the path must follow it.
	if err := os.MkdirAll(filepath.Join(townRoot, "myrig", "witness", "rig"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want = filepath.Join(townRoot, "myrig", "witness", "rig", "state.json")
	if got := WitnessStatePath(townRoot, "myrig"); got != want {
		t.Errorf("WitnessStatePath with witness/rig = %q, want %q", got, want)
	}
}

func TestNormalizePatrolCounterZeroesAndPreservesEverythingElse(t *testing.T) {
	townRoot := t.TempDir()
	path := writeWitnessState(t, townRoot, "myrig", `{
  "patrol_count": 610,
  "extraordinary_action": false,
  "last_patrol": "2026-09-20T05:20:00Z",
  "polecats": {"opal": {"status": "idle-clean"}},
  "unknown_future_field": [1, 2, 3]
}`)

	changed, err := NormalizePatrolCounter(townRoot, "myrig")
	if err != nil {
		t.Fatalf("NormalizePatrolCounter: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for patrol_count 610")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("parse back: %v", err)
	}

	var count int
	if err := json.Unmarshal(fields["patrol_count"], &count); err != nil {
		t.Fatalf("parse patrol_count: %v", err)
	}
	if count != 0 {
		t.Errorf("patrol_count = %d, want 0", count)
	}

	// Every other field must survive verbatim — this file is the witness's own
	// and carries state Go knows nothing about.
	for _, key := range []string{"extraordinary_action", "last_patrol", "polecats", "unknown_future_field"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("field %q was dropped by the rewrite", key)
		}
	}
	var lastPatrol string
	if err := json.Unmarshal(fields["last_patrol"], &lastPatrol); err != nil {
		t.Fatalf("parse last_patrol: %v", err)
	}
	if lastPatrol != "2026-09-20T05:20:00Z" {
		t.Errorf("last_patrol = %q, want it preserved", lastPatrol)
	}
}

func TestNormalizePatrolCounterNoRewriteWhenAlreadyZero(t *testing.T) {
	townRoot := t.TempDir()
	path := writeWitnessState(t, townRoot, "myrig", "{\n  \"patrol_count\": 0,\n  \"notes\": \"x\"\n}\n")

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	changed, err := NormalizePatrolCounter(townRoot, "myrig")
	if err != nil {
		t.Fatalf("NormalizePatrolCounter: %v", err)
	}
	if changed {
		t.Error("changed = true, want false when patrol_count is already 0")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != string(original) {
		t.Error("file was rewritten despite being already normalized")
	}
	if info, err := os.Stat(path); err == nil && info.ModTime() != before.ModTime() {
		t.Error("mtime changed despite no rewrite")
	}
}

func TestNormalizePatrolCounterMissingFileIsNoOp(t *testing.T) {
	townRoot := t.TempDir()

	changed, err := NormalizePatrolCounter(townRoot, "myrig")
	if err != nil {
		t.Fatalf("NormalizePatrolCounter on missing file: %v", err)
	}
	if changed {
		t.Error("changed = true, want false for a missing state file")
	}
	if _, err := os.Stat(WitnessStatePath(townRoot, "myrig")); !os.IsNotExist(err) {
		t.Error("a missing state file must not be created")
	}
}

func TestNormalizePatrolCounterAbsentFieldIsNoOp(t *testing.T) {
	townRoot := t.TempDir()
	path := writeWitnessState(t, townRoot, "myrig", `{"notes": "no counter here"}`)

	changed, err := NormalizePatrolCounter(townRoot, "myrig")
	if err != nil {
		t.Fatalf("NormalizePatrolCounter: %v", err)
	}
	if changed {
		t.Error("changed = true, want false when patrol_count is absent")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("parse back: %v", err)
	}
	if _, ok := fields["patrol_count"]; ok {
		t.Error("a counter the witness never wrote must not be added")
	}
}

// A state file we cannot parse is not one to overwrite: the witness's notes
// live in there, and a half-written file (it writes this by hand) must not be
// replaced with an empty map.
func TestNormalizePatrolCounterUnparseableLeavesFileUntouched(t *testing.T) {
	townRoot := t.TempDir()
	const corrupt = `{"patrol_count": 610, "notes": "truncated`
	path := writeWitnessState(t, townRoot, "myrig", corrupt)

	changed, err := NormalizePatrolCounter(townRoot, "myrig")
	if err == nil {
		t.Fatal("expected an error on unparseable JSON")
	}
	if changed {
		t.Error("changed = true on an unparseable file")
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if string(data) != corrupt {
		t.Error("unparseable state file was modified")
	}
}

// A non-numeric counter is equally suspicious — report it rather than writing
// a value over whatever the witness meant.
func TestNormalizePatrolCounterNonNumericCounterErrors(t *testing.T) {
	townRoot := t.TempDir()
	const contents = `{"patrol_count": "many"}`
	path := writeWitnessState(t, townRoot, "myrig", contents)

	changed, err := NormalizePatrolCounter(townRoot, "myrig")
	if err == nil {
		t.Fatal("expected an error on a non-numeric patrol_count")
	}
	if changed {
		t.Error("changed = true on a non-numeric counter")
	}
	if data, _ := os.ReadFile(path); string(data) != contents {
		t.Error("file was modified on a non-numeric counter")
	}
}

func TestNormalizePatrolCounterPreservesFileMode(t *testing.T) {
	townRoot := t.TempDir()
	path := writeWitnessState(t, townRoot, "myrig", `{"patrol_count": 42}`)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := NormalizePatrolCounter(townRoot, "myrig"); err != nil {
		t.Fatalf("NormalizePatrolCounter: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

// Leaves no temp files behind, on either path.
func TestNormalizePatrolCounterLeavesNoTempFiles(t *testing.T) {
	for _, contents := range []string{`{"patrol_count": 7}`, `{"patrol_count": 0}`} {
		townRoot := t.TempDir()
		writeWitnessState(t, townRoot, "myrig", contents)
		if _, err := NormalizePatrolCounter(townRoot, "myrig"); err != nil {
			t.Fatalf("NormalizePatrolCounter: %v", err)
		}

		entries, err := os.ReadDir(WitnessStateDir(townRoot, "myrig"))
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, e := range entries {
			if e.Name() != patrolStateFileName {
				t.Errorf("leftover file %q for contents %s", e.Name(), contents)
			}
		}
	}
}
