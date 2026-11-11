package metrics

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backfillSettings is a town settings document with two model seats and one
// seat whose agent names no model, which is left out.
const backfillSettings = `{
  "agents": {
    "sbx-flash": {"command": "claude", "args": ["--model", "deepseek-flash[1m]"]},
    "sbx-haiku": {"command": "claude", "args": ["--dangerously-skip-permissions", "--model=claude-haiku-5-5"]},
    "sbx-nomodel": {"command": "claude", "args": []}
  },
  "polecat_pool": {"seats": {"sbx-flash": {}, "sbx-haiku": {}, "sbx-nomodel": {}}}
}`

// readBackfillRecords reads a backfill file back as records.
func readBackfillRecords(t *testing.T, path string) []RegimeEventRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	records, err := parseRegimeRecords(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return records
}

// backfillRecordAt returns the one platform record at instant, failing when
// there is not exactly one.
func backfillRecordAt(t *testing.T, records []RegimeEventRecord, at time.Time) RegimeEventRecord {
	t.Helper()
	var found []RegimeEventRecord
	for _, rec := range records {
		if rec.Kind == "platform" && rec.TS.Equal(at) {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d platform records at %s, want 1: %#v", len(found), at.Format(time.RFC3339), records)
	}
	return found[0]
}

// The backfill names the two moves and the seat models settings put in force at
// the sandbox move, and the reader merges the file beside a record that
// restates one move as a single event.
func TestRegimeBackfillWritesTheMovesAndTheSeats(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownFile(t, filepath.Join(town, "settings", "config.json"), backfillSettings)
	env := Env{Town: town, ReadFile: os.ReadFile}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	path, n, err := RegimeBackfill(env, now, "backfill")
	if err != nil {
		t.Fatalf("RegimeBackfill: %v", err)
	}
	if want := filepath.Join(town, ".runtime", "metrics", "regime-events-backfill-20261111.jsonl"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	if n != 4 {
		t.Fatalf("wrote %d events, want the 2 moves and the 2 model seats", n)
	}
	records := readBackfillRecords(t, path)

	sandbox := backfillRecordAt(t, records, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))
	if sandbox.Kind != "platform" || sandbox.Reason != "seats moved from the host into sandboxes" {
		t.Errorf("2026-10-29 record = %#v, want the sandbox move as a platform event", sandbox)
	}
	pod := backfillRecordAt(t, records, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))
	if pod.Kind != "platform" || pod.Reason != "daemon moved from the host into the control cluster" {
		t.Errorf("2026-11-10 record = %#v, want the pod move as a platform event", pod)
	}
	seats := map[string]string{}
	for _, rec := range records {
		if rec.Kind != "seat_model" {
			continue
		}
		if !rec.TS.Equal(regimeSandboxMoveAt) {
			t.Errorf("seat event %#v is not at the sandbox move", rec)
		}
		seats[rec.Reason] = rec.AfterRef
	}
	want := map[string]string{
		"sbx-flash seat runs deepseek-flash[1m]": "deepseek-flash[1m]",
		"sbx-haiku seat runs claude-haiku-5-5":   "claude-haiku-5-5",
	}
	for reason, model := range want {
		if seats[reason] != model {
			t.Errorf("seat events = %#v, want %q -> %q", seats, reason, model)
		}
	}
	if len(seats) != len(want) {
		t.Errorf("seat events = %#v, want only the seats whose agent names a model", seats)
	}

	// The reader merges the file; a runtime record that restates one move is the
	// same event to it, and it counts once.
	duplicate := RegimeEventRecord{
		TS:       sandbox.TS,
		Kind:     sandbox.Kind,
		AfterRef: sandbox.AfterRef,
		Reason:   "restated in the record",
		Actor:    "ace",
	}
	if err := RecordRegimeEvent(RegimeEventsPath(town), duplicate); err != nil {
		t.Fatalf("appending the duplicate: %v", err)
	}
	events, err := ReadRegimeEvents(OSEnv(town))
	if err != nil {
		t.Fatalf("ReadRegimeEvents: %v", err)
	}
	sandboxMoves := 0
	for _, ev := range events {
		if ev.Date == "2026-10-29" && strings.HasPrefix(ev.What, "platform: ") {
			sandboxMoves++
		}
	}
	if sandboxMoves != 1 {
		t.Fatalf("%d sandbox-move events, want the file and the record counted once: %#v", sandboxMoves, events)
	}
}

