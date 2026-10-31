package session

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readSeatLines reads the town's seat-end ledger.
func readSeatLines(t *testing.T, town string) []sandboxSeatEndLine {
	t.Helper()
	data, err := os.ReadFile(SandboxSeatLedgerPath(town))
	if err != nil {
		t.Fatalf("reading the seat-end ledger: %v", err)
	}
	var lines []sandboxSeatEndLine
	for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var line sandboxSeatEndLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("decoding seat-end line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestRemoveSandboxRecordAppendsSeatLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rec := SandboxRecord{
		Session:   "gt-gastown-box",
		Rig:       "gastown",
		Polecat:   "box",
		Workspace: "customer-zero",
		Profile:   "polecat-default",
		Image:     "img:dev",
		CreatedAt: time.Date(2026, 10, 31, 9, 30, 0, 0, time.FixedZone("PDT", -7*60*60)),
	}
	if err := WriteSandboxRecord(town, rec); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	before := time.Now()
	if err := RemoveSandboxRecord(town, rec.Session); err != nil {
		t.Fatalf("removing the record: %v", err)
	}
	after := time.Now()
	if _, ok, err := ReadSandboxRecord(town, rec.Session); ok || err != nil {
		t.Fatalf("record after removal: ok=%v err=%v, want gone", ok, err)
	}
	lines := readSeatLines(t, town)
	if len(lines) != 1 {
		t.Fatalf("seat-end lines = %d, want 1: %+v", len(lines), lines)
	}
	got := lines[0]
	if got.Session != rec.Session || got.Rig != rec.Rig || got.Polecat != rec.Polecat ||
		got.Workspace != rec.Workspace || got.Profile != rec.Profile {
		t.Fatalf("seat-end line = %+v, want the record's fields %+v", got, rec)
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) {
		t.Fatalf("created_at = %s, want the record's %s", got.CreatedAt, rec.CreatedAt)
	}
	if got.EndedAt.Before(before) || got.EndedAt.After(after) {
		t.Fatalf("ended_at = %s, want between %s and %s", got.EndedAt, before, after)
	}
	if got.CreatedAt.Location() != time.UTC || got.EndedAt.Location() != time.UTC {
		t.Fatalf("times = created %s ended %s, want both UTC",
			got.CreatedAt.Location(), got.EndedAt.Location())
	}
	raw, err := os.ReadFile(SandboxSeatLedgerPath(town))
	if err != nil {
		t.Fatalf("reading the ledger's text: %v", err)
	}
	if !strings.Contains(string(raw), `"created_at":"2026-10-31T16:30:00Z"`) {
		t.Fatalf("created_at is not RFC3339 UTC: %s", raw)
	}
}

func TestSeatLineEndedAtIsTheInjectedClock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	ended := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rec := SandboxRecord{Session: "gt-gastown-box", Rig: "gastown", Polecat: "box"}
	if err := WriteSandboxRecord(town, rec); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	if err := removeSandboxRecord(town, rec.Session, func() time.Time { return ended }); err != nil {
		t.Fatalf("removing the record: %v", err)
	}
	raw, err := os.ReadFile(SandboxSeatLedgerPath(town))
	if err != nil {
		t.Fatalf("reading the ledger's text: %v", err)
	}
	if !strings.Contains(string(raw), `"ended_at":"2026-10-31T12:00:00Z"`) {
		t.Fatalf("ended_at is not the injected clock at RFC3339 UTC: %s", raw)
	}
}

func TestRemoveMissingRecordWritesNoLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := RemoveSandboxRecord(town, "gt-gastown-box"); err != nil {
		t.Fatalf("removing a record that does not exist: %v", err)
	}
	if _, err := os.Stat(SandboxSeatLedgerPath(town)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ledger stat = %v, want no ledger for a record that never existed", err)
	}
}

func TestSeatLineAppendFailureStillRemoves(t *testing.T) {
	t.Parallel()
	// Each case blocks the ledger one step later than the last, without relying
	// on permission bits the test user may not be bound by.
	cases := []struct {
		name  string
		block func(t *testing.T, town string)
	}{
		{
			name: "the ledger directory cannot be made",
			block: func(t *testing.T, town string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
					t.Fatalf("creating .runtime: %v", err)
				}
				if err := os.WriteFile(filepath.Join(town, ".runtime", "metrics"), []byte("not a directory"), 0o644); err != nil {
					t.Fatalf("blocking the ledger directory: %v", err)
				}
			},
		},
		{
			name: "the ledger file cannot be opened",
			block: func(t *testing.T, town string) {
				t.Helper()
				if err := os.MkdirAll(SandboxSeatLedgerPath(town), 0o755); err != nil {
					t.Fatalf("putting a directory at the ledger path: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tc.block(t, town)
			rec := SandboxRecord{Session: "gt-gastown-box", Rig: "gastown", Polecat: "box"}
			if err := WriteSandboxRecord(town, rec); err != nil {
				t.Fatalf("writing the record: %v", err)
			}
			if err := RemoveSandboxRecord(town, rec.Session); err == nil {
				t.Fatal("an unwritable ledger path removed the record without an error")
			}
			if _, ok, err := ReadSandboxRecord(town, rec.Session); ok || err != nil {
				t.Fatalf("record after the failed append: ok=%v err=%v, want gone", ok, err)
			}
		})
	}
}

func TestSeatLineWriteFailureIsReturned(t *testing.T) {
	t.Parallel()
	_, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening a pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the pipe's write end: %v", err)
	}
	if err := writeSeatLine(w, []byte(`{"session":"gt-gastown-box"}`)); err == nil {
		t.Fatal("a write to a closed file returned no error")
	}
}

func TestRemoveUnreadableRecordStillRemoves(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(SandboxRegistryDir(town), 0o755); err != nil {
		t.Fatalf("creating the registry: %v", err)
	}
	path := filepath.Join(SandboxRegistryDir(town), "gt-gastown-box.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("writing the unreadable record: %v", err)
	}
	if err := RemoveSandboxRecord(town, "gt-gastown-box"); err == nil {
		t.Fatal("an unreadable record was removed without an error")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("record stat = %v, want gone", err)
	}
	if _, err := os.Stat(SandboxSeatLedgerPath(town)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ledger stat = %v, want no line for a record that could not be read", err)
	}
}

func TestOpenShellPrepareRecordsTheRigsWorkspace(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"),
		`{"default":"customer-zero","rigs":{"`+openshellTestRig+`":"acme"}}`)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	rec, ok, err := ReadSandboxRecord(town, openshellTestSession)
	if err != nil || !ok {
		t.Fatalf("record after Prepare: ok=%v err=%v", ok, err)
	}
	if rec.Workspace != "acme" {
		t.Fatalf("record workspace = %q, want the rig's mapping acme", rec.Workspace)
	}
}

func TestOpenShellPrepareRefusesAnUnreadableWorkspaceMap(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"), `{"default":""}`)
	fake := openshellPrepareFake(nil)
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)
	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
		t.Fatal("Prepare accepted a workspace map with no default")
	}
	if len(*pushes) != 0 {
		t.Fatalf("pushes = %d, want none before the workspace map was read", len(*pushes))
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("sandbox calls = %v, want none", calls)
	}
}
