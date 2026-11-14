package estlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logLine renders r as the log holds it, for a test that writes a log by hand.
func logLine(t *testing.T, r Record) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal %s: %v", r.BeadID, err)
	}
	return string(b)
}

// writeLog writes lines, one per line with a final newline, to the estimate
// log of a fresh town, and returns the town root.
func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(Path(town)), 0o755); err != nil {
		t.Fatalf("create the log directory: %v", err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(Path(town), []byte(body), 0o644); err != nil {
		t.Fatalf("write the log: %v", err)
	}
	return town
}

// appendRaw appends text to the log of town with no newline of its own, for a
// test that writes a line the package's writer would not.
func appendRaw(t *testing.T, town, text string) {
	t.Helper()
	f, err := os.OpenFile(Path(town), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("append to the log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the log: %v", err)
	}
}

// TestReadReturnsAppendedRecordsInOrder: what Append wrote reads back through
// Read in the same order with every field equal, for an estimate with all
// components and one with only the shape minutes.
func TestReadReturnsAppendedRecordsInOrder(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want := []Record{
		sampleRecord("gt-first"),
		shapeOnlyRecord("gt-second"),
		sampleRecord("gt-third"),
	}
	for _, r := range want {
		if err := Append(town, r); err != nil {
			t.Fatalf("Append(%s): %v", r.BeadID, err)
		}
	}
	got, skipped, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if len(got) != len(want) {
		t.Fatalf("records = %d, want %d", len(got), len(want))
	}
	for i := range want {
		assertRecordEqual(t, got[i], want[i])
	}
}

// TestReadSkipsAndCountsMalformedLines: a line that does not decode as a Record
// is skipped and counted, and the records on either side of it still read.
func TestReadSkipsAndCountsMalformedLines(t *testing.T) {
	t.Parallel()
	before, after := sampleRecord("gt-before"), shapeOnlyRecord("gt-after")
	tests := []struct {
		name string
		line string
	}{
		{"truncated JSON", `{"bead_id":"gt-x"`},
		{"not an object", `[1,2]`},
		{"wrong field type", `{"minutes":{"shape":"soon"}}`},
		{"JSON null", `null`},
		{"null among spaces", `  null  `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := writeLog(t, logLine(t, before), tt.line, logLine(t, after))
			got, malformed, err := Read(town)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if malformed != 1 {
				t.Errorf("skipped = %d, want 1", malformed)
			}
			if len(got) != 2 {
				t.Fatalf("records = %d, want 2", len(got))
			}
			assertRecordEqual(t, got[0], before)
			assertRecordEqual(t, got[1], after)
		})
	}
}

// TestReadSkipsAMalformedFinalLine: a torn last line, which a writer has not
// terminated yet, is skipped and counted without losing the records before it.
func TestReadSkipsAMalformedFinalLine(t *testing.T) {
	t.Parallel()
	want := sampleRecord("gt-a")
	town := writeLog(t, logLine(t, want))
	appendRaw(t, town, `{"bead_id":"gt-tor`)
	got, malformed, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if malformed != 1 {
		t.Errorf("skipped = %d, want 1", malformed)
	}
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1", len(got))
	}
	assertRecordEqual(t, got[0], want)
}

// TestReadDoesNotCountBlankLines: a blank line is no record, and it is not
// counted among the malformed ones.
func TestReadDoesNotCountBlankLines(t *testing.T) {
	t.Parallel()
	first, second := sampleRecord("gt-a"), shapeOnlyRecord("gt-b")
	town := writeLog(t, logLine(t, first), "", "   ", logLine(t, second))
	got, malformed, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if malformed != 0 {
		t.Errorf("skipped = %d, want 0", malformed)
	}
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	assertRecordEqual(t, got[0], first)
	assertRecordEqual(t, got[1], second)
}

// TestReadMissingLogIsNoRecords: a town that has never written an estimate has
// no log, which reads as no records rather than an error.
func TestReadMissingLogIsNoRecords(t *testing.T) {
	t.Parallel()
	got, malformed, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read = %v, want nil for a log that does not exist", err)
	}
	if len(got) != 0 || malformed != 0 {
		t.Errorf("records = %d, skipped = %d, want 0 and 0", len(got), malformed)
	}
}

// TestReadRefusesNoTownRoot: an empty town root names no log, so Read refuses
// instead of opening a path relative to the working directory.
func TestReadRefusesNoTownRoot(t *testing.T) {
	t.Parallel()
	if _, _, err := Read(""); !errors.Is(err, ErrNoTownRoot) {
		t.Fatalf("Read with no town root = %v, want ErrNoTownRoot", err)
	}
}

// TestReadErrorsNameTheLogPath: a log that cannot be read is an error naming
// the file, never an empty log. A read-only file does not drive this: a test
// run as root reads one anyway, so the rows put a file or directory where the
// log needs a readable file.
func TestReadErrorsNameTheLogPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// block makes the log path or one of its parents unreadable as a log.
		block func(t *testing.T, town string)
	}{
		{
			name: "the log path is a directory",
			block: func(t *testing.T, town string) {
				if err := os.MkdirAll(Path(town), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a parent of the log path is a file",
			block: func(t *testing.T, town string) {
				if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tt.block(t, town)
			got, _, err := Read(town)
			if err == nil {
				t.Fatalf("Read = nil error, want an error for an unreadable log")
			}
			if !strings.Contains(err.Error(), Path(town)) {
				t.Errorf("error = %q, want it to name %q", err, Path(town))
			}
			if got != nil {
				t.Errorf("records = %+v, want none beside an error", got)
			}
		})
	}
}

// TestReadOverlongLineIsAReadFailure: a line past the scanner's ceiling is a
// read failure, not a malformed line, so it is never skipped in silence.
func TestReadOverlongLineIsAReadFailure(t *testing.T) {
	t.Parallel()
	town := writeLog(t, logLine(t, sampleRecord("gt-a")), strings.Repeat("x", 1<<20+1))
	got, _, err := Read(town)
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("Read = %v, want bufio.ErrTooLong", err)
	}
	if got != nil {
		t.Errorf("records = %d, want none beside a failed read", len(got))
	}
}
