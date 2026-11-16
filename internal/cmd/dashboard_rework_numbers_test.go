package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reworkProgramNumbersTown writes data at the reader's document path in a fresh
// town and returns the town.
func reworkProgramNumbersTown(t *testing.T, data []byte) string {
	t.Helper()
	town := t.TempDir()
	dir := filepath.Join(town, ".runtime", "rework-numbers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "latest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// reworkProgramNumbersLog collects the lines a reader's failures are logged at.
type reworkProgramNumbersLog struct {
	lines []string
}

func (l *reworkProgramNumbersLog) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// TestReworkProgramNumbersReaderPassesDocument: the feed carries the daemon's
// document byte for byte.
func TestReworkProgramNumbersReaderPassesDocument(t *testing.T) {
	t.Parallel()

	document := []byte(`{"generated_at":"2026-11-16T12:00:00Z","numbers":{"week":"2026-11-09"}}`)
	town := reworkProgramNumbersTown(t, document)
	var log reworkProgramNumbersLog
	reader := newDashReworkNumbersReader(town).withWhy(newDashWhyLog(log.logf))
	got := reader.read()
	if string(got) != string(document) {
		t.Fatalf("the reader gave %s, want the document verbatim", got)
	}
	if !json.Valid(got) {
		t.Fatal("the reader gave something that is not JSON")
	}
	if len(log.lines) != 0 {
		t.Fatalf("a good read logged %v", log.lines)
	}
}

// TestReworkProgramNumbersReaderRefusesInvalidJSON: a document the page could
// not parse is refused, and the failure is logged once, not once per poll.
func TestReworkProgramNumbersReaderRefusesInvalidJSON(t *testing.T) {
	t.Parallel()

	town := reworkProgramNumbersTown(t, []byte("not json"))
	var log reworkProgramNumbersLog
	reader := newDashReworkNumbersReader(town).withWhy(newDashWhyLog(log.logf))
	if got := reader.read(); got != nil {
		t.Fatalf("the reader passed %s, want nil", got)
	}
	if got := reader.read(); got != nil {
		t.Fatalf("the reader passed %s on the second poll, want nil", got)
	}
	if len(log.lines) != 1 {
		t.Fatalf("a repeated failure logged %d lines, want one", len(log.lines))
	}
}

// TestReworkProgramNumbersReaderRefusesOversize: a document over the cap is
// refused even when it is valid JSON, because the page parses its state whole.
func TestReworkProgramNumbersReaderRefusesOversize(t *testing.T) {
	t.Parallel()

	// A valid document padded past the cap with whitespace, which JSON allows.
	document := make([]byte, reworkNumbersMaxBytes+1)
	copy(document, `{"numbers":{}}`)
	for i := len(`{"numbers":{}}`); i < len(document); i++ {
		document[i] = ' '
	}
	if !json.Valid(document) {
		t.Fatal("the test's oversize document is not valid JSON")
	}
	town := reworkProgramNumbersTown(t, document)
	var log reworkProgramNumbersLog
	reader := newDashReworkNumbersReader(town).withWhy(newDashWhyLog(log.logf))
	if got := reader.read(); got != nil {
		t.Fatalf("the reader passed a document of %d bytes, want nil", len(got))
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "larger than") {
		t.Fatalf("the oversize read logged %v, want one line naming the size", log.lines)
	}
}

// TestReworkProgramNumbersReaderMissingFileIsNoRun: no document yet is a step
// that has not run, not a failed read.
func TestReworkProgramNumbersReaderMissingFileIsNoRun(t *testing.T) {
	t.Parallel()

	var log reworkProgramNumbersLog
	reader := newDashReworkNumbersReader(t.TempDir()).withWhy(newDashWhyLog(log.logf))
	if got := reader.read(); got != nil {
		t.Fatalf("the reader gave %s, want nil", got)
	}
	if len(log.lines) != 0 {
		t.Fatalf("a town with no document logged %v", log.lines)
	}
}
