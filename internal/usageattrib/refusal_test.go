package usageattrib

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// These tests take each failure path a scan, a record write or a read can take, so every
// refusal the code returns is driven by a test that asserts it.

func usageDirOf(town string) string {
	return filepath.Join(town, ".runtime", "usage")
}

func writeRaw(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirRaw(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestUsageAttribScanRefusals drives each way a scan refuses to finish. The scan must return an
// error; where it had already read a bead's record, that record must still be the one on disk.
func TestUsageAttribScanRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, town string, f *fixture)
	}{
		{"an event timestamp that does not parse", func(_ *testing.T, _ string, f *fixture) {
			f.evs = []events.Event{ev("yesterday", events.TypeSling, "", map[string]interface{}{"bead": "b-1", "target": "gastown/polecats/nux"})}
		}},
		{"an unreadable record already on disk is not replaced", func(t *testing.T, town string, _ *fixture) {
			writeRaw(t, WorkRecordPath(usageDirOf(town), "b-1"), "{not a record")
		}},
		{"a record write that fails", func(_ *testing.T, _ string, f *fixture) {
			f.replace = func(string, dashboard.UsageRecord, []string, string) error {
				return errors.New("disk full")
			}
		}},
		{"a usage directory that is a file", func(t *testing.T, town string, f *fixture) {
			*f = fixture{}
			writeRaw(t, usageDirOf(town), "not a directory")
		}},
		{"an unmatched file that cannot be replaced", func(t *testing.T, town string, _ *fixture) {
			mkdirRaw(t, filepath.Join(usageDirOf(town), UnmatchedFile))
		}},
		{"a state file that cannot be replaced", func(t *testing.T, town string, _ *fixture) {
			mkdirRaw(t, filepath.Join(usageDirOf(town), StateFile))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			f := twoBeads()
			tt.setup(t, town, &f)
			if _, err := Scan(ScanOptions{Town: town}, f.deps()); err == nil {
				t.Fatal("Scan succeeded, want a refusal")
			}
		})
	}
}

// TestUsageAttribScanKeepsUnreadableRecord checks the case the refusal exists for: a record that
// cannot be read is left exactly as it was, not replaced by what the scan built.
func TestUsageAttribScanKeepsUnreadableRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := WorkRecordPath(usageDirOf(town), "b-1")
	writeRaw(t, path, "{not a record")
	f := twoBeads()
	if _, err := Scan(ScanOptions{Town: town}, f.deps()); err == nil {
		t.Fatal("Scan succeeded over an unreadable record")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{not a record" {
		t.Errorf("the unreadable record was rewritten: %q", b)
	}
}

func TestUsageAttribPlanBeadsRefusesBeadWithNoPrefix(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	m, err := workspaceid.Load(town)
	if err != nil {
		t.Fatal(err)
	}
	runs := []Run{{Bead: "-x", Rig: "gastown", Seat: "nux", Start: t0, End: t0.Add(time.Hour), Ended: true}}
	_, err = planBeads(runs, nil, nil, nil, []int{0}, [][]string{nil}, map[string]int{}, m, usageDirOf(town), now, false)
	if err == nil {
		t.Fatal("planBeads planned a bead with no workspace prefix")
	}
}

// TestUsageAttribWriteRefusals drives the failure paths of the record writer, the reads and the
// files a scan keeps beside the records.
func TestUsageAttribWriteRefusals(t *testing.T) {
	t.Parallel()
	goodRec := func() dashboard.UsageRecord {
		return dashboard.UsageRecord{
			Bead: "b-1", Kind: dashboard.UsageKindWork, Class: dashboard.UsageClassPolecat,
			Rig: "gastown", Workspace: "customer-zero", Complete: true, Start: t0, End: t0.Add(time.Hour),
		}
	}
	tests := []struct {
		name string
		run  func(t *testing.T, dir string) error
	}{
		{"a record with no workspace", func(_ *testing.T, dir string) error {
			rec := goodRec()
			rec.Workspace = ""
			return Replace(dir, rec, []string{"s1"}, "")
		}},
		{"a session with no id", func(_ *testing.T, dir string) error {
			return Replace(dir, goodRec(), []string{""}, "")
		}},
		{"a record whose time cannot be encoded", func(_ *testing.T, dir string) error {
			rec := goodRec()
			rec.Start = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			return Replace(dir, rec, []string{"s1"}, "")
		}},
		{"a beads directory that is a file", func(t *testing.T, dir string) error {
			writeRaw(t, filepath.Join(dir, "beads"), "not a directory")
			return Replace(dir, goodRec(), []string{"s1"}, "")
		}},
		{"a bead id with a path in it", func(_ *testing.T, dir string) error {
			_, err := ReadWork(dir, "../b-1")
			return err
		}},
		{"a record file that is not a record", func(t *testing.T, dir string) error {
			writeRaw(t, WorkRecordPath(dir, "b-1"), "{not a record")
			_, err := ReadWork(dir, "b-1")
			return err
		}},
		{"a transcript with no session id", func(_ *testing.T, dir string) error {
			_, _, err := ReadSession(filepath.Join(dir, ".jsonl"))
			return err
		}},
		{"a transcript that does not exist", func(_ *testing.T, dir string) error {
			_, _, err := ReadSession(filepath.Join(dir, "missing.jsonl"))
			return err
		}},
		{"a transcript path that is a directory", func(t *testing.T, dir string) error {
			mkdirRaw(t, filepath.Join(dir, "sess-3.jsonl"))
			_, _, err := ReadSession(filepath.Join(dir, "sess-3.jsonl"))
			return err
		}},
		{"a landings path that is a file", func(t *testing.T, dir string) error {
			writeRaw(t, filepath.Join(dir, ".runtime", "landings"), "not a directory")
			_, err := ReadLandings(dir)
			return err
		}},
		{"a landing file that is a dangling link", func(t *testing.T, dir string) error {
			landings := filepath.Join(dir, ".runtime", "landings")
			mkdirRaw(t, landings)
			if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(landings, "gastown.jsonl")); err != nil {
				t.Fatal(err)
			}
			_, err := ReadLandings(dir)
			return err
		}},
		{"an unmatched line whose time cannot be encoded", func(_ *testing.T, dir string) error {
			return writeUnmatched(dir, []unmatchedLine{{Session: "s1", LastWrite: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}})
		}},
		{"a state whose time cannot be encoded", func(_ *testing.T, dir string) error {
			return writeState(dir, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), ScanOptions{}, ScanResult{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := tt.run(t, dir); err == nil {
				t.Fatal("got no error, want a refusal")
			}
		})
	}
}
