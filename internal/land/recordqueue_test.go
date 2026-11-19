package land

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordQueueOps(t *testing.T) {
	t.Parallel()
	q := RecordQueue{Path: filepath.Join(t.TempDir(), "missing", "q.jsonl")}
	one := PendingRecord{Bead: "gt-a", Kind: RecordKindNotes, Key: "k1", Text: "one", At: time.Unix(1, 0).UTC()}
	two := PendingRecord{Bead: "gt-a", Kind: RecordKindComment, Key: "k2", Text: "two", At: time.Unix(2, 0).UTC()}
	// The first Enqueue creates the missing directory; the repeat is a duplicate.
	for _, rec := range []PendingRecord{one, one, two} {
		if err := q.Enqueue(rec); err != nil {
			t.Fatalf("Enqueue(%s): %v", rec.Key, err)
		}
	}
	got, err := q.Pending()
	if err != nil || len(got) != 2 || got[0].Key != "k1" || got[1].Key != "k2" || got[0].Text != "one" {
		t.Fatalf("Pending = %+v, %v; want k1 then k2 (duplicate skipped)", got, err)
	}
	if err := q.Remove(one); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, err = q.Pending(); err != nil || len(got) != 1 || got[0].Key != "k2" {
		t.Errorf("after Remove Pending = %+v, %v; want only k2", got, err)
	}
}

func TestRecordQueueRemoveMissing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gone", "q.jsonl")
	err := (RecordQueue{Path: path}).Remove(PendingRecord{Bead: "gt-a", Kind: RecordKindNotes, Key: "k"})
	if err != nil {
		t.Fatalf("Remove from a missing queue: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Remove created %s; want no file (stat err %v)", path, err)
	}
}

func TestRecordQueueTornLastLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "q.jsonl")
	body := `{"bead":"gt-a","kind":"notes","key":"k1","text":"one"}` + "\n" +
		`{"bead":"gt-b","kind":"comment","key":"k2","text":"two"}` + "\n" + `{"bead":"gt-c","kind":"no`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	q := RecordQueue{Path: path}
	got, err := q.Pending()
	if err == nil || !strings.Contains(err.Error(), "unparsable") || len(got) != 2 {
		t.Fatalf("Pending = %+v, %v; want the two records before the torn line, torn reported", got, err)
	}
	if err := q.Remove(got[0]); err != nil {
		t.Fatalf("Remove with a torn line present: %v", err)
	}
	if got, err = q.Pending(); err == nil || len(got) != 1 || got[0].Key != "k2" {
		t.Errorf("after Remove Pending = %+v, %v; want only k2 with the torn line still reported", got, err)
	}
}

func TestRecordQueueFailures(t *testing.T) {
	t.Parallel()
	type op struct {
		name string
		run  func() error
	}
	ok := PendingRecord{Bead: "gt-a", Kind: RecordKindNotes, Key: "k", Text: "one", At: time.Unix(1, 0).UTC()}
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 300))
	verdict, unencodable, beadless, keyless := ok, ok, ok, ok
	verdict.Kind = "verdict"
	unencodable.At = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	beadless.Bead = ""
	keyless.Key = ""
	// Remove's rewrite names its temp file base+".tmp.<digits>", so a 250-byte
	// base is a valid queue name whose temp name is over NAME_MAX: the rewrite is
	// refused whatever account runs the test.
	tempTooLong := filepath.Join(t.TempDir(), strings.Repeat("y", 250))
	if err := (RecordQueue{Path: tempTooLong}).Enqueue(ok); err != nil {
		t.Fatal(err)
	}
	// Group and other can write this directory, so the landings dir's private
	// check refuses it for every account, root included.
	open := t.TempDir()
	//testpolicy:allow no-exec-files — a directory, not a file: its owner search bit lets the append reach the private-mode check
	if err := os.Chmod(open, 0o766); err != nil {
		t.Fatal(err)
	}
	cases := []op{
		{"pending reads an unopenable queue", func() error { _, err := (RecordQueue{Path: long}).Pending(); return err }},
		{"enqueue reads an unopenable queue", func() error { return (RecordQueue{Path: long}).Enqueue(ok) }},
		{"remove reads an unopenable queue", func() error { return (RecordQueue{Path: long}).Remove(ok) }},
		{"pending reads a directory as the queue", func() error { _, err := (RecordQueue{Path: t.TempDir()}).Pending(); return err }},
		{"unknown kind", func() error { return (RecordQueue{Path: long}).Enqueue(verdict) }},
		{"enqueue without a bead", func() error { return (RecordQueue{Path: filepath.Join(t.TempDir(), "a")}).Enqueue(beadless) }},
		{"enqueue without a key", func() error { return (RecordQueue{Path: filepath.Join(t.TempDir(), "a")}).Enqueue(keyless) }},
		{"unencodable record", func() error { return (RecordQueue{Path: filepath.Join(t.TempDir(), "a")}).Enqueue(unencodable) }},
		{"append into a non-private directory", func() error { return (RecordQueue{Path: filepath.Join(open, "new.jsonl")}).Enqueue(ok) }},
		{"rewrite whose temp name is too long", func() error { return (RecordQueue{Path: tempTooLong}).Remove(ok) }},
	}
	for _, c := range cases {
		if err := c.run(); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
}
