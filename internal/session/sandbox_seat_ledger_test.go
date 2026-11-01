package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// A removal that fails writes no seat-end line, so the retry that removes the
// record lines the seat once. The line used to be written before the unlink: a
// failed removal put a line in the ledger, and the retry put a second one in
// (gt-pe98l).
func TestRemoveRetriedWritesOneSeatLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rec := SandboxRecord{Session: "gt-gastown-box", Rig: "gastown", Polecat: "box"}
	if err := WriteSandboxRecord(town, rec); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	locked := errors.New("registry locked")
	calls := 0
	remove := func(path string) error {
		calls++
		if calls == 1 {
			return locked
		}
		return os.Remove(path)
	}
	if err := removeSandboxRecordWith(town, rec.Session, time.Now, remove); !errors.Is(err, locked) {
		t.Fatalf("first removal = %v, want the unlink's failure", err)
	}
	if _, err := os.Stat(SandboxSeatLedgerPath(town)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed removal wrote its seat-end line; the retry would write it twice (stat %v)", err)
	}
	if err := removeSandboxRecordWith(town, rec.Session, time.Now, remove); err != nil {
		t.Fatalf("retried removal: %v", err)
	}
	if lines := readSeatLines(t, town); len(lines) != 1 {
		t.Fatalf("seat-end lines = %d, want 1: %+v", len(lines), lines)
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
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening a pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
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

// The record's workspace comes from the town's map through the rig's beads
// prefix: the map is keyed by prefix, not by rig name, so a map that names the
// prefix is not missed because the rig is called something else. A town with no
// registry takes the default prefix, which is what the map names then.
func TestOpenShellPrepareRecordsTheRigsWorkspace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		rigsJSON  string // "" leaves the town with no rig registry
		workspace string
		want      string
	}{
		{
			name:      "the map is keyed by the rig's beads prefix",
			rigsJSON:  `{"version":1,"rigs":{"` + openshellTestRig + `":{"git_url":"http://host.docker.internal:3000/sloan/gastown.git","beads":{"repo":"","prefix":"gst-"}}}}`,
			workspace: `{"default":"customer-zero","rigs":{"gst":"acme"}}`,
			want:      "acme",
		},
		{
			name:      "a town with no rig registry takes the default prefix",
			workspace: `{"default":"customer-zero","rigs":{"gt":"acme"}}`,
			want:      "acme",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			if c.rigsJSON != "" {
				writeOpenshellFile(t, filepath.Join(town, "mayor", "rigs.json"), c.rigsJSON)
			}
			writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"), c.workspace)
			fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
			b, _ := openshellTestBackend(town, openshellTestProfile, fake)
			if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			rec, ok, err := ReadSandboxRecord(town, openshellTestSession)
			if err != nil || !ok {
				t.Fatalf("record after Prepare: ok=%v err=%v", ok, err)
			}
			if rec.Workspace != c.want {
				t.Fatalf("record workspace = %q, want the prefix's mapping %q", rec.Workspace, c.want)
			}
		})
	}
}

// A seat-end line that cannot be written does not turn a completed rollback
// into a failed one: the sandbox and its record are gone, the loss is warned
// like a failed token revoke, and the spawn's own failure is what returns
// (gt-pe98l).
func TestOpenShellPrepareRollbackWarnsWhenTheSeatLineCannotBeWritten(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	// A record left by an earlier partial write for this session.
	if err := WriteSandboxRecord(town, SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat, Profile: "polecat-default"}); err != nil {
		t.Fatal(err)
	}
	// A directory where the ledger file goes: the line cannot be appended.
	if err := os.MkdirAll(SandboxSeatLedgerPath(town), 0o755); err != nil {
		t.Fatalf("blocking the ledger: %v", err)
	}
	// The sandbox never becomes Ready, so Prepare rolls back.
	fake := openshellPrepareFake(nil)
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil {
		t.Fatal("Prepare succeeded with a sandbox that never became Ready")
	}
	if strings.Contains(err.Error(), "rolling back sandbox") {
		t.Errorf("Prepare error = %v, want the spawn's failure without a rollback failure", err)
	}
	if _, ok, readErr := ReadSandboxRecord(town, openshellTestSession); ok || readErr != nil {
		t.Fatalf("record after the rollback: ok=%v err=%v, want gone", ok, readErr)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "removing its record while rolling back") {
		t.Errorf("warnings = %q, want the unwritten seat-end line named", warnings)
	}
}

// A town whose workspace map, or whose rig registry, cannot be read, or whose
// registry does not name the rig, refuses the spawn before anything is pushed or
// created: the record would carry an empty or guessed workspace id (plan row M5).
func TestOpenShellPrepareRefusesAnUnreadableWorkspaceMap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		prepare func(t *testing.T, town string)
	}{
		{
			name: "a workspace map with no default",
			prepare: func(t *testing.T, town string) {
				writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"), `{"default":""}`)
			},
		},
		{
			name: "a rig registry that cannot be read",
			prepare: func(t *testing.T, town string) {
				writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"), `{"default":"customer-zero"}`)
				writeOpenshellFile(t, filepath.Join(town, "mayor", "rigs.json"), "{not json")
			},
		},
		{
			name: "a rig registry with no entry for the rig",
			prepare: func(t *testing.T, town string) {
				writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"), `{"default":"customer-zero","rigs":{"gt":"acme"}}`)
				writeOpenshellFile(t, filepath.Join(town, "mayor", "rigs.json"), `{"version":1,"rigs":{"other":{"git_url":"http://host.docker.internal:3000/sloan/other.git","beads":{"repo":"","prefix":"ot-"}}}}`)
			},
		},
		{
			name: "a rig entry with no beads prefix",
			prepare: func(t *testing.T, town string) {
				writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"), `{"default":"customer-zero","rigs":{"gt":"acme"}}`)
				writeOpenshellFile(t, filepath.Join(town, "mayor", "rigs.json"), `{"version":1,"rigs":{"`+openshellTestRig+`":{"git_url":"http://host.docker.internal:3000/sloan/gastown.git","beads":{"repo":""}}}}`)
			},
		},
		{
			name: "a rig entry with no beads config",
			prepare: func(t *testing.T, town string) {
				writeOpenshellFile(t, filepath.Join(town, "settings", "workspaces.json"), `{"default":"customer-zero","rigs":{"gt":"acme"}}`)
				writeOpenshellFile(t, filepath.Join(town, "mayor", "rigs.json"), `{"version":1,"rigs":{"`+openshellTestRig+`":{"git_url":"http://host.docker.internal:3000/sloan/gastown.git"}}}`)
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			c.prepare(t, town)
			fake := openshellPrepareFake(nil)
			b, pushes := openshellTestBackend(town, openshellTestProfile, fake)
			if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
				t.Fatal("Prepare accepted a town whose workspace id cannot be resolved")
			}
			if len(*pushes) != 0 {
				t.Fatalf("pushes = %d, want none before the workspace was resolved", len(*pushes))
			}
			if calls := fake.Calls(); len(calls) != 0 {
				t.Fatalf("sandbox calls = %v, want none", calls)
			}
		})
	}
}
