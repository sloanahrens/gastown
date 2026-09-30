package feed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// feedLine returns a feed-visible sling event line for actor.
func feedLine(t *testing.T, actor string) string {
	t.Helper()
	data, err := json.Marshal(events.Event{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Source:     "gt",
		Type:       events.TypeSling,
		Actor:      actor,
		Payload:    map[string]interface{}{"bead": "gt-1", "target": "gastown/" + actor},
		Visibility: events.VisibilityFeed,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func appendTo(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// openCurator returns a curator on a fresh town whose events file holds
// history, with the events file opened the way Start opens it. Tests call
// c.poll(tail) where the running curator would tick.
func openCurator(t *testing.T, history ...string) (c *Curator, tail *events.Tail, eventsPath, feedPath string) {
	t.Helper()
	dir := t.TempDir()
	eventsPath = filepath.Join(dir, events.EventsFile)
	feedPath = filepath.Join(dir, FeedFile)
	if len(history) > 0 {
		appendTo(t, eventsPath, history...)
	}
	c = NewCurator(dir)
	tail, err := c.openTail(eventsPath)
	if err != nil {
		t.Fatalf("opening events tail: %v", err)
	}
	t.Cleanup(func() { _ = tail.Close() })
	return c, tail, eventsPath, feedPath
}

func assertFeedHas(t *testing.T, feedPath, actor string) {
	t.Helper()
	data, _ := os.ReadFile(feedPath)
	if !strings.Contains(string(data), `"actor":"`+actor+`"`) {
		t.Fatalf("feed never received actor %q; feed:\n%s", actor, data)
	}
}

func assertFeedLacks(t *testing.T, feedPath string, actors ...string) {
	t.Helper()
	data, _ := os.ReadFile(feedPath)
	for _, a := range actors {
		if strings.Contains(string(data), `"actor":"`+a+`"`) {
			t.Errorf("feed replayed history actor %q:\n%s", a, data)
		}
	}
}

// A rotator renames a rewritten events file over the path. Before claude-9jq
// the curator kept reading the old inode and stopped curating the feed until
// the next daemon restart.
func TestCurator_FollowsRenameRotation(t *testing.T) {
	t.Parallel()
	// Build each line once: feedLine stamps time.Now() to the second, and a
	// rebuilt copy straddling a second boundary would not match the anchor.
	kept := feedLine(t, "kept")
	c, tail, eventsPath, feedPath := openCurator(t, feedLine(t, "expired"), kept)

	tmp := eventsPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(kept+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, eventsPath); err != nil {
		t.Fatal(err)
	}
	c.poll(tail) // a poll sees the bare rotation
	appendTo(t, eventsPath, feedLine(t, "after-rotation"))
	c.poll(tail)

	assertFeedHas(t, feedPath, "after-rotation")
	assertFeedLacks(t, feedPath, "expired", "kept")
}

func TestCurator_FollowsTruncateInPlace(t *testing.T) {
	t.Parallel()
	c, tail, eventsPath, feedPath := openCurator(t, feedLine(t, "old1"), feedLine(t, "old2"), feedLine(t, "old3"))

	if err := os.Truncate(eventsPath, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, eventsPath, feedLine(t, "after-truncate"))
	c.poll(tail)

	assertFeedHas(t, feedPath, "after-truncate")
	assertFeedLacks(t, feedPath, "old1", "old2", "old3")
}

// A writer that opened the file before the rename can land a line in the old
// inode; the curator drains it before switching, then follows the new file.
func TestCurator_LateWriteToOldFileAfterRename(t *testing.T) {
	t.Parallel()
	history := feedLine(t, "history") // built once; see TestCurator_FollowsRenameRotation
	c, tail, eventsPath, feedPath := openCurator(t, history)

	oldWriter, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer oldWriter.Close()

	tmp := eventsPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(history+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, eventsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := oldWriter.WriteString(feedLine(t, "late-old-inode") + "\n"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, eventsPath, feedLine(t, "new-inode"))
	c.poll(tail)

	assertFeedHas(t, feedPath, "new-inode")
	assertFeedHas(t, feedPath, "late-old-inode")
	assertFeedLacks(t, feedPath, "history")
}
