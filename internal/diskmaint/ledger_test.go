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
