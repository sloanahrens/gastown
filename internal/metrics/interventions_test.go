package metrics

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// interventionTestNow is the instant the reader counts at in these tests.
var interventionTestNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// interventionTestRecord is the record the writer tests append.
func interventionTestRecord() InterventionRecord {
	return InterventionRecord{
		TS:     time.Date(2026, 11, 1, 10, 47, 0, 0, time.UTC),
		Kind:   "takeover",
		Bead:   "gt-abc",
		Rig:    "gastown",
		Reason: "the polecat stalled on the gate",
		Actor:  "gastown/crew/dipper",
	}
}

// TestRecordInterventionAppendsOneLine writes two records through the writer and
// reads them back: one line each, the line the record it was given, in a file
// created 0600. A writer that rewrote the file instead of appending leaves one
// line and this test fails.
func TestRecordInterventionAppendsOneLine(t *testing.T) {
	t.Parallel()
	path := InterventionsPath(t.TempDir())
	first := interventionTestRecord()
	second := first
	second.TS = first.TS.Add(time.Minute)
	second.Reason = "the polecat was restarted"
	for _, rec := range []InterventionRecord{first, second} {
		if err := RecordIntervention(path, rec); err != nil {
			t.Fatalf("RecordIntervention: %v", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), data)
	}
	var got, last InterventionRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("decoding the first line: %v", err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Errorf("first line = %#v; want %#v", got, first)
	}
	if err := json.Unmarshal([]byte(lines[1]), &last); err != nil {
		t.Fatalf("decoding the second line: %v", err)
	}
	if !reflect.DeepEqual(last, second) {
		t.Errorf("second line = %#v; want %#v", last, second)
	}
}

// TestRecordInterventionRefusesAnInvalidRecord covers each record the writer
// refuses: the error names what is wrong and no file is created.
func TestRecordInterventionRefusesAnInvalidRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*InterventionRecord)
		wantErr string
	}{
		{name: "an unknown kind", mutate: func(r *InterventionRecord) { r.Kind = "seat" }, wantErr: "unknown kind"},
		{name: "an empty kind", mutate: func(r *InterventionRecord) { r.Kind = "  " }, wantErr: "unknown kind"},
		{name: "an empty reason", mutate: func(r *InterventionRecord) { r.Reason = "  " }, wantErr: "empty reason"},
		{name: "an empty actor", mutate: func(r *InterventionRecord) { r.Actor = "" }, wantErr: "empty actor"},
		{name: "a zero timestamp", mutate: func(r *InterventionRecord) { r.TS = time.Time{} }, wantErr: "no timestamp"},
		{name: "a takeover with no bead", mutate: func(r *InterventionRecord) { r.Bead = " " }, wantErr: "needs a bead"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := InterventionsPath(t.TempDir())
			rec := interventionTestRecord()
			tc.mutate(&rec)
			err := RecordIntervention(path, rec)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RecordIntervention = %v, want it to mention %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("a refused record left %s behind (%v)", path, statErr)
			}
		})
	}
}

// TestRecordInterventionAcceptsABeadlessHold records the two kinds that need no
// bead, so the writer is not merely permissive by accident: a hold and a restart
// with no bead are written, with an empty bead and rig.
func TestRecordInterventionAcceptsABeadlessHold(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"hold", "restart"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			path := InterventionsPath(t.TempDir())
			rec := interventionTestRecord()
			rec.Kind, rec.Bead, rec.Rig = kind, "", ""
			if err := RecordIntervention(path, rec); err != nil {
				t.Fatalf("RecordIntervention: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			var got InterventionRecord
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decoding the line: %v", err)
			}
			if got.Kind != kind || got.Bead != "" || got.Rig != "" {
				t.Errorf("line = %#v; want a %s with no bead and no rig", got, kind)
			}
		})
	}
}

