package feed

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGtEventsSource_TailPicksUpEventsAppendedAfterFirstPoll reproduces gt-1su:
// the feed TUI's GtEventsSource silently stops seeing new events shortly after
// startup. bufio.Scanner latches an internal error (including io.EOF) the
// first time Scan() returns false and will never return true again on that
// scanner instance, even after more data is appended to the underlying file.
// tail() reused a single scanner across every poll tick, so once the file's
// initial EOF was hit (near-certain on the very first 100ms tick, since no
// writer is racing to append within that window), every later append was
// silently dropped forever - exactly the "stale from the start" behavior
// reported against the live TUI. print_events.go's PrintGtEvents (the
// --plain path) avoids this by constructing a fresh scanner each poll tick;
// GtEventsSource.tail must do the same.
func TestGtEventsSource_TailPicksUpEventsAppendedAfterFirstPoll(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, ".events.jsonl")

	now := time.Now()
	initial, _ := json.Marshal(GtEvent{
		Timestamp: now.Format(time.RFC3339), Source: "test", Type: "create",
		Actor: "a", Visibility: "feed", Payload: map[string]interface{}{"message": "initial"},
	})
	if err := os.WriteFile(eventsPath, append(initial, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	src, err := NewGtEventsSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	// Drain the initial backlog event emitted by loadRecentEvents.
	select {
	case <-src.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial backlog event")
	}

	// Let the tail loop's ticker fire several times against an unchanged
	// file so it latches onto EOF before we append anything new - this is
	// what happens in practice between TUI startup and the next real event.
	time.Sleep(500 * time.Millisecond)

	appended, _ := json.Marshal(GtEvent{
		Timestamp: now.Add(time.Second).Format(time.RFC3339), Source: "test", Type: "sling",
		Actor: "b", Visibility: "feed", Payload: map[string]interface{}{"bead": "gt-1", "target": "p1"},
	})
	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(appended, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	select {
	case e := <-src.Events():
		if e.Message == "" || e.Type != "sling" {
			t.Errorf("expected the appended sling event, got: %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("appended event was never picked up by the tail loop (sticky bufio.Scanner EOF)")
	}
}

// TestGtEventsSource_LoadRecentEventsOffsetSurvivesRaceWithWriter reproduces
// gt-0qu: a line written to .events.jsonl in the narrow window between
// loadRecentEvents() finishing its backlog scan and tail() resuming polling
// must still be picked up, never silently dropped forever.
//
// loadRecentEvents scans the whole file up to whatever was on disk when its
// scan completed, then returns. If the caller resumed tailing by seeking to
// the file's end *at that later point* (`Seek(0, io.SeekEnd)`), any line a
// concurrent writer appended in between - after the scan's read position,
// before the reseek - would land strictly between the two and be skipped by
// both: already past what the backlog scan consumed, and behind where the
// tail scanner jumps to. loadRecentEvents must instead return the exact byte
// offset where its own scan stopped, so the caller resumes from there.
func TestGtEventsSource_LoadRecentEventsOffsetSurvivesRaceWithWriter(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, ".events.jsonl")

	now := time.Now()
	initial, _ := json.Marshal(GtEvent{
		Timestamp: now.Format(time.RFC3339), Source: "test", Type: "create",
		Actor: "a", Visibility: "feed", Payload: map[string]interface{}{"message": "initial"},
	})
	if err := os.WriteFile(eventsPath, append(initial, '\n'), 0644); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	src := &GtEventsSource{file: file, events: make(chan Event, 200)}
	offset := src.loadRecentEvents()

	// Drain the backlog event loadRecentEvents just emitted.
	select {
	case <-src.events:
	default:
		t.Fatal("expected the initial backlog event to be queued")
	}

	// Simulate a writer appending a new line in the race window between the
	// scan completing and the caller reseeking - i.e. right here, after
	// loadRecentEvents has already returned.
	raced, _ := json.Marshal(GtEvent{
		Timestamp: now.Add(time.Second).Format(time.RFC3339), Source: "test", Type: "sling",
		Actor: "b", Visibility: "feed", Payload: map[string]interface{}{"bead": "gt-1", "target": "p1"},
	})
	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(raced, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// The buggy approach: reseek to "current end of file" now that the race
	// window has closed. This must NOT be where tailing resumes from.
	buggyEOF, _ := file.Seek(0, io.SeekEnd)
	if buggyEOF == offset {
		t.Fatal("test setup broken: the raced write did not advance the file past loadRecentEvents' offset")
	}

	// The fix: resume from the offset loadRecentEvents reported, not from
	// the file's current end. The raced line must still be readable from
	// there.
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		t.Fatal("expected the raced line to still be readable from loadRecentEvents' returned offset, but got EOF")
	}
	line := scanner.Text()
	if event := parseGtEventLine(line); event == nil || event.Type != "sling" {
		t.Errorf("expected the raced sling event at the returned offset, got line: %q", line)
	}
}
