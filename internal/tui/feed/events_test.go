package feed

import (
	"encoding/json"
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