// TestRecordInterventionRefusesAnUnwritablePath covers the two filesystem
// refusals a caller can reach: a parent that is a regular file, and a path that
// is a directory. Neither is written, and the error names the failure.
func TestRecordInterventionRefusesAnUnwritablePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "a parent that is a file", path: filepath.Join(file, "interventions.jsonl"), wantErr: "creating"},
		{name: "a path that is a directory", path: dir, wantErr: "opening"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := RecordIntervention(tc.path, interventionTestRecord())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RecordIntervention = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// failingInterventionFile is the file the append seam tests hand the writer: its
// write and close answer whatever the case asks.
type failingInterventionFile struct {
	n        int // bytes the write claims; 0 or more than the line means all
	writeErr error
	closeErr error
	closed   bool
}

func (f *failingInterventionFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.n > 0 && f.n < len(p) {
		return f.n, nil
	}
	return len(p), nil
}

func (f *failingInterventionFile) Close() error {
	f.closed = true
	return f.closeErr
}

// openingInterventionFile is an open seam that hands back f.
func openingInterventionFile(f interventionFile) func(string, int, os.FileMode) (interventionFile, error) {
	return func(string, int, os.FileMode) (interventionFile, error) { return f, nil }
}

// TestAppendInterventionReportsAFailedAppend drives each failure between the
// encoded line and the file: the writer reports it rather than count the record
// as written. A failed write still closes the file.
func TestAppendInterventionReportsAFailedAppend(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	path := filepath.Join(t.TempDir(), "interventions.jsonl")
	tests := []struct {
		name    string
		seams   func() (interventionAppend, *failingInterventionFile)
		wantErr string
	}{
		{
			name: "an encoding failure",
			seams: func() (interventionAppend, *failingInterventionFile) {
				return interventionAppend{encode: func(any) ([]byte, error) { return nil, boom }, open: openInterventionFile}, nil
			},
			wantErr: "encoding",
		},
		{
			name: "an open failure",
			seams: func() (interventionAppend, *failingInterventionFile) {
				return interventionAppend{encode: json.Marshal, open: func(string, int, os.FileMode) (interventionFile, error) {
					return nil, boom
				}}, nil
			},
			wantErr: "opening",
		},
		{
			name: "a failed write",
			seams: func() (interventionAppend, *failingInterventionFile) {
				f := &failingInterventionFile{writeErr: boom}
				return interventionAppend{encode: json.Marshal, open: openingInterventionFile(f)}, f
			},
			wantErr: "appending",
		},
		{
			name: "a short write",
			seams: func() (interventionAppend, *failingInterventionFile) {
				f := &failingInterventionFile{n: 1}
				return interventionAppend{encode: json.Marshal, open: openingInterventionFile(f)}, f
			},
			wantErr: "short write",
		},
		{
			name: "a failed close",
			seams: func() (interventionAppend, *failingInterventionFile) {
				f := &failingInterventionFile{closeErr: boom}
				return interventionAppend{encode: json.Marshal, open: openingInterventionFile(f)}, f
			},
			wantErr: "closing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seams, file := tc.seams()
			err := appendIntervention(path, interventionTestRecord(), seams)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("appendIntervention = %v, want it to mention %q", err, tc.wantErr)
			}
			if file != nil && !file.closed {
				t.Error("the file was not closed after the failure")
			}
		})
	}
}

// TestCountInterventionsPerWeekByKind counts a record with lines from two weeks,
// three rigs and three kinds, and requires each (week, rig, kind) count, the
// future line left out, and every unreadable line counted rather than read as an
// intervention.
func TestCountInterventionsPerWeekByKind(t *testing.T) {
	t.Parallel()
	line := func(ts time.Time, kind, bead, rig string) string {
		data, err := json.Marshal(InterventionRecord{
			TS: ts, Kind: kind, Bead: bead, Rig: rig, Reason: "needed a person", Actor: "gastown/crew/dipper",
		})
		if err != nil {
			t.Fatalf("marshalling a line: %v", err)
		}
		return string(data)
	}
	lines := []string{
		line(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), "hold", "", "gastown"),
		line(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), "hold", "", "gastown"),
		line(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC), "waiver", "gt-abc", "gastown"),
		line(time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC), "hold", "", "beads"),
		line(time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC), "hold", "", "gastown"),
		// After now: not an intervention yet.
		line(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC), "hold", "", "gastown"),
		// Unreadable: not JSON, no actor, no bead on a takeover, unknown kind.
		"not json",
		`{"ts":"2026-11-02T09:00:00Z","kind":"hold","reason":"no actor"}`,
		`{"ts":"2026-11-02T09:00:00Z","kind":"takeover","reason":"no bead","actor":"overseer"}`,
		`{"ts":"2026-11-02T09:00:00Z","kind":"seat","bead":"gt-abc","reason":"unknown kind","actor":"overseer"}`,
		"",
	}
	got := CountInterventions(lines, interventionTestNow)
	want := []InterventionCount{
		{Week: "2026-10-26", Rig: "gastown", Kind: "hold", Count: 1},
		{Week: "2026-11-02", Rig: "beads", Kind: "hold", Count: 1},
		{Week: "2026-11-02", Rig: "gastown", Kind: "hold", Count: 2},
		{Week: "2026-11-02", Rig: "gastown", Kind: "waiver", Count: 1},
	}
	if !reflect.DeepEqual(got.Counts, want) {
		t.Errorf("counts = %#v; want %#v", got.Counts, want)
	}
	if got.Unparseable != 4 {
		t.Errorf("unparseable = %d, want 4", got.Unparseable)
	}
}

// TestCountInterventionsOnAnEmptyRecord requires no counts and no unparseable
// line from an empty record, not an error or a phantom zero row.
func TestCountInterventionsOnAnEmptyRecord(t *testing.T) {
	t.Parallel()
	got := CountInterventions(nil, interventionTestNow)
	if len(got.Counts) != 0 || got.Unparseable != 0 {
		t.Fatalf("report = %#v; want no counts and no unparseable lines", got)
	}
}
