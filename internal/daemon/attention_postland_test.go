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

// TestPostLandUnknownStoreSaveFailuresAreErrors: a record that cannot be
// encoded and a temporary file that cannot be written each answer an error, so
// the runner keeps the pending entry and raises the alarm again on the next
// pass (gt-gg9hv.7.1).
func TestPostLandUnknownStoreSaveFailuresAreErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	u := landworker.PostLandUnknown{Rig: attentionRig, SHA: "abc1234567890", Context: "ci / post-land (push)", State: "pending", At: now}

	t.Run("a record that cannot be encoded", func(t *testing.T) {
		t.Parallel()
		// A time outside years 0 to 9999 has no JSON form.
		bad := u
		bad.At = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		s := postLandUnknownStore{path: filepath.Join(t.TempDir(), "daemon", postLandUnknownFile)}
		if err := s.record(bad, postLandUnknownWindow); err == nil {
			t.Fatal("record of an unencodable time answered nil, want the encoding error")
		}
	})

	t.Run("a temporary file that cannot be written", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon", postLandUnknownFile)
		// A directory where the temporary file belongs makes the write fail.
		if err := os.MkdirAll(path+".tmp", 0o700); err != nil {
			t.Fatal(err)
		}
		s := postLandUnknownStore{path: path}
		if err := s.record(u, postLandUnknownWindow); err == nil {
			t.Fatal("record with a directory in the temporary file's place answered nil, want the write error")
		}
	})
}

// TestPostLandUnknownStoreNeverTreatsAnUnreadableFileAsEmpty: a store file that
// cannot be read or decoded makes record answer an error and leaves the file
// exactly as it was, so the runner keeps its pending entries and the problem
// shows in the log; once the file is fixed the next record succeeds without a
// restart (gt-gg9hv.7.1).
func TestPostLandUnknownStoreNeverTreatsAnUnreadableFileAsEmpty(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	u := landworker.PostLandUnknown{Rig: attentionRig, SHA: "abc1234567890", Context: "ci / post-land (push)", State: "pending", At: now}

	t.Run("a file that does not decode", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), postLandUnknownFile)
		const corrupt = "{not json"
		if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
			t.Fatal(err)
		}
		s := postLandUnknownStore{path: path}
		err := s.record(u, postLandUnknownWindow)
		if err == nil || !strings.Contains(err.Error(), "decoding the post-land alarm store") {
			t.Fatalf("record over a corrupt file = %v, want the decoding error", err)
		}
		if got, _ := os.ReadFile(path); string(got) != corrupt {
			t.Fatalf("the corrupt file was rewritten to %q, want it left as it was", got)
		}
		// The file is fixed: the same store picks it up on the next record.
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.record(u, postLandUnknownWindow); err != nil {
			t.Fatalf("record after the file was fixed: %v", err)
		}
		if n := len(s.snapshot(time.Time{})); n != 1 {
			t.Fatalf("records after the fix = %d, want one", n)
		}
	})

	t.Run("a file that cannot be read", func(t *testing.T) {
		t.Parallel()
		// A directory where the file belongs makes the read fail with an error
		// that is not "does not exist".
		path := filepath.Join(t.TempDir(), postLandUnknownFile)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		s := postLandUnknownStore{path: path}
		err := s.record(u, postLandUnknownWindow)
		if err == nil || !strings.Contains(err.Error(), "reading the post-land alarm store") {
			t.Fatalf("record over an unreadable path = %v, want the reading error", err)
		}
	})
}
