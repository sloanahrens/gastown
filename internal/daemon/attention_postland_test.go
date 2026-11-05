package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/landworker"
)

func TestCollectPostLandUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	recorded := landworker.PostLandUnknown{
		Rig: attentionRig, SHA: "abc1234567890", Context: "ci / post-land (push)",
		State: "pending", At: now.Add(-time.Hour),
	}
	f.d.postLandUnknowns.record(recorded, postLandUnknownWindow)
	// A record a window older than the new one is dropped from the store.
	old := recorded
	old.SHA = "0lder"
	old.At = now.Add(-8 * 24 * time.Hour)
	f.d.postLandUnknowns.record(old, postLandUnknownWindow)
	// A second timeout for the same commit replaces its record: one item.
	recorded.State, recorded.At = "missing", now.Add(-30*time.Minute)
	f.d.postLandUnknowns.record(recorded, postLandUnknownWindow)

	// The old record is gone from the store itself: the collector would filter
	// an 8 day old record by its own window anyway, so only reading the store
	// proves the pruning.
	stored := f.d.postLandUnknowns.snapshot(time.Time{})
	if len(stored) != 1 || stored[0].Key() != recorded.Key() {
		t.Fatalf("store holds %+v, want only the newest record", stored)
	}

	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one per landed commit", items)
	}
	it := items[0]
	if it.Key != recorded.Key() || it.Kind != attention.KindPostLandUnknown ||
		it.Severity != attention.SeverityLow || it.Rig != attentionRig || it.SHA != recorded.SHA {
		t.Errorf("item = %+v", it)
	}
	if !strings.Contains(it.Summary, recorded.Context) || !strings.Contains(it.Summary, "missing") {
		t.Errorf("summary %q does not name the context and last state", it.Summary)
	}
}

// A record past the window stops raising, so an old silence does not stay in
// the queue forever; the townhealth reading is the backstop long before then.
func TestCollectPostLandUnknownSkipsRecordsPastTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.d.postLandUnknowns.record(landworker.PostLandUnknown{
		Rig: attentionRig, SHA: "abc1234567890", Context: "ci / post-land (push)",
		State: "pending", At: now.Add(-8 * 24 * time.Hour),
	}, 30*24*time.Hour) // kept by the store, filtered by the collector

	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none past the window", items)
	}
}

// TestPostLandUnknownStoreSurvivesARestart: record answers nil only once the
// file is written, a store built over the same path after a restart still holds
// the record, and a path that cannot be written answers an error so the runner
// keeps the pending entry (gt-gg9hv.7.1).
func TestPostLandUnknownStoreSurvivesARestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	u := landworker.PostLandUnknown{Rig: attentionRig, SHA: "abc1234567890", Context: "ci / post-land (push)", State: "pending", At: now}
	path := filepath.Join(t.TempDir(), "daemon", postLandUnknownFile)

	first := postLandUnknownStore{path: path}
	if err := first.record(u, postLandUnknownWindow); err != nil {
		t.Fatalf("record: %v", err)
	}
	restarted := postLandUnknownStore{path: path}
	got := restarted.snapshot(time.Time{})
	if len(got) != 1 || got[0].Key() != u.Key() || got[0].State != "pending" {
		t.Fatalf("restarted store = %+v, want the one record", got)
	}
	// The same key raised again after the restart replaces it: still one item.
	if err := restarted.record(u, postLandUnknownWindow); err != nil {
		t.Fatalf("second record: %v", err)
	}
	if n := len(restarted.snapshot(time.Time{})); n != 1 {
		t.Fatalf("records after a repeat = %d, want one", n)
	}

	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := postLandUnknownStore{path: filepath.Join(blocked, "sub", postLandUnknownFile)}
	if err := broken.record(u, postLandUnknownWindow); err == nil {
		t.Fatal("record under an unwritable path answered nil, want the error that keeps the pending entry")
	}
}
