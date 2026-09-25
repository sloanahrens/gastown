package patrolstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeState(t *testing.T, dir, name, contents string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestResetCounterZeroesAndPreservesEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := writeState(t, dir, "state.json", `{
  "patrol_count": 610,
  "extraordinary_action": false,
  "last_patrol": "2026-09-20T05:20:00Z",
  "unknown_future_field": [1, 2, 3]
}`)

	changed, err := ResetCounter(path, "patrol_count")
	if err != nil {
		t.Fatalf("ResetCounter: %v", err)
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
	for _, key := range []string{"extraordinary_action", "last_patrol", "unknown_future_field"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("field %q was dropped by the rewrite", key)
		}
	}
}

func TestResetCounterNoRewriteWhenAlreadyZero(t *testing.T) {
	dir := t.TempDir()
	path := writeState(t, dir, "state.json", "{\n  \"patrol_count\": 0,\n  \"notes\": \"x\"\n}\n")

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	changed, err := ResetCounter(path, "patrol_count")
	if err != nil {
		t.Fatalf("ResetCounter: %v", err)
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

func TestResetCounterMissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	changed, err := ResetCounter(path, "patrol_count")
	if err != nil {
		t.Fatalf("ResetCounter on missing file: %v", err)
	}
	if changed {
		t.Error("changed = true, want false for a missing state file")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a missing state file must not be created")
	}
}

func TestResetCounterAbsentFieldIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := writeState(t, dir, "state.json", `{"notes": "no counter here"}`)

	changed, err := ResetCounter(path, "patrol_count")
	if err != nil {
		t.Fatalf("ResetCounter: %v", err)
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
		t.Error("a counter that was never written must not be added")
	}
}

func TestResetCounterUnparseableLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	const corrupt = `{"patrol_count": 610, "notes": "truncated`
	path := writeState(t, dir, "state.json", corrupt)

	changed, err := ResetCounter(path, "patrol_count")
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

func TestResetCounterNonNumericCounterErrors(t *testing.T) {
	dir := t.TempDir()
	const contents = `{"patrol_count": "many"}`
	path := writeState(t, dir, "state.json", contents)

	changed, err := ResetCounter(path, "patrol_count")
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

func TestResetCounterPreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := writeState(t, dir, "state.json", `{"patrol_count": 42}`)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := ResetCounter(path, "patrol_count"); err != nil {
		t.Fatalf("ResetCounter: %v", err)
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
func TestResetCounterLeavesNoTempFiles(t *testing.T) {
	for _, contents := range []string{`{"patrol_count": 7}`, `{"patrol_count": 0}`} {
		dir := t.TempDir()
		writeState(t, dir, "state.json", contents)
		if _, err := ResetCounter(filepath.Join(dir, "state.json"), "patrol_count"); err != nil {
			t.Fatalf("ResetCounter: %v", err)
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, e := range entries {
			if e.Name() != "state.json" {
				t.Errorf("leftover file %q for contents %s", e.Name(), contents)
			}
		}
	}
}