// A town with no settings file still gets the moves: no seats are known, which
// is not a seat list the backfill refuses.
func TestRegimeBackfillWithoutSettingsWritesTheMoves(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := Env{Town: town, ReadFile: os.ReadFile}
	path, n, err := RegimeBackfill(env, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), "backfill")
	if err != nil {
		t.Fatalf("RegimeBackfill: %v", err)
	}
	if n != 2 {
		t.Fatalf("wrote %d events, want the two moves", n)
	}
	if got := readBackfillRecords(t, path); len(got) != 2 {
		t.Fatalf("%d records, want 2: %#v", len(got), got)
	}
}

// A settings file that is there and cannot be parsed fails the backfill and
// writes no file: a seat list the town cannot read is not a town with no seats.
func TestRegimeBackfillRefusesUnreadableSettings(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownFile(t, filepath.Join(town, "settings", "config.json"), `{"polecat_pool": `)
	env := Env{Town: town, ReadFile: os.ReadFile}
	path, _, err := RegimeBackfill(env, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), "backfill")
	if err == nil || !strings.Contains(err.Error(), "settings/config.json") {
		t.Fatalf("err = %v, want the unreadable settings file named", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a refused backfill left %s behind (%v)", path, statErr)
	}
}

// A backfill with no reader is a wiring error, not a town with no seats.
func TestRegimeBackfillRequiresAReader(t *testing.T) {
	t.Parallel()
	_, _, err := RegimeBackfill(Env{Town: t.TempDir()}, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), "backfill")
	if err == nil || !strings.Contains(err.Error(), "ReadFile") {
		t.Fatalf("err = %v, want the unwired-reader refusal", err)
	}
}

// A backfill file the host cannot create fails the call with the path named.
func TestRegimeBackfillReturnsTheWriteError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A regular file where the runtime directory goes makes the directory
	// creation fail.
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("in the way\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := RegimeBackfill(Env{Town: town, ReadFile: os.ReadFile}, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), "backfill")
	if err == nil || !strings.Contains(err.Error(), "creating") {
		t.Fatalf("err = %v, want the failed write named", err)
	}
}

// A backfill with no actor to record under is refused and writes no file.
func TestRegimeBackfillRefusesNoActor(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := Env{Town: town, ReadFile: os.ReadFile}
	path, _, err := RegimeBackfill(env, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), " ")
	if err == nil || !strings.Contains(err.Error(), "no actor") {
		t.Fatalf("err = %v, want the missing-actor refusal", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a refused backfill left %s behind (%v)", path, statErr)
	}
}

// A record the writer would refuse fails the whole file before it is opened, so
// a bad event never reaches the reader as a truncated history.
func TestWriteRegimeBackfillRefusesAnInvalidRecord(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "regime-events-backfill-20261111.jsonl")
	_, err := writeRegimeBackfill(path, []RegimeEventRecord{
		{TS: time.Now(), Kind: "platform", Reason: "", Actor: "ace"},
	})
	if err == nil || !strings.Contains(err.Error(), "empty reason") {
		t.Fatalf("err = %v, want the writer's refusal", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a refused write left %s behind (%v)", path, statErr)
	}
}

