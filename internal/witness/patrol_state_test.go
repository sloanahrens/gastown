package witness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// staleWitnessState is the shape the witness left behind on the om rig when
// this bug stalled it (gt-oabl): a patrol counter far past the handoff ceiling,
// an extraordinary-action flag still set, and the continuity fields that must
// survive the reset.
const staleWitnessState = `{
  "patrol_count": 602,
  "extraordinary_action": true,
  "last_patrol": "2026-09-20T04:05:00Z",
  "session_note": "NEW SESSION started 2026-09-20 03:31Z",
  "nudges": {"mayor": {"count": 1, "last": "2026-09-20T04:04:00Z"}},
  "pending_cleanup": ["read me first"]
}`

func writeState(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := PatrolStatePath(dir)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func decodeState(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fields
}

// assertField compares a field by value, not by bytes: the reset re-indents
// the file, and a preserved field may legitimately come back formatted
// differently.
func assertField(t *testing.T, fields map[string]json.RawMessage, key, want string) {
	t.Helper()
	got, ok := fields[key]
	if !ok {
		t.Fatalf("field %q missing from state file", key)
	}
	var gotValue, wantValue interface{}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("field %q is not valid JSON: %v", key, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("want value for %q is not valid JSON: %v", key, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("field %q = %s, want %s", key, got, want)
	}
	if strings.TrimSpace(string(got)) == "" {
		t.Errorf("field %q is blank", key)
	}
}

// TestResetPatrolState_ClearsInheritedCounter is the regression guard for
// gt-oabl: a session that starts with a stale counter hands off after a single
// patrol cycle and goes idle forever, so the counter and the flag must be
// cleared while the witness's continuity fields survive.
func TestResetPatrolState_ClearsInheritedCounter(t *testing.T) {
	dir := t.TempDir()
	path := writeState(t, dir, staleWitnessState)

	changed, err := ResetPatrolState(dir)
	if err != nil {
		t.Fatalf("ResetPatrolState: %v", err)
	}
	if !changed {
		t.Fatal("ResetPatrolState reported no change for a stale counter")
	}

	fields := decodeState(t, path)
	assertField(t, fields, "patrol_count", "0")
	assertField(t, fields, "extraordinary_action", "false")
	// Everything else is the successor session's continuity and must be kept.
	assertField(t, fields, "last_patrol", `"2026-09-20T04:05:00Z"`)
	assertField(t, fields, "session_note", `"NEW SESSION started 2026-09-20 03:31Z"`)
	assertField(t, fields, "nudges", `{"mayor": {"count": 1, "last": "2026-09-20T04:04:00Z"}}`)
	assertField(t, fields, "pending_cleanup", `["read me first"]`)
}

func TestResetPatrolState_NoStateFileIsANoOp(t *testing.T) {
	dir := t.TempDir()

	changed, err := ResetPatrolState(dir)
	if err != nil {
		t.Fatalf("ResetPatrolState on a missing file: %v", err)
	}
	if changed {
		t.Error("ResetPatrolState reported a change with no state file present")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ResetPatrolState created %d file(s) where there was no state", len(entries))
	}
}

// TestResetPatrolState_AlreadyResetIsNotRewritten keeps a healthy witness from
// having its state file reformatted under it on every session start.
func TestResetPatrolState_AlreadyResetIsNotRewritten(t *testing.T) {
	dir := t.TempDir()
	original := `{"patrol_count": 0, "extraordinary_action": false, "session_note": "keep"}`
	path := writeState(t, dir, original)

	changed, err := ResetPatrolState(dir)
	if err != nil {
		t.Fatalf("ResetPatrolState: %v", err)
	}
	if changed {
		t.Error("ResetPatrolState rewrote an already-reset state file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != original {
		t.Errorf("state file changed:\n got: %s\nwant: %s", got, original)
	}
}

func TestResetPatrolState_FillsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeState(t, dir, `{"session_note": "keep"}`)

	changed, err := ResetPatrolState(dir)
	if err != nil {
		t.Fatalf("ResetPatrolState: %v", err)
	}
	if !changed {
		t.Fatal("ResetPatrolState left a state file without patrol keys untouched")
	}
	fields := decodeState(t, path)
	assertField(t, fields, "patrol_count", "0")
	assertField(t, fields, "extraordinary_action", "false")
	assertField(t, fields, "session_note", `"keep"`)
}

// TestResetPatrolState_UnparseableStateIsLeftAlone: a state file we cannot
// understand is evidence, not garbage — report it and destroy nothing.
func TestResetPatrolState_UnparseableStateIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	corrupt := `{"patrol_count": 602,`
	path := writeState(t, dir, corrupt)

	changed, err := ResetPatrolState(dir)
	if err == nil {
		t.Fatal("ResetPatrolState accepted an unparseable state file")
	}
	if changed {
		t.Error("ResetPatrolState reported a change for an unparseable state file")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read %s: %v", path, readErr)
	}
	if string(got) != corrupt {
		t.Errorf("ResetPatrolState modified an unparseable state file: %s", got)
	}
}

func TestResetPatrolState_PreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := writeState(t, dir, staleWitnessState)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := ResetPatrolState(dir); err != nil {
		t.Fatalf("ResetPatrolState: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("state file mode = %o, want 600", got)
	}
}

func TestWitnessDir_PrefersLegacyRigSubdir(t *testing.T) {
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "witness"), 0o755); err != nil {
		t.Fatalf("mkdir witness: %v", err)
	}

	if got, want := WitnessDir(rigPath), filepath.Join(rigPath, "witness"); got != want {
		t.Errorf("WitnessDir = %s, want %s", got, want)
	}

	legacy := filepath.Join(rigPath, "witness", "rig")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatalf("mkdir witness/rig: %v", err)
	}
	if got := WitnessDir(rigPath); got != legacy {
		t.Errorf("WitnessDir = %s, want the legacy clone %s", got, legacy)
	}
}
