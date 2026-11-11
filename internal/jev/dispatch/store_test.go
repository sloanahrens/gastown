package dispatch

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// logLine is one well-formed log line for bead at ts.
func logLine(t *testing.T, bead string, ts time.Time) string {
	t.Helper()
	b, err := json.Marshal(Line{TS: ts, Bead: bead})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeLog writes the lines to a fresh file and returns its path.
func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJevDispatchReadAllCountsMalformedLines(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	path := writeLog(t, logLine(t, "gt-a", ts), "{not json", logLine(t, "gt-b", ts.Add(time.Minute)))

	got, malformed, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if malformed != 1 {
		t.Errorf("malformed = %d, want 1", malformed)
	}
	if len(got) != 2 || got[0].Bead != "gt-a" || got[1].Bead != "gt-b" {
		t.Errorf("lines = %+v, want gt-a then gt-b", got)
	}
}

func TestJevDispatchReadAllMissingFileIsAnError(t *testing.T) {
	t.Parallel()
	_, _, err := ReadAll(filepath.Join(t.TempDir(), "absent.jsonl"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
}

func TestJevDispatchReadAllFailsOnAnOverlongLine(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	// A line past the 1 MiB scanner limit is a read failure, not a malformed
	// line: the read stops with an error and returns no lines.
	path := writeLog(t, logLine(t, "gt-a", ts), strings.Repeat("x", 1<<20+1))

	got, _, err := ReadAll(path)
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
	if got != nil {
		t.Errorf("lines = %d, want none after a failed read", len(got))
	}
}

func TestJevDispatchReadAllEmptyFileIsEmpty(t *testing.T) {
	t.Parallel()
	path := writeLog(t)

	got, malformed, err := ReadAll(path)
	if err != nil {
		t.Fatalf("err = %v, want nil for an empty log", err)
	}
	if len(got) != 0 || malformed != 0 {
		t.Errorf("lines = %d, malformed = %d, want 0 and 0", len(got), malformed)
	}
}

func TestJevDispatchFirstPerBeadKeepsTheEarliestLine(t *testing.T) {
	t.Parallel()
	early := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	// gt-a is logged twice; gt-b once. Every order of the three lines must
	// keep gt-a's early line.
	a1 := Line{Bead: "gt-a", TS: early, Agent: "first"}
	a2 := Line{Bead: "gt-a", TS: late, Agent: "second"}
	b := Line{Bead: "gt-b", TS: late}
	orders := [][]Line{
		{a1, a2, b},
		{a2, a1, b},
		{a2, b, a1},
		{a1, b, a2},
	}
	for _, lines := range orders {
		got := FirstPerBead(lines)
		if len(got) != 2 {
			t.Fatalf("beads = %d, want 2", len(got))
		}
		if !got["gt-a"].TS.Equal(early) || got["gt-a"].Agent != "first" {
			t.Errorf("order %v: gt-a = %+v, want the early line", lines, got["gt-a"])
		}
		if !got["gt-b"].TS.Equal(late) {
			t.Errorf("order %v: gt-b = %+v, want its only line", lines, got["gt-b"])
		}
	}
}
