package events

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// logLine is one event as the log holds it.
func logLine(typ, at, bead string) string {
	return fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"x","payload":{"bead":%q}}`, at, typ, bead)
}

// appendLog appends text to the town's events log, creating it when needed.
func appendLog(t *testing.T, town, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(town, EventsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// The reader folds appended lines into one clock across calls, so the
// dashboard's Age and the guard read the same map without the log being parsed
// again (gt-tzbtf).
func TestDispatchReaderReadsOnlyTheAppendedLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	appendLog(t, town, logLine(TypeSling, t9, "gt-a")+"\n"+logLine(TypeSling, t10, "gt-b")+"\n")

	r := NewDispatchReader(town)
	got, err := r.Starts()
	if err != nil {
		t.Fatalf("Starts: %v", err)
	}
	if at := got["gt-a"]; !at.Equal(mustTime(t, t9)) {
		t.Errorf("gt-a = %v, want %v", at, t9)
	}

	// A done event and the dispatch after it, appended between polls: the new
	// run's clock arrives without the first two lines being read again.
	appendLog(t, town, logLine(TypeDone, t10, "gt-a")+"\n"+logLine(TypeSling, t11, "gt-a")+"\n")
	got, err = r.Starts()
	if err != nil {
		t.Fatalf("Starts after append: %v", err)
	}
	if at := got["gt-a"]; !at.Equal(mustTime(t, t11)) {
		t.Errorf("gt-a = %v, want the dispatch after its done event, %v", at, t11)
	}
	if at := got["gt-b"]; !at.Equal(mustTime(t, t10)) {
		t.Errorf("gt-b = %v, want %v", at, t10)
	}
	if r.offset <= int64(len(logLine(TypeSling, t9, "gt-a")+"\n"+logLine(TypeSling, t10, "gt-b")+"\n")) {
		t.Errorf("offset = %d, want it to have advanced past both reads", r.offset)
	}

	// A poll that finds nothing appended answers from what the reader holds.
	quiet, err := r.Starts()
	if err != nil {
		t.Fatalf("Starts with nothing appended: %v", err)
	}
	if !quiet["gt-a"].Equal(got["gt-a"]) || !quiet["gt-b"].Equal(got["gt-b"]) {
		t.Errorf("quiet poll = %v, want the previous reading %v", quiet, got)
	}
}

// A line a writer has not finished is not folded in until it is whole: half an
// event parsed now would lose the bead it names.
func TestDispatchReaderHoldsAPartialLineForTheNextRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	line := logLine(TypeSling, t9, "gt-a") + "\n"
	appendLog(t, town, line)
	r := NewDispatchReader(town)
	if _, err := r.Starts(); err != nil {
		t.Fatalf("Starts: %v", err)
	}

	next := logLine(TypeSling, t10, "gt-b") + "\n"
	appendLog(t, town, next[:len(next)/2])
	got, err := r.Starts()
	if err != nil {
		t.Fatalf("Starts with a partial line: %v", err)
	}
	if _, ok := got["gt-b"]; ok {
		t.Fatalf("read half a line as a dispatch: %v", got)
	}
	if at := got["gt-a"]; !at.Equal(mustTime(t, t9)) {
		t.Errorf("gt-a = %v, want %v kept across the partial read", at, t9)
	}

	appendLog(t, town, next[len(next)/2:])
	got, err = r.Starts()
	if err != nil {
		t.Fatalf("Starts after the line finished: %v", err)
	}
	if at, ok := got["gt-b"]; !ok || !at.Equal(mustTime(t, t10)) {
		t.Errorf("gt-b = %v (%t), want the finished line to date it, %v", at, ok, t10)
	}
}

// A log the prune replaced is read from its start: offsets from the old file
// would name bytes of a history that is gone.
func TestDispatchReaderStartsOverOnAReplacedLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	appendLog(t, town, logLine(TypeSling, t9, "gt-a")+"\n")
	r := NewDispatchReader(town)
	if _, err := r.Starts(); err != nil {
		t.Fatalf("Starts: %v", err)
	}

	// The prune's replacement, renamed into place: the old dispatch is gone and
	// the kept history names another bead.
	replacement := filepath.Join(town, EventsFile+".prune.tmp")
	if err := os.WriteFile(replacement, []byte(logLine(TypeSling, t11, "gt-b")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(town, EventsFile)); err != nil {
		t.Fatal(err)
	}
	got, err := r.Starts()
	if err != nil {
		t.Fatalf("Starts after the replace: %v", err)
	}
	if _, ok := got["gt-a"]; ok {
		t.Errorf("kept a dispatch from the replaced file: %v", got)
	}
	if at := got["gt-b"]; !at.Equal(mustTime(t, t11)) {
		t.Errorf("gt-b = %v, want %v from the replacement", at, t11)
	}
}

// A town with no log has dispatched nothing; a log that cannot be read is an
// error, so the dashboard marks the reading unavailable instead of drawing
// every seat as young.
func TestDispatchReaderNoLogAndUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	got, err := NewDispatchReader(town).Starts()
	if err != nil || len(got) != 0 {
		t.Fatalf("Starts with no log = %v (%d beads), want an empty clock and no error", err, len(got))
	}

	bad := t.TempDir()
	if err := os.Mkdir(filepath.Join(bad, EventsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDispatchReader(bad).Starts(); err == nil {
		t.Fatal("Starts read an unreadable log as an empty one")
	}
}

// The reader's clock is the same map a whole-log read gives, appended one line
// at a time.
func TestDispatchReaderMatchesAWholeLogRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var whole []Event
	for _, l := range []struct{ typ, at, bead string }{
		{TypeSling, t9, "gt-a"},
		{TypeSling, t10, "gt-b"},
		{TypeDone, t10, "gt-a"},
		{TypeSling, t11, "gt-a"},
		{TypeOverrunStop, t11, "gt-b"},
		{TypeBeadClosed, t11, "gt-b"},
		{TypeSling, t12, "gt-b"},
	} {
		line := logLine(l.typ, l.at, l.bead) + "\n"
		appendLog(t, town, line)
		evs, err := Read(town)
		if err != nil {
			t.Fatal(err)
		}
		whole = evs
	}
	want := DispatchStarts(whole)
	got, err := NewDispatchReader(town).Starts()
	if err != nil {
		t.Fatalf("Starts: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("reader dated %d beads, want %d: %v", len(got), len(want), got)
	}
	for bead, at := range want {
		if g, ok := got[bead]; !ok || !g.Equal(at) {
			t.Errorf("%s = %v (%t), want %v", bead, g, ok, at)
		}
	}
}

// A log the reader cannot open is an error, not a town that dispatched nothing:
// the dashboard shows the Age as unavailable rather than dating every seat from
// a default. A regular file where the town root belongs is the case.
func TestDispatchReaderReportsALogItCannotOpen(t *testing.T) {
	t.Parallel()
	town := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(town, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDispatchReader(town).Starts(); err == nil {
		t.Fatal("Starts read a log it could not open as an empty one")
	}
}
