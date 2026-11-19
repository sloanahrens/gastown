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
	verdict, unencodable := ok, ok
	verdict.Kind = "verdict"
	unencodable.At = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	ro := t.TempDir()
	if err := (RecordQueue{Path: filepath.Join(ro, "rw.jsonl")}).Enqueue(ok); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the fixture directory drops its write bit (0500 keeps the search bit) so the append's create and the rewrite's temp file are refused
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		//testpolicy:allow no-exec-files — the target is the fixture directory: t.TempDir needs its search bit back to remove it
		if err := os.Chmod(ro, 0o700); err != nil {
			t.Errorf("restore fixture directory permissions: %v", err)
		}
	})
	cases := []op{
		{"pending reads an unopenable queue", func() error { _, err := (RecordQueue{Path: long}).Pending(); return err }},
		{"enqueue reads an unopenable queue", func() error { return (RecordQueue{Path: long}).Enqueue(ok) }},
		{"remove reads an unopenable queue", func() error { return (RecordQueue{Path: long}).Remove(ok) }},
		{"unknown kind", func() error { return (RecordQueue{Path: long}).Enqueue(verdict) }},
		{"unencodable record", func() error { return (RecordQueue{Path: filepath.Join(t.TempDir(), "a")}).Enqueue(unencodable) }},
		{"append into a read-only directory", func() error { return (RecordQueue{Path: filepath.Join(ro, "new.jsonl")}).Enqueue(ok) }},
		{"rewrite into a read-only directory", func() error { return (RecordQueue{Path: filepath.Join(ro, "rw.jsonl")}).Remove(ok) }},
	}
	for _, c := range cases {
		if err := c.run(); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
}
