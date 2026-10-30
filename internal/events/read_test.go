package events

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadReturnsWrittenEventsInOrder covers the round trip gt log depends on:
// what LogFeedTo appends is what Read hands back, payload intact (gt-i057g).
func TestReadReturnsWrittenEventsInOrder(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	if err := LogFeedTo(townRoot, TypeWake, "gastown/polecats/shale", WakePayload("gastown", "gt-i057g")); err != nil {
		t.Fatalf("LogFeedTo wake: %v", err)
	}
	if err := LogFeedTo(townRoot, TypeDone, "gastown/polecats/shale", DonePayload("gt-i057g", "polecat/shale/gt-i057g")); err != nil {
		t.Fatalf("LogFeedTo done: %v", err)
	}

	got, err := Read(townRoot)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Read returned %d events, want 2", len(got))
	}
	if got[0].Type != TypeWake || got[1].Type != TypeDone {
		t.Errorf("types = %q, %q; want %q, %q", got[0].Type, got[1].Type, TypeWake, TypeDone)
	}
	for i, e := range got {
		if e.Actor != "gastown/polecats/shale" {
			t.Errorf("event %d actor = %q", i, e.Actor)
		}
		if _, err := time.Parse(time.RFC3339, e.Timestamp); err != nil {
			t.Errorf("event %d timestamp %q is not RFC3339: %v", i, e.Timestamp, err)
		}
	}
	if context, _ := got[0].Payload["context"].(string); context != "gt-i057g" {
		t.Errorf("wake context = %#v, want gt-i057g", got[0].Payload["context"])
	}
	if rig, _ := got[0].Payload["rig"].(string); rig != "gastown" {
		t.Errorf("wake rig = %#v, want gastown", got[0].Payload["rig"])
	}
}

// TestReadMissingFileIsEmpty covers a town that has not logged anything yet:
// an absent log is empty, not an error, which is what makes gt log's
// "no events" branch reachable before the first event.
func TestReadMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	got, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Read = %d events, want 0", len(got))
	}
}

// TestReadSkipsMalformedLines covers the torn tail of a file a writer is still
// appending to, and any other line that is not an event: it must not hide the
// well-formed events around it.
func TestReadSkipsMalformedLines(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := LogFeedTo(townRoot, TypeWake, "gastown/polecats/shale", WakePayload("gastown", "first")); err != nil {
		t.Fatalf("LogFeedTo: %v", err)
	}

	path := filepath.Join(townRoot, EventsFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open events file: %v", err)
	}
	if _, err := f.WriteString("{\"ts\":\"not-a-time\",\"type\":\n"); err != nil {
		t.Fatalf("append malformed line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := LogFeedTo(townRoot, TypeKill, "gastown/polecats/shale", KillPayload("gastown", "shale", "gt session stop")); err != nil {
		t.Fatalf("LogFeedTo kill: %v", err)
	}

	got, err := Read(townRoot)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Read returned %d events, want the 2 well-formed ones", len(got))
	}
	if got[0].Type != TypeWake || got[1].Type != TypeKill {
		t.Errorf("types = %q, %q; want %q, %q", got[0].Type, got[1].Type, TypeWake, TypeKill)
	}
}

// TestReadBoundedRefusesALogPastItsBound: a reader that needs the whole log
// gets it while it fits, and an error when it does not — never a window of it,
// which a caller could take for the log (gt-v7ptp).
func TestReadBoundedRefusesALogPastItsBound(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, context := range []string{"first", "second", "third"} {
		if err := LogFeedTo(townRoot, TypeWake, "gastown/polecats/shale", WakePayload("gastown", context)); err != nil {
			t.Fatalf("LogFeedTo %s: %v", context, err)
		}
	}
	path := filepath.Join(townRoot, EventsFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// A log no larger than the bound is read whole.
	got, err := ReadBounded(townRoot, info.Size())
	if err != nil {
		t.Fatalf("ReadBounded within its bound: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ReadBounded = %d events, want 3", len(got))
	}

	// One byte past the bound refuses the log rather than returning a window of
	// it: a caller cannot tell a window that begins inside a run from the run.
	for _, bound := range []int64{info.Size() - 1, 1} {
		got, err := ReadBounded(townRoot, bound)
		if !errors.Is(err, ErrEventsLogTooLarge) {
			t.Errorf("ReadBounded(%d) error = %v, want ErrEventsLogTooLarge", bound, err)
		}
		if got != nil {
			t.Errorf("ReadBounded(%d) = %d events, want none with the error", bound, len(got))
		}
	}

	// A missing log is nothing to read, as Read has it.
	got, err = ReadBounded(t.TempDir(), 1)
	if err != nil || got != nil {
		t.Errorf("ReadBounded on a town with no log = %v, %v; want no events and no error", got, err)
	}
}
