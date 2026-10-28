package diskmaint

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLedgerAppendAndTail(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	for i := 0; i < 5; i++ {
		if err := l.Append(LedgerEntry{TS: time.Unix(int64(i), 0).UTC(), Target: "registry", Mode: "dry-run", Planned: i}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Tail(3)
	if err != nil || len(got) != 3 || got[0].Planned != 2 || got[2].Planned != 4 {
		t.Fatalf("tail = %+v, %v; want entries 2..4 in order", got, err)
	}
}

func TestLedgerRotatesAtMaxBytes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	entry := LedgerEntry{TS: time.Unix(0, 0).UTC(), Target: "registry", Mode: "apply"}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	// Three lines fit exactly, so the fourth append is the one that rotates.
	maxBytes := int64(len(line)+1) * 3
	l := NewLedger(path, maxBytes)

	for i := 0; i < 5; i++ {
		e := entry
		e.Planned = i
		if err := l.Append(e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= maxBytes {
		t.Errorf("live file is %d bytes, want under %d", info.Size(), maxBytes)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}

	got, err := l.Tail(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("tail across the rotated and live files = %d entries, want 5: %+v", len(got), got)
	}
	for i, e := range got {
		if e.Planned != i {
			t.Errorf("tail[%d].Planned = %d, want %d", i, e.Planned, i)
		}
	}
}

// The file is read by other tools, so its JSON keys are a contract of their
// own: present when set, absent when empty.
func TestLedgerWritesJSONLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path, 1<<20)
	entry := LedgerEntry{TS: time.Unix(7, 0).UTC(), Target: "registry", Mode: "apply", Planned: 2, Done: 1, BytesFreed: 4096}
	if err := l.Append(entry); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(bytes.Split(bytes.TrimSpace(data), []byte("\n"))); n != 1 {
		t.Fatalf("append wrote %d lines, want 1", n)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("ledger line is not one JSON object: %v", err)
	}
	for _, key := range []string{"ts", "target", "mode", "planned", "done", "bytes_freed"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("ledger line has no %q key: %s", key, data)
		}
	}
	for _, key := range []string{"refused", "deferred", "errors"} {
		if _, ok := raw[key]; ok {
			t.Errorf("empty %q must be omitted: %s", key, data)
		}
	}
}

func TestLedgerTailOfMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	got, err := l.Tail(3)
	if err != nil || got != nil {
		t.Fatalf("tail of a missing file = %v, %v; want nil, nil", got, err)
	}
}

func TestLedgerSkipsTornLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path, 1<<20)
	for i := 0; i < 2; i++ {
		if err := l.Append(LedgerEntry{TS: time.Unix(int64(i), 0).UTC(), Target: "registry", Mode: "dry-run", Planned: i}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":"1970-01-01T00:00:02Z","target":"regis`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := l.Tail(10)
	if err != nil {
		t.Fatalf("a torn last line must not error: %v", err)
	}
	if len(got) != 2 || got[0].Planned != 0 || got[1].Planned != 1 {
		t.Fatalf("tail = %+v, want the two complete entries", got)
	}
	if n := l.Skipped(); n != 1 {
		t.Errorf("Skipped() = %d, want the 1 torn line counted", n)
	}
}

// A run that appends after a crash must not glue its record onto the torn text:
// both the old lines and the new one stay readable.
func TestLedgerAppendAfterTornLineKeepsBoth(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path, 1<<20)
	for i := 0; i < 2; i++ {
		if err := l.Append(LedgerEntry{TS: time.Unix(int64(i), 0).UTC(), Target: "registry", Mode: "dry-run", Planned: i}); err != nil {
			t.Fatal(err)
		}
	}
	appendRaw(t, path, `{"ts":"1970-01-01T00:00:02Z","target":"regis`)
	if err := l.Append(LedgerEntry{TS: time.Unix(3, 0).UTC(), Target: "registry", Mode: "dry-run", Planned: 2}); err != nil {
		t.Fatal(err)
	}

	got, err := l.Tail(10)
	if err != nil {
		t.Fatalf("tail after appending past a torn line: %v", err)
	}
	if len(got) != 3 || got[0].Planned != 0 || got[1].Planned != 1 || got[2].Planned != 2 {
		t.Fatalf("tail = %+v, want entries 0..2 with the new one last", got)
	}
	if n := l.Skipped(); n != 1 {
		t.Errorf("Skipped() = %d, want only the torn fragment counted", n)
	}
}

// A record whose trailing newline never landed is intact JSON; the next append
// starts its own line and the old record becomes readable again.
func TestLedgerAppendAfterMissingNewlineKeepsThatEntry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path, 1<<20)
	complete, err := json.Marshal(LedgerEntry{TS: time.Unix(0, 0).UTC(), Target: "registry", Mode: "dry-run", Planned: 7})
	if err != nil {
		t.Fatal(err)
	}
	appendRaw(t, path, string(complete))
	if err := l.Append(LedgerEntry{TS: time.Unix(1, 0).UTC(), Target: "registry", Mode: "apply", Planned: 8}); err != nil {
		t.Fatal(err)
	}

	got, err := l.Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Planned != 7 || got[1].Planned != 8 {
		t.Fatalf("tail = %+v, want both records read in order", got)
	}
	if n := l.Skipped(); n != 0 {
		t.Errorf("Skipped() = %d, want none: the entry was whole", n)
	}
}

