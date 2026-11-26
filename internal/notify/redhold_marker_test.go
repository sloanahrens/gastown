package notify

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/testutil"
)

// redHoldMarkerPathIn is where the marker must live: the name and directory are
// the contract PushRedHold's reader relies on.
func redHoldMarkerPathIn(townRoot string) string {
	return filepath.Join(attention.Dir(townRoot), "red_hold_push.json")
}

// A town with no marker reads as the zero marker, and a written marker reads
// back equal, owner-only, from the attention directory.
func TestRedHoldMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	root := testutil.PrivateTempDir(t)

	got, err := ReadRedHoldMarker(root)
	if err != nil {
		t.Fatalf("ReadRedHoldMarker with no marker file = %v, want the zero marker", err)
	}
	if !sameMarker(got, RedHoldMarker{}) {
		t.Errorf("ReadRedHoldMarker with no marker file = %+v, want the zero marker", got)
	}

	want := RedHoldMarker{Since: time.Date(2026, 11, 26, 9, 30, 0, 0, time.UTC),
		TextedAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)}
	if err := WriteRedHoldMarker(root, want); err != nil {
		t.Fatalf("WriteRedHoldMarker = %v", err)
	}
	got, err = ReadRedHoldMarker(root)
	if err != nil {
		t.Fatalf("ReadRedHoldMarker after write = %v", err)
	}
	if !sameMarker(got, want) {
		t.Errorf("ReadRedHoldMarker after write = %+v, want %+v", got, want)
	}
	info, err := os.Stat(redHoldMarkerPathIn(root))
	if err != nil {
		t.Fatalf("the marker is not at %s: %v", redHoldMarkerPathIn(root), err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("marker mode = %o, want 600: only its owner may read what was sent", perm)
	}
}

// A marker file that cannot be read is an error, never the zero marker, which
// would push the text and mail again.
func TestRedHoldMarkerRefuses(t *testing.T) {
	t.Parallel()
	t.Run("garbage content", func(t *testing.T) {
		t.Parallel()
		root := testutil.PrivateTempDir(t)
		if err := os.MkdirAll(filepath.Dir(redHoldMarkerPathIn(root)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(redHoldMarkerPathIn(root), []byte("not a marker\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if m, err := ReadRedHoldMarker(root); err == nil {
			t.Errorf("ReadRedHoldMarker on garbage = %+v, nil; want an error", m)
		}
	})
	t.Run("unreadable path", func(t *testing.T) {
		t.Parallel()
		root := testutil.PrivateTempDir(t)
		// A directory where the file belongs fails the read with a reason other
		// than not-exist, which must not read as a marker never written.
		if err := os.MkdirAll(redHoldMarkerPathIn(root), 0o700); err != nil {
			t.Fatal(err)
		}
		if m, err := ReadRedHoldMarker(root); err == nil {
			t.Errorf("ReadRedHoldMarker on an unreadable path = %+v, nil; want an error", m)
		}
	})
}

// A marker that cannot be written must reach the caller as an error, or the
// push it recorded is lost and sent again as if it had never been made.
func TestRedHoldMarkerWriteFails(t *testing.T) {
	t.Parallel()
	root := testutil.PrivateTempDir(t)
	// A directory where the marker belongs is a path the atomic rename cannot
	// replace, as an unwritable attention directory is on a host.
	if err := os.MkdirAll(redHoldMarkerPathIn(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteRedHoldMarker(root, RedHoldMarker{}); err == nil {
		t.Error("WriteRedHoldMarker over a directory = nil, want an error")
	}
}
