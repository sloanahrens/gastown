package events

import (
	"os"
	"path/filepath"
	"strings"
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

// TestReadTailReadsAtMostItsBound: a reader that asks for a bounded tail gets
// the newest events and is told the file was not read whole, so it cannot take
// a window that begins inside the log for the log (gt-v7ptp).
func TestReadTailReadsAtMostItsBound(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, reason := range []string{"first", "second", "third"} {
		if err := LogFeedTo(townRoot, TypeWake, "gastown/polecats/shale", WakePayload("gastown", reason)); err != nil {
			t.Fatalf("LogFeedTo %s: %v", reason, err)
		}
	}
	path := filepath.Join(townRoot, EventsFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Whole file within the bound: every event, and whole true.
	got, whole, err := ReadTail(townRoot, info.Size())
	if err != nil {
		t.Fatalf("ReadTail whole: %v", err)
	}
	if !whole || len(got) != 3 {
		t.Fatalf("ReadTail whole = %d events, whole %v; want 3 and true", len(got), whole)
	}

	// A window over the last line alone: the newest event, and whole false.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lastLine := int64(len(data) - strings.LastIndex(string(data[:len(data)-1]), "\n") - 1)
	got, whole, err = ReadTail(townRoot, lastLine)
	if err != nil {
		t.Fatalf("ReadTail bounded: %v", err)
	}
	if whole {
		t.Error("whole = true for a read that started inside the file")
	}
	if len(got) != 1 || got[0].Type != TypeWake {
		t.Fatalf("ReadTail bounded = %d events (%v), want the newest one", len(got), got)
	}
	if context, _ := got[0].Payload["context"].(string); context != "third" {
		t.Errorf("newest event context = %q, want third", context)
	}

	// A log no larger than the bound is still read whole.
	got, whole, err = ReadTail(townRoot, info.Size()+1)
	if err != nil {
		t.Fatalf("ReadTail above the file's size: %v", err)
	}
	if !whole || len(got) != 3 {
		t.Errorf("ReadTail = %d events, whole %v; want 3 and true", len(got), whole)
	}
}
