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

// startCurator starts a curator on a fresh town whose events file holds
// history, and waits until the curator is tailing it.
func startCurator(t *testing.T, history ...string) (eventsPath, feedPath string) {
	t.Helper()
	dir := t.TempDir()
	eventsPath = filepath.Join(dir, events.EventsFile)
	feedPath = filepath.Join(dir, FeedFile)
	if len(history) > 0 {
		appendTo(t, eventsPath, history...)
	}
	c := NewCurator(dir)
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Stop)
	time.Sleep(100 * time.Millisecond)
	return eventsPath, feedPath
}

// waitForFeedActor waits until the feed file mentions actor.
func waitForFeedActor(t *testing.T, feedPath, actor string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(feedPath); err == nil && strings.Contains(string(data), `"actor":"`+actor+`"`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(feedPath)
	t.Fatalf("feed never received actor %q; feed:\n%s", actor, data)
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

// The KRC pruner renames a rewritten events file over the path. Before
// claude-9jq the curator kept reading the old inode and stopped curating the
// feed until the next daemon restart.
func TestCurator_FollowsRenameRotation(t *testing.T) {
	// Build each line once: feedLine stamps time.Now() to the second, and a
	// rebuilt copy straddling a second boundary would not match the anchor.
	kept := feedLine(t, "kept")
	eventsPath, feedPath := startCurator(t, feedLine(t, "expired"), kept)

	tmp := eventsPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(kept+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, eventsPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond) // let a poll see the bare rotation
	appendTo(t, eventsPath, feedLine(t, "after-rotation"))

	waitForFeedActor(t, feedPath, "after-rotation")
	assertFeedLacks(t, feedPath, "expired", "kept")
}

func TestCurator_FollowsTruncateInPlace(t *testing.T) {
	eventsPath, feedPath := startCurator(t, feedLine(t, "old1"), feedLine(t, "old2"), feedLine(t, "old3"))

	if err := os.Truncate(eventsPath, 0); err != nil {
		t.Fatal(err)
	}
	appendTo(t, eventsPath, feedLine(t, "after-truncate"))

	waitForFeedActor(t, feedPath, "after-truncate")
	assertFeedLacks(t, feedPath, "old1", "old2", "old3")
}

// A writer that opened the file before the rename can land a line in the old
// inode; the curator drains it before switching, then follows the new file.
func TestCurator_LateWriteToOldFileAfterRename(t *testing.T) {
	history := feedLine(t, "history") // built once; see TestCurator_FollowsRenameRotation
	eventsPath, feedPath := startCurator(t, history)

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

	waitForFeedActor(t, feedPath, "new-inode")
	assertFeedLacks(t, feedPath, "history")
}