// appendRaw writes bytes straight to the file, the way a writer that died
// mid-line leaves them.
func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A line that does not decode is counted, so a damaged ledger is not mistaken
// for a short one.
func TestLedgerTailCountsSkippedLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	for i := 0; i < 2; i++ {
		body, err := json.Marshal(LedgerEntry{TS: time.Unix(int64(i), 0).UTC(), Target: "registry", Planned: i})
		if err != nil {
			t.Fatal(err)
		}
		appendRaw(t, path, string(body)+"\n")
	}
	appendRaw(t, path, "{not json}\n")

	l := NewLedger(path, 1<<20)
	got, err := l.Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Planned != 0 || got[1].Planned != 1 {
		t.Fatalf("tail = %+v, want the two decodable entries", got)
	}
	if n := l.Skipped(); n != 1 {
		t.Errorf("Skipped() = %d, want the undecodable line counted", n)
	}
}

// Append and Tail take the ledger lock, and the write lands while it is held:
// that is what keeps a rotation from racing an append.
func TestLedgerAppendWritesWhileHoldingTheLock(t *testing.T) {
	t.Parallel()
	var held bool
	var locked string
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	l.acquire = func(path string) (func(), error) {
		locked, held = path, true
		return func() { held = false }, nil
	}
	l.write = func(f *os.File, b []byte) (int, error) {
		if !held {
			t.Error("the write ran outside the ledger lock")
		}
		return f.Write(b)
	}

	if err := l.Append(LedgerEntry{Target: "registry", Mode: "dry-run"}); err != nil {
		t.Fatal(err)
	}
	if want := l.path + ".lock"; locked != want {
		t.Errorf("Append locked %q, want %q", locked, want)
	}
	if held {
		t.Error("Append returned without releasing the lock")
	}
}

func TestLedgerTailReadsUnderTheLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path, 1<<20)
	if err := l.Append(LedgerEntry{Target: "registry", Planned: 1}); err != nil {
		t.Fatal(err)
	}
	var held bool
	var locked string
	l.acquire = func(p string) (func(), error) {
		locked, held = p, true
		return func() { held = false }, nil
	}

	got, err := l.Tail(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Planned != 1 {
		t.Fatalf("tail = %+v, want the appended entry", got)
	}
	if want := path + ".lock"; locked != want {
		t.Errorf("Tail locked %q, want %q", locked, want)
	}
	if held {
		t.Error("Tail returned without releasing the lock")
	}
}

