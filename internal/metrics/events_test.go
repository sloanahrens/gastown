package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// regimeEnv serves one regime-events file (or the error) at the town's path and
// a town with no runtime record.
func regimeEnv(content string, readErr error) Env {
	env := envAt(fixedNow())
	env.Town = "/town"
	env.ReadFile = func(path string) ([]byte, error) {
		if path != filepath.Join("/town", "settings", "regime-events.json") {
			return nil, errors.New("read of an unexpected path: " + path)
		}
		if readErr != nil {
			return nil, readErr
		}
		return []byte(content), nil
	}
	env.ReadDir = func(dir string) ([]DirEntry, error) {
		if dir != filepath.Join("/town", ".runtime", "metrics") {
			return nil, errors.New("listing of an unexpected path: " + dir)
		}
		return nil, fs.ErrNotExist
	}
	return env
}

// regimeEventsJSON builds a schema-1 file of n one-line events.
func regimeEventsJSON(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"date":"2026-10-10","what":"change %d"}`, i)
	}
	return `{"v":1,"events":[` + strings.Join(entries, ",") + `]}`
}

// noEvents checks a result that means no event is known: empty, non-nil, and
// marshalling as [] so a panel without marks still gets an array.
func noEvents(t *testing.T, events []RegimeEvent) {
	t.Helper()
	if events == nil || len(events) != 0 {
		t.Fatalf("events = %#v; want an empty non-nil list", events)
	}
	data, err := json.Marshal(events)
	if err != nil || string(data) != "[]" {
		t.Fatalf("marshal = %s, %v; want []", data, err)
	}
}

func TestReadRegimeEvents(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 130)
	tests := []struct {
		name    string
		content string
		readErr error
		wantErr string
		check   func(*testing.T, []RegimeEvent)
	}{
		{
			name: "a valid file comes back sorted by date, then in file order",
			content: `{"v":1,"events":[
				{"date":"2026-10-20","what":"  gate raised to 3  "},
				{"date":"2026-10-01","what":"flash seats replaced by Sonnet seats","confirmed":true},
				{"date":"2026-10-20","what":"second change on the same day"}
			]}`,
			check: func(t *testing.T, events []RegimeEvent) {
				want := []RegimeEvent{
					{Date: "2026-10-01", What: "flash seats replaced by Sonnet seats", Confirmed: true},
					{Date: "2026-10-20", What: "gate raised to 3"},
					{Date: "2026-10-20", What: "second change on the same day"},
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("events = %#v; want %#v", events, want)
				}
			},
		},
		{name: "a missing file is no events and no error", readErr: fs.ErrNotExist, check: noEvents},
		{name: "an empty events array is no events", content: `{"v":1,"events":[]}`, check: noEvents},
		{
			name:    "a what over 120 runes is cut",
			content: `{"v":1,"events":[{"date":"2026-10-10","what":"` + long + `"}]}`,
			check: func(t *testing.T, events []RegimeEvent) {
				want := string([]rune(long)[:120])
				if got := events[0].What; got != want || len([]rune(got)) != 120 {
					t.Fatalf("what = %q (%d runes); want 120 runes ending at %q", got, len([]rune(got)), want)
				}
			},
		},
		{
			name:    "sixty-four events is the limit",
			content: regimeEventsJSON(64),
			check: func(t *testing.T, events []RegimeEvent) {
				if len(events) != 64 {
					t.Fatalf("64 events = %d; want 64", len(events))
				}
			},
		},
		{name: "sixty-five events", content: regimeEventsJSON(65), wantErr: "65"},
		{name: "an unreadable file", readErr: fs.ErrPermission, wantErr: "permission"},
		{name: "malformed JSON", content: `{"v":1,"events":[`, wantErr: "decoding"},
		{name: "an unknown top-level field", content: `{"v":1,"events":[],"extra":1}`, wantErr: "unknown field"},
		{name: "an unknown event field", content: `{"v":1,"events":[{"date":"2026-10-10","what":"x","source":"log"}]}`, wantErr: "unknown field"},
		{name: "another schema version", content: `{"v":2,"events":[]}`, wantErr: "schema version 2"},
		{name: "no version", content: `{"events":[]}`, wantErr: "schema version 0"},
		{name: "data after the object", content: `{"v":1,"events":[]} {"v":1}`, wantErr: "data after"},
		{name: "a day that is not on the calendar", content: `{"v":1,"events":[{"date":"2026-10-32","what":"x"}]}`, wantErr: "date"},
		{name: "a date in another form", content: `{"v":1,"events":[{"date":"10/10/2026","what":"x"}]}`, wantErr: "date"},
		{name: "a date that is missing", content: `{"v":1,"events":[{"what":"x"}]}`, wantErr: "date"},
		{name: "an empty what", content: `{"v":1,"events":[{"date":"2026-10-10","what":"   "}]}`, wantErr: "empty"},
		{name: "a what that is not a string", content: `{"v":1,"events":[{"date":"2026-10-10","what":7}]}`, wantErr: "decoding"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events, err := ReadRegimeEvents(regimeEnv(tc.content, tc.readErr))
			if tc.wantErr != "" {
				if err == nil || events != nil {
					t.Fatalf("ReadRegimeEvents = %v, %v; want an error and no events", events, err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadRegimeEvents: %v", err)
			}
			tc.check(t, events)
		})
	}
}

// runtimeRegimeEnv serves the files named in files and the listing of the
// town's runtime metrics directory. A path not in either answers as a test
// error, so a reader that reads something else fails loudly.
func runtimeRegimeEnv(files map[string]string, entries []DirEntry) Env {
	env := envAt(fixedNow())
	env.Town = "/town"
	env.ReadFile = func(path string) ([]byte, error) {
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		if strings.HasPrefix(path, filepath.Join("/town", ".runtime", "metrics")) {
			return nil, errors.New("read of an unexpected path: " + path)
		}
		return nil, fs.ErrNotExist
	}
	env.ReadDir = func(dir string) ([]DirEntry, error) {
		if dir != filepath.Join("/town", ".runtime", "metrics") {
			return nil, errors.New("listing of an unexpected path: " + dir)
		}
		return entries, nil
	}
	return env
}

// TestRegimeEventsReaderMergesTheFiles reads the settings file, the runtime
// record and a backfill file as one series: ordered by the instant each event
// happened at, with the backfill's restatement of a recorded event counted once.
func TestRegimeEventsReaderMergesTheFiles(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join("/town", ".runtime", "metrics")
	files := map[string]string{
		filepath.Join("/town", "settings", "regime-events.json"): `{"v":1,"events":[
			{"date":"2026-10-05","what":"flash seats replaced by Sonnet seats"}]}`,
		filepath.Join(runtimeDir, "regime-events.jsonl"): `{"ts":"2026-10-01T09:30:00Z","kind":"directive","before_ref":"main@a","after_ref":"main@b","commit":"aaa","reason":"first directive","actor":"overseer"}
{"ts":"2026-10-05T12:00:00Z","kind":"gate","before_ref":"main@b","after_ref":"main@c","commit":"bbb","reason":"gate raised to 3","actor":"overseer"}
`,
		filepath.Join(runtimeDir, "regime-events-backfill-20261101.jsonl"): `{"ts":"2026-09-20T08:00:00Z","kind":"install","before_ref":"","after_ref":"","commit":"","reason":"gt installed","actor":"overseer","source":"log"}
{"ts":"2026-10-05T12:00:00Z","kind":"gate","before_ref":"main@b","after_ref":"main@c","commit":"bbb","reason":"gate raised to 3","actor":"overseer","source":"log"}
`,
	}
	entries := []DirEntry{
		{Name: "regime-events.jsonl"},
		{Name: "regime-events-backfill-20261101.jsonl"},
		{Name: "bursts.jsonl"},
	}
	events, err := ReadRegimeEvents(runtimeRegimeEnv(files, entries))
	if err != nil {
		t.Fatalf("ReadRegimeEvents: %v", err)
	}
	want := []RegimeEvent{
		{Date: "2026-09-20", What: "install: gt installed", Confirmed: true},
		{Date: "2026-10-01", What: "directive: first directive", Confirmed: true},
		{Date: "2026-10-05", What: "flash seats replaced by Sonnet seats"},
		{Date: "2026-10-05", What: "gate: gate raised to 3", Confirmed: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v; want the three sources merged oldest first with the repeated record once: %#v", events, want)
	}
}

// TestRegimeEventsReaderRefusesAnUnusableRecord covers each way a runtime record
// line can be unusable: the reader reports it rather than reading the file as no
// events.
func TestRegimeEventsReaderRefusesAnUnusableRecord(t *testing.T) {
	t.Parallel()
	const line = `{"ts":"2026-10-01T09:30:00Z","kind":"gate","before_ref":"a","after_ref":"b","commit":"c","reason":"gate raised","actor":"overseer"}`
	tests := []struct {
		name    string
		record  string
		wantErr string
	}{
		{name: "a line that is not JSON", record: "{not json\n", wantErr: "decoding"},
		{name: "a line cut short", record: line + "\n" + `{"ts":"2026-10-02T`, wantErr: "decoding"},
		{name: "an unknown field", record: `{"ts":"2026-10-01T09:30:00Z","kind":"gate","before_ref":"a","after_ref":"b","commit":"c","reason":"x","actor":"overseer","extra":1}` + "\n", wantErr: "unknown field"},
		{name: "an unknown kind", record: strings.Replace(line, `"gate"`, `"seat"`, 1) + "\n", wantErr: "unknown kind"},
		{name: "an empty reason", record: strings.Replace(line, `"gate raised"`, `"  "`, 1) + "\n", wantErr: "empty reason"},
		{name: "an empty actor", record: strings.Replace(line, `"overseer"`, `""`, 1) + "\n", wantErr: "empty actor"},
		{name: "no timestamp", record: strings.Replace(line, `"ts":"2026-10-01T09:30:00Z",`, "", 1) + "\n", wantErr: "no timestamp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtimeDir := filepath.Join("/town", ".runtime", "metrics")
			files := map[string]string{filepath.Join(runtimeDir, "regime-events.jsonl"): tc.record}
			env := runtimeRegimeEnv(files, []DirEntry{{Name: "regime-events.jsonl"}})
			events, err := ReadRegimeEvents(env)
			if err == nil || len(events) != 0 {
				t.Fatalf("ReadRegimeEvents = %v, %v; want an error and no events", events, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestRegimeEventsReaderRefusesAnUnreadableRecord covers the reads around a
// record: a directory that cannot be listed, a record that cannot be read, and
// an Env with no directory seam are errors, never an empty record.
func TestRegimeEventsReaderRefusesAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	town := "/town"
	settings := func(env Env) Env {
		env.Town = town
		env.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
		return env
	}
	tests := []struct {
		name    string
		env     Env
		wantErr string
	}{
		{
			name: "a directory that cannot be listed",
			env: func() Env {
				env := settings(envAt(fixedNow()))
				env.ReadDir = func(string) ([]DirEntry, error) { return nil, fs.ErrPermission }
				return env
			}(),
			wantErr: "listing",
		},
		{
			name: "a record that cannot be read",
			env: func() Env {
				env := settings(envAt(fixedNow()))
				env.ReadDir = func(string) ([]DirEntry, error) {
					return []DirEntry{{Name: "regime-events.jsonl"}}, nil
				}
				env.ReadFile = func(path string) ([]byte, error) {
					if strings.HasSuffix(path, "regime-events.jsonl") {
						return nil, fs.ErrPermission
					}
					return nil, fs.ErrNotExist
				}
				return env
			}(),
			wantErr: "reading",
		},
		{
			name:    "an Env with no directory seam",
			env:     settings(envAt(fixedNow())),
			wantErr: "ReadDir is unset",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events, err := ReadRegimeEvents(tc.env)
			if err == nil || len(events) != 0 {
				t.Fatalf("ReadRegimeEvents = %v, %v; want an error and no events", events, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// regimeTestRecord is the record the writer tests append.
func regimeTestRecord() RegimeEventRecord {
	return RegimeEventRecord{
		TS:        time.Date(2026, 11, 1, 10, 47, 0, 0, time.UTC),
		Kind:      "gate",
		BeforeRef: "main@a",
		AfterRef:  "main@b",
		Commit:    "abc123",
		Reason:    "gate raised to 3",
		Actor:     "gastown/polecats/capable",
	}
}

// TestRecordRegimeEventAppendsOneLine writes two records through the writer and
// reads them back: one line each, the first the record it was given, in a file
// created 0600. A writer that rewrote the file instead of appending leaves one
// line and this test fails.
func TestRecordRegimeEventAppendsOneLine(t *testing.T) {
	t.Parallel()
	path := RegimeEventsPath(t.TempDir())
	first := regimeTestRecord()
	second := first
	second.TS = first.TS.Add(time.Minute)
	second.Reason = "gate lowered to 2"
	for _, rec := range []RegimeEventRecord{first, second} {
		if err := RecordRegimeEvent(path, rec); err != nil {
			t.Fatalf("RecordRegimeEvent: %v", err)
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
	var got, last RegimeEventRecord
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

// TestRecordRegimeEventRefusesAnInvalidRecord covers each record the writer
// refuses: the error names what is wrong and no file is created.
func TestRecordRegimeEventRefusesAnInvalidRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*RegimeEventRecord)
		wantErr string
	}{
		{name: "an unknown kind", mutate: func(r *RegimeEventRecord) { r.Kind = "seat" }, wantErr: "unknown kind"},
		{name: "an empty kind", mutate: func(r *RegimeEventRecord) { r.Kind = "  " }, wantErr: "unknown kind"},
		{name: "an empty reason", mutate: func(r *RegimeEventRecord) { r.Reason = "  " }, wantErr: "empty reason"},
		{name: "an empty actor", mutate: func(r *RegimeEventRecord) { r.Actor = "" }, wantErr: "empty actor"},
		{name: "a zero timestamp", mutate: func(r *RegimeEventRecord) { r.TS = time.Time{} }, wantErr: "no timestamp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := RegimeEventsPath(t.TempDir())
			rec := regimeTestRecord()
			tc.mutate(&rec)
			err := RecordRegimeEvent(path, rec)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RecordRegimeEvent = %v, want it to mention %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("a refused record left %s behind (%v)", path, statErr)
			}
		})
	}
}

// TestRegimeEventsReaderSkipsAFileRemovedAfterListing covers a record listed by
// the directory and gone by the time it is read: it has no records, which is not
// an error, the same as a file that was never there.
func TestRegimeEventsReaderSkipsAFileRemovedAfterListing(t *testing.T) {
	t.Parallel()
	env := envAt(fixedNow())
	env.Town = "/town"
	env.ReadDir = func(string) ([]DirEntry, error) {
		return []DirEntry{{Name: "regime-events.jsonl"}}, nil
	}
	env.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	events, err := ReadRegimeEvents(env)
	if err != nil {
		t.Fatalf("ReadRegimeEvents: %v", err)
	}
	noEvents(t, events)
}

// TestRecordRegimeEventRefusesAnUnwritablePath covers the two filesystem
// refusals a caller can reach: a parent that is a regular file, and a path that
// is a directory. Neither is written, and the error names the failure.
func TestRecordRegimeEventRefusesAnUnwritablePath(t *testing.T) {
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
		{name: "a parent that is a file", path: filepath.Join(file, "regime-events.jsonl"), wantErr: "creating"},
		{name: "a path that is a directory", path: dir, wantErr: "opening"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := RecordRegimeEvent(tc.path, regimeTestRecord())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RecordRegimeEvent = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// failingRegimeFile is the file the append seam tests hand the writer: its write
// and close answer whatever the case asks.
type failingRegimeFile struct {
	n        int // bytes the write claims; 0 or more than the line means all
	writeErr error
	closeErr error
	closed   bool
}

func (f *failingRegimeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.n > 0 && f.n < len(p) {
		return f.n, nil
	}
	return len(p), nil
}

func (f *failingRegimeFile) Close() error {
	f.closed = true
	return f.closeErr
}

// openingRegimeFile is an open seam that hands back f.
func openingRegimeFile(f regimeFile) func(string, int, os.FileMode) (regimeFile, error) {
	return func(string, int, os.FileMode) (regimeFile, error) { return f, nil }
}

// TestAppendRegimeEventReportsAFailedAppend drives each failure between the
// encoded line and the file: the writer reports it rather than count the record
// as written. A failed write still closes the file.
func TestAppendRegimeEventReportsAFailedAppend(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	path := filepath.Join(t.TempDir(), "regime-events.jsonl")
	tests := []struct {
		name    string
		seams   func() (regimeAppend, *failingRegimeFile)
		wantErr string
	}{
		{
			name: "an encoding failure",
			seams: func() (regimeAppend, *failingRegimeFile) {
				return regimeAppend{encode: func(any) ([]byte, error) { return nil, boom }, open: openRegimeFile}, nil
			},
			wantErr: "encoding",
		},
		{
			name: "an open failure",
			seams: func() (regimeAppend, *failingRegimeFile) {
				return regimeAppend{encode: json.Marshal, open: func(string, int, os.FileMode) (regimeFile, error) {
					return nil, boom
				}}, nil
			},
			wantErr: "opening",
		},
		{
			name: "a failed write",
			seams: func() (regimeAppend, *failingRegimeFile) {
				f := &failingRegimeFile{writeErr: boom}
				return regimeAppend{encode: json.Marshal, open: openingRegimeFile(f)}, f
			},
			wantErr: "appending",
		},
		{
			name: "a short write",
			seams: func() (regimeAppend, *failingRegimeFile) {
				f := &failingRegimeFile{n: 1}
				return regimeAppend{encode: json.Marshal, open: openingRegimeFile(f)}, f
			},
			wantErr: "short write",
		},
		{
			name: "a failed close",
			seams: func() (regimeAppend, *failingRegimeFile) {
				f := &failingRegimeFile{closeErr: boom}
				return regimeAppend{encode: json.Marshal, open: openingRegimeFile(f)}, f
			},
			wantErr: "closing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seams, file := tc.seams()
			err := appendRegimeEvent(path, regimeTestRecord(), seams)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("appendRegimeEvent = %v, want it to mention %q", err, tc.wantErr)
			}
			if file != nil && !file.closed {
				t.Error("the file was not closed after the failure")
			}
		})
	}
}

// TestRegimeEventsReaderCutsALongRecordReason requires a record's what to fit the
// panel line: the kind and reason are cut at the settings file's 120 runes.
func TestRegimeEventsReaderCutsALongRecordReason(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 130)
	rec := `{"ts":"2026-10-01T09:30:00Z","kind":"gate","before_ref":"a","after_ref":"b","commit":"c","reason":"` + long + `","actor":"overseer"}` + "\n"
	runtimeDir := filepath.Join("/town", ".runtime", "metrics")
	env := runtimeRegimeEnv(map[string]string{filepath.Join(runtimeDir, "regime-events.jsonl"): rec},
		[]DirEntry{{Name: "regime-events.jsonl"}})
	events, err := ReadRegimeEvents(env)
	if err != nil {
		t.Fatalf("ReadRegimeEvents: %v", err)
	}
	want := string([]rune("gate: " + long)[:120])
	if len(events) != 1 || events[0].What != want {
		t.Fatalf("events = %#v; want one event whose what is the first 120 runes %q", events, want)
	}
}

// The backfill files merge in name order, whatever order the directory lists
// them in: two backfill files holding the same event at one instant keep the
// earlier-named file's twin, and two different events at one instant keep the
// earlier-named file's first (gt-ng1lc).
func TestRegimeEventsReaderReadsBackfillFilesInNameOrder(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join("/town", ".runtime", "metrics")
	const at = `"ts":"2026-10-05T12:00:00Z","kind":"gate","before_ref":"","after_ref":`
	files := map[string]string{
		filepath.Join(runtimeDir, "regime-events.jsonl"):                   "",
		filepath.Join(runtimeDir, "regime-events-backfill-20261101.jsonl"): `{` + at + `"b","commit":"","reason":"from the later file","actor":"a"}` + "\n",
		filepath.Join(runtimeDir, "regime-events-backfill-20261031.jsonl"): `{` + at + `"a","commit":"","reason":"from the earlier file","actor":"a"}` + "\n",
	}
	listings := [][]DirEntry{
		{{Name: "regime-events-backfill-20261101.jsonl"}, {Name: "regime-events-backfill-20261031.jsonl"}},
		{{Name: "regime-events-backfill-20261031.jsonl"}, {Name: "regime-events-backfill-20261101.jsonl"}},
	}
	want := []RegimeEvent{
		{Date: "2026-10-05", What: "gate: from the earlier file", Confirmed: true},
		{Date: "2026-10-05", What: "gate: from the later file", Confirmed: true},
	}
	for _, entries := range listings {
		events, err := ReadRegimeEvents(runtimeRegimeEnv(files, entries))
		if err != nil {
			t.Fatalf("ReadRegimeEvents: %v", err)
		}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("listing %v: events = %#v; want %#v whatever the listing order", entries, events, want)
		}
	}
}

// A runtime line the reader cannot use is an error, and the settings events,
// which do not depend on it, still come back beside it (gt-ng1lc).
func TestRegimeEventsReaderKeepsSettingsEventsWhenTheRecordIsBad(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join("/town", ".runtime", "metrics")
	files := map[string]string{
		filepath.Join("/town", "settings", "regime-events.json"): `{"v":1,"events":[{"date":"2026-10-05","what":"flash seats replaced by Sonnet seats"}]}`,
		filepath.Join(runtimeDir, "regime-events.jsonl"):         "{not json\n",
	}
	events, err := ReadRegimeEvents(runtimeRegimeEnv(files, []DirEntry{{Name: "regime-events.jsonl"}}))
	if err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("error = %v; want the record's decoding error", err)
	}
	want := []RegimeEvent{{Date: "2026-10-05", What: "flash seats replaced by Sonnet seats"}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v; want the settings event kept: %#v", events, want)
	}
	var reports []string
	marks := panelEvents(runtimeRegimeEnv(files, []DirEntry{{Name: "regime-events.jsonl"}}), func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	})
	if len(marks) != 1 || marks[0].What != want[0].What {
		t.Fatalf("panel marks = %#v; want the settings mark", marks)
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "decoding") {
		t.Fatalf("reports = %q; want one line naming the record error", reports)
	}
}

// The writer's file is what the reader reads: records appended with
// RecordRegimeEvent to a real town layout, one in the main file and one in a
// backfill file beside it, come back through the host's directory and file
// reads, oldest first (gt-ng1lc).
func TestRegimeEventsWriterToReaderRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rec := func(ts, kind, reason string) RegimeEventRecord {
		at, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Fatal(err)
		}
		return RegimeEventRecord{TS: at, Kind: kind, Reason: reason, Actor: "tester"}
	}
	if err := RecordRegimeEvent(RegimeEventsPath(town), rec("2026-10-02T09:00:00Z", "gate", "gate raised")); err != nil {
		t.Fatal(err)
	}
	backfill := filepath.Join(town, regimeEventsRuntimeDir, "regime-events-backfill-20261101.jsonl")
	if err := RecordRegimeEvent(backfill, rec("2026-09-20T08:00:00Z", "install", "gt installed")); err != nil {
		t.Fatal(err)
	}
	events, err := ReadRegimeEvents(OSEnv(town))
	if err != nil {
		t.Fatalf("ReadRegimeEvents: %v", err)
	}
	want := []RegimeEvent{
		{Date: "2026-09-20", What: "install: gt installed", Confirmed: true},
		{Date: "2026-10-02", What: "gate: gate raised", Confirmed: true},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v; want what the writer recorded: %#v", events, want)
	}
}
