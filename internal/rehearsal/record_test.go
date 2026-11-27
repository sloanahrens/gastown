package rehearsal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func fullRecord() Record {
	return Record{
		RunID: "run-1", Tag: "tag", ChangeSet: "cs", Start: time.Unix(1795729500, 0).UTC(), End: time.Unix(1795730400, 0).UTC(),
		Stopped: true, StopReason: "stop", CacheBytes: 1, Interventions: []string{"i"},
		Slings: []Sling{{Bead: "b", Seconds: 1, Outcome: SlingOK}}, Gates: []Gate{{Bead: "b", Seconds: 2, Verdict: GatePass}},
		Landings: []Landing{{Bead: "b", Landed: true, CIFailed: true}}, FailedCalls: []string{"f"}, Stranded: []string{"s"},
		SideRuns: []SideRun{{Seconds: 3, ExitCode: 1}}, Load: Load{Samples: 4, Max1: 5, P90One: 6}, SweepRan: true,
	}
}

func TestRehearsalRecordSaveRead(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "record.json")
	want := fullRecord()
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got, err := Read(path); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %+v, %v; want %+v", got, err, want)
	}
}

func TestRehearsalRecordReadRefuses(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		write      bool
	}{
		{name: "missing file"},
		{name: "bad JSON", body: `{"RunID":`, write: true},
		{name: "unknown field", body: `{"RunID":"r","Bogus":1}`, write: true},
		{name: "trailing object", body: `{"RunID":"r"} {}`, write: true},
		{name: "trailing garbage", body: `{"RunID":"r"}]`, write: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "record.json")
			if tt.write {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}
			if got, err := Read(path); err == nil || !reflect.DeepEqual(got, Record{}) {
				t.Fatalf("Read(%q) = %+v, %v; want an error and no record", tt.body, got, err)
			}
		})
	}
}

func TestRehearsalRecordSaveFailureKeepsPath(t *testing.T) {
	t.Parallel()
	// A non-empty directory at the path makes the final rename fail for root too,
	// where a read-only mode would not: rename cannot replace a directory with a file.
	parent := t.TempDir()
	path := filepath.Join(parent, "record.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	keep := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(keep, []byte("original"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := Save(path, fullRecord()); err == nil {
		t.Fatal("Save replaced a non-empty directory")
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "original" {
		t.Fatalf("after failed Save: %q, %v; want the original contents", got, err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 {
		t.Fatalf("after failed Save: parent has %d entries, %v; want only the directory (no temp file left)", len(entries), err)
	}
}