// A live file that cannot be read must fail the append rather than be
// overwritten by a record written blind.
func TestLedgerAppendUnreadableLiveErrors(t *testing.T) {
	t.Parallel()
	// A component longer than any filesystem allows fails the read with
	// something other than not-exist, and the lock is out of the way so the
	// read is what the append reaches.
	l := NewLedger(filepath.Join(t.TempDir(), strings.Repeat("a", 300)), 1<<20)
	l.acquire = func(string) (func(), error) { return func() {}, nil }
	if err := l.Append(LedgerEntry{Target: "registry"}); err == nil {
		t.Fatal("an unreadable live file must return an error")
	}
}

func TestLedgerAppendOpenFailureErrors(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	l.open = func(string) (*os.File, error) { return nil, errors.New("open: read-only filesystem") }
	err := l.Append(LedgerEntry{Target: "registry"})
	if err == nil || !strings.Contains(err.Error(), "read-only filesystem") {
		t.Fatalf("Append = %v, want the open failure rather than a silently shorter ledger", err)
	}
}

func TestLedgerAppendLockFailureErrors(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	l.acquire = func(string) (func(), error) { return nil, errors.New("lock: cannot acquire") }
	err := l.Append(LedgerEntry{Target: "registry"})
	if err == nil || !strings.Contains(err.Error(), "cannot acquire") {
		t.Fatalf("Append = %v, want the lock failure so the run is not left looking recorded", err)
	}
	if _, statErr := os.Stat(l.path); !os.IsNotExist(statErr) {
		t.Errorf("an append that could not lock must not write: %v", statErr)
	}
}

func TestLedgerTailLockFailureErrors(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	l.acquire = func(string) (func(), error) { return nil, errors.New("lock: cannot acquire") }
	if _, err := l.Tail(1); err == nil || !strings.Contains(err.Error(), "cannot acquire") {
		t.Fatalf("Tail = %v, want the lock failure rather than an unread ledger", err)
	}
}

func TestLedgerAppendRefusesUnencodableEntry(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	// json cannot encode a year outside 0..9999, so nothing is written.
	err := l.Append(LedgerEntry{TS: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), Target: "registry"})
	if err == nil {
		t.Fatal("an entry that does not encode must be refused")
	}
	if _, statErr := os.Stat(l.path); !os.IsNotExist(statErr) {
		t.Errorf("a refused entry must not create the file: %v", statErr)
	}
}

func TestLedgerWriteFailureErrors(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 1<<20)
	l.write = func(*os.File, []byte) (int, error) { return 0, errors.New("no space left") }
	err := l.Append(LedgerEntry{Target: "registry"})
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("Append = %v, want the write failure so the run is not left looking recorded", err)
	}
}

func TestLedgerUnreadablePathErrors(t *testing.T) {
	t.Parallel()
	// A component longer than any filesystem allows fails the stat with
	// something other than not-exist, and that must reach the caller.
	l := NewLedger(filepath.Join(t.TempDir(), strings.Repeat("a", 300)), 1<<20)
	if err := l.Append(LedgerEntry{Target: "registry"}); err == nil {
		t.Fatal("a ledger path that cannot be inspected must return an error")
	}
}

func TestLedgerAppendIntoMissingDirErrors(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "absent", "ledger.jsonl"), 1<<20)
	if err := l.Append(LedgerEntry{Target: "registry"}); err == nil {
		t.Fatal("Append into a directory that does not exist must return an error")
	}
}

func TestLedgerRotationFailureErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(path, 1)
	if err := l.Append(LedgerEntry{Target: "registry"}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// A directory where the rotation target belongs makes the rename fail,
	// and a ledger that cannot rotate must not keep growing instead.
	if err := os.Mkdir(path+".1", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(LedgerEntry{Target: "registry"}); err == nil {
		t.Fatal("a rotation that cannot rename must return an error")
	}
}

func TestLedgerTailUnreadableErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLedger(path, 1<<20).Tail(1); err == nil {
		t.Fatal("tail of an unreadable path must return an error")
	}
}

func TestLedgerTailUnreadableRotationErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.Mkdir(path+".1", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLedger(path, 1<<20).Tail(1); err == nil {
		t.Fatal("an unreadable rotation must return an error, not read as the end of the ledger")
	}
}