// Writing the same day's file twice replaces it with the same events: the
// backfill is idempotent, and the reader never sees a line twice.
func TestWriteRegimeBackfillReplacesTheSameDayFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "regime-events-backfill-20261111.jsonl")
	records := []RegimeEventRecord{
		{TS: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), Kind: "platform", Reason: "daemon moved", Actor: "backfill"},
		{TS: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Kind: "platform", Reason: "seats moved", Actor: "backfill"},
	}
	if n, err := writeRegimeBackfill(path, records); err != nil || n != 2 {
		t.Fatalf("first write = %d, %v; want 2 records", n, err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := writeRegimeBackfill(path, records); err != nil || n != 2 {
		t.Fatalf("second write = %d, %v; want 2 records", n, err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("second write changed the file:\nfirst  %q\nsecond %q", first, second)
	}
	got := readBackfillRecords(t, path)
	if len(got) != 2 || !got[0].TS.Equal(records[1].TS) {
		t.Fatalf("records = %#v, want the two moves oldest first", got)
	}
}

// The two moves are records the writer accepts as written: the backfill's own
// table never carries a field the reader rejects.
func TestRegimeBackfillMovesValidate(t *testing.T) {
	t.Parallel()
	for _, move := range regimeBackfillMoves {
		move.Actor = "backfill"
		if err := move.validate(); err != nil {
			t.Errorf("move %#v: %v", move, err)
		}
		data, err := json.Marshal(move)
		if err != nil {
			t.Fatalf("marshalling %#v: %v", move, err)
		}
		if !json.Valid(data) {
			t.Fatalf("move %q is not valid JSON", move.Reason)
		}
	}
}

// backfillFakeFile is one open file a test decides the result of: the count
// Write returns (-1 is len(p)), and the errors either call gives.
type backfillFakeFile struct {
	written  int
	writeErr error
	closeErr error
}

func (f *backfillFakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.written < 0 {
		return len(p), nil
	}
	return f.written, nil
}

func (f *backfillFakeFile) Close() error { return f.closeErr }

// Every failure a host cannot be made to produce is an error naming the step:
// the encoder, the open, the write, a short write and the close.
func TestWriteRegimeBackfillDrivesItsFailures(t *testing.T) {
	t.Parallel()
	records := []RegimeEventRecord{{
		TS: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Kind: "platform", Reason: "moved", Actor: "backfill",
	}}
	file := func(f *backfillFakeFile) backfillWrite {
		return backfillWrite{
			encode: json.Marshal,
			open:   func(string, int, os.FileMode) (backfillFile, error) { return f, nil },
		}
	}
	tests := []struct {
		name  string
		seams backfillWrite
		want  string
	}{
		{
			name: "the encoder fails",
			seams: backfillWrite{
				encode: func(any) ([]byte, error) { return nil, errors.New("encode failed") },
				open:   func(string, int, os.FileMode) (backfillFile, error) { return &backfillFakeFile{written: -1}, nil },
			},
			want: "encoding",
		},
		{
			name: "the file cannot be opened",
			seams: backfillWrite{
				encode: json.Marshal,
				open:   func(string, int, os.FileMode) (backfillFile, error) { return nil, errors.New("permission denied") },
			},
			want: "opening",
		},
		{name: "the write fails", seams: file(&backfillFakeFile{writeErr: errors.New("disk full")}), want: "writing"},
		{name: "the write is short", seams: file(&backfillFakeFile{written: 1}), want: "short write"},
		{name: "the close fails", seams: file(&backfillFakeFile{written: -1, closeErr: errors.New("flush failed")}), want: "closing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "regime-events-backfill-20261111.jsonl")
			_, err := writeRegimeBackfillWith(path, records, tc.seams)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// A directory the host cannot make is an error naming the creation.
func TestWriteRegimeBackfillRefusesADirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("in the way\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(town, ".runtime", "metrics", "regime-events-backfill-20261111.jsonl")
	_, err := writeRegimeBackfill(path, []RegimeEventRecord{{
		TS: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Kind: "platform", Reason: "moved", Actor: "backfill",
	}})
	if err == nil || !strings.Contains(err.Error(), "creating") {
		t.Fatalf("err = %v, want the failed directory creation named", err)
	}
}

// A path that is already a directory cannot be opened for writing.
func TestWriteRegimeBackfillRefusesAFileItCannotOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "regime-events-backfill-20261111.jsonl")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := writeRegimeBackfill(path, []RegimeEventRecord{{
		TS: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Kind: "platform", Reason: "moved", Actor: "backfill",
	}})
	if err == nil || !strings.Contains(err.Error(), "opening") {
		t.Fatalf("err = %v, want the failed open named", err)
	}
}
