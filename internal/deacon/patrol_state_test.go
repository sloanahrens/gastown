package deacon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeDeaconState writes raw JSON as the deacon's state.json and returns its
// path. Raw bytes, not a struct: this file is agent-managed (the deacon
// writes it by hand, the same way the witness owns its own state.json before
// gt-oabl), so a field Go knows nothing about must survive a rewrite.
func writeDeaconState(t *testing.T, townRoot, contents string) string {
	t.Helper()
	dir := DeaconStateDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, patrolStateFileName)
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestResetPatrolCountZeroesAndPreservesEverythingElse(t *testing.T) {
	townRoot := t.TempDir()
	path := writeDeaconState(t, townRoot, `{
  "patrol_count": 29,
  "extraordinary_action": false,
  "last_patrol": "2026-09-24T20:20:00Z",
  "unknown_future_field": [1, 2, 3]
}`)

	changed, err := ResetPatrolCount(townRoot)
	if err != nil {
		t.Fatalf("ResetPatrolCount: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for patrol_count 29")
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

	for _, key := range []string{"extraordinary_action", "last_patrol", "unknown_future_field"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("field %q was dropped by the rewrite", key)
		}
	}
	var lastPatrol string
	if err := json.Unmarshal(fields["last_patrol"], &lastPatrol); err != nil {
		t.Fatalf("parse last_patrol: %v", err)
	}
	if lastPatrol != "2026-09-24T20:20:00Z" {
		t.Errorf("last_patrol = %q, want it preserved", lastPatrol)
	}
}

func TestResetPatrolCountNoRewriteWhenAlreadyZero(t *testing.T) {
	townRoot := t.TempDir()
	path := writeDeaconState(t, townRoot, "{\n  \"patrol_count\": 0,\n  \"notes\": \"x\"\n}\n")

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	changed, err := ResetPatrolCount(townRoot)
	if err != nil {
		t.Fatalf("ResetPatrolCount: %v", err)
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
}

func TestResetPatrolCountMissingFileIsNoOp(t *testing.T) {
	townRoot := t.TempDir()

	changed, err := ResetPatrolCount(townRoot)
	if err != nil {
		t.Fatalf("ResetPatrolCount on missing file: %v", err)
	}
	if changed {
		t.Error("changed = true, want false for a missing state file")
	}
	if _, err := os.Stat(DeaconStatePath(townRoot)); !os.IsNotExist(err) {
		t.Error("a missing state file must not be created")
	}
}

func TestResetPatrolCountAbsentFieldIsNoOp(t *testing.T) {
	townRoot := t.TempDir()
	path := writeDeaconState(t, townRoot, `{"notes": "no counter here"}`)

	changed, err := ResetPatrolCount(townRoot)
	if err != nil {
		t.Fatalf("ResetPatrolCount: %v", err)
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
		t.Error("a counter the deacon never wrote must not be added")
	}
}

// A state file we cannot parse is not one to overwrite: the deacon's own
// notes live in there, and a half-written file must not be replaced.
func TestResetPatrolCountUnparseableLeavesFileUntouched(t *testing.T) {
	townRoot := t.TempDir()
	const corrupt = `{"patrol_count": 29, "notes": "truncated`
	path := writeDeaconState(t, townRoot, corrupt)

	changed, err := ResetPatrolCount(townRoot)
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
