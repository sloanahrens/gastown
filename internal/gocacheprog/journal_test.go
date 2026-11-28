package gocacheprog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// journalT0 is the time a journal's start line carries.
var journalT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// requireRecords fails unless got holds want, in order, with equal times.
func requireRecords(t *testing.T, got, want []Record) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("records = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Op != w.Op || g.ActionID != w.ActionID || g.OutputID != w.OutputID || g.Size != w.Size || !g.Time.Equal(w.Time) {
			t.Errorf("record %d = %+v, want %+v", i, g, w)
		}
	}
}

// journalFile writes body as a journal file under a fresh temp dir and returns its path.
func journalFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.log")
	must(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

const (
	testStartLine = `{"op":"start","time":"2026-01-02T03:04:05Z"}` + "\n"
	testPutLine   = `{"op":"put","action":"aa","output":"bb","size":5,"time":"2026-01-02T03:04:06Z"}` + "\n"
	testUseLine   = `{"op":"use","action":"aa","time":"2026-01-02T03:04:07Z"}` + "\n"
)

func TestJournalReadsStartAndRecordsInOrder(t *testing.T) {
	t.Parallel()
	path := journalFile(t, testStartLine+testPutLine+testUseLine+
		`{"op":"put","action":"cc","output":"dd","size":7,"time":"2026-01-02T03:04:08Z"}`+"\n")
	got, err := ReadJournal(path)
	must(t, err)
	if !got.Start.Equal(journalT0) {
		t.Errorf("start = %v, want %v", got.Start, journalT0)
	}
	requireRecords(t, got.Records, []Record{
		{Op: "put", ActionID: "aa", OutputID: "bb", Size: 5, Time: journalT0.Add(1 * time.Second)},
		{Op: "use", ActionID: "aa", Time: journalT0.Add(2 * time.Second)},
		{Op: "put", ActionID: "cc", OutputID: "dd", Size: 7, Time: journalT0.Add(3 * time.Second)},
	})
	if got.Undecodable != 0 {
		t.Errorf("undecodable = %d, want 0", got.Undecodable)
	}
}

func TestJournalCutLastLineIgnored(t *testing.T) {
	t.Parallel()
	path := journalFile(t, testStartLine+testPutLine+`{"op":"use","action":"aa"`)
	got, err := ReadJournal(path)
	must(t, err)
	if got.Undecodable != 0 {
		t.Errorf("undecodable = %d, want 0: a line with no newline is not counted", got.Undecodable)
	}
	requireRecords(t, got.Records, []Record{
		{Op: "put", ActionID: "aa", OutputID: "bb", Size: 5, Time: journalT0.Add(1 * time.Second)},
	})
}

func TestJournalCountsBadLines(t *testing.T) {
	t.Parallel()
	path := journalFile(t, testStartLine+
		"not json\n"+
		testPutLine+
		`{"op":"bogus","time":"2026-01-02T03:04:06Z"}`+"\n"+
		`{"op":"put","action":"zz","output":"bb","size":1,"time":"2026-01-02T03:04:06Z"}`+"\n"+
		testStartLine+
		testUseLine)
	got, err := ReadJournal(path)
	must(t, err)
	if got.Undecodable != 4 {
		t.Errorf("undecodable = %d, want 4 (not json, unknown op, non-hex action, second start)", got.Undecodable)
	}
	if len(got.Records) != 2 {
		t.Errorf("records = %d, want 2 (the put and the use)", len(got.Records))
	}
}

func TestJournalCountsMalformedRecords(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"put with empty output":   `{"op":"put","action":"aa","size":5,"time":"2026-01-02T03:04:06Z"}`,
		"put with negative size":  `{"op":"put","action":"aa","output":"bb","size":-1,"time":"2026-01-02T03:04:06Z"}`,
		"use with empty action":   `{"op":"use","time":"2026-01-02T03:04:06Z"}`,
		"use with non-hex action": `{"op":"use","action":"g0","time":"2026-01-02T03:04:06Z"}`,
	}
	for name, line := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			t.Parallel()
			path := journalFile(t, testStartLine+line+"\n")
			got, err := ReadJournal(path)
			must(t, err)
			if got.Undecodable != 1 {
				t.Errorf("undecodable = %d, want 1", got.Undecodable)
			}
			if len(got.Records) != 0 {
				t.Errorf("records = %d, want 0: a malformed record is not kept", len(got.Records))
			}
		})
	}
}

func TestJournalMissingStartLineIsError(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty file":           "",
		"put before any start": testPutLine,
		"start cut short":      `{"op":"start","time":"2026-01-02T03:04:05Z"}`,
		"start not decodable":  "not json\n" + testPutLine,
		"start without time":   `{"op":"start"}` + "\n",
	}
	for name, body := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			t.Parallel()
			path := journalFile(t, body)
			_, err := ReadJournal(path)
			if err == nil {
				t.Fatal("ReadJournal: want error for a journal with no usable start line")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name %s", err, path)
			}
		})
	}
}

func TestJournalReadErrorsOnMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := ReadJournal(filepath.Join(t.TempDir(), "absent.log")); err == nil {
		t.Fatal("ReadJournal of a missing file: want error, not an empty journal")
	}
}
