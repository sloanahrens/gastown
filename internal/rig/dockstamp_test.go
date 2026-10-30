package rig

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDockStampPath pins where both sides look: under the town's runtime
// directory, keyed by rig, so the daemon and `gt rig dock` cannot drift apart.
func TestDockStampPath(t *testing.T) {
	t.Parallel()
	got := DockStampPath("/town", "gastown")
	want := filepath.Join("/town", ".runtime", "rigs", "gastown", DockStampFile)
	if got != want {
		t.Errorf("DockStampPath = %q, want %q", got, want)
	}
}

// TestDockStamp_NoStampIsTheZeroTime: a town that has never docked a rig has no
// stamp, and that is not an error - it is the answer "no dock has happened".
func TestDockStamp_NoStampIsTheZeroTime(t *testing.T) {
	t.Parallel()
	got, err := DockStamp(t.TempDir(), "gastown")
	if err != nil {
		t.Fatalf("DockStamp with no stamp = %v, want nil", err)
	}
	if !got.IsZero() {
		t.Errorf("DockStamp with no stamp = %v, want the zero time", got)
	}
}

// TestTouchDockStamp_RecordsTheDockAndIsReadBack covers the writer the daemon's
// memo hangs on: the stamp lands under the runtime directory (parent
// directories included) and reads back as the time the dock happened.
func TestTouchDockStamp_RecordsTheDockAndIsReadBack(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	if err := TouchDockStamp(townRoot, "gastown", at); err != nil {
		t.Fatalf("TouchDockStamp = %v", err)
	}
	got, err := DockStamp(townRoot, "gastown")
	if err != nil {
		t.Fatalf("DockStamp = %v", err)
	}
	if !got.Equal(at) {
		t.Errorf("DockStamp = %v, want %v", got, at)
	}
}

// TestTouchDockStamp_ForRigsAreIndependent: each rig's stamp is its own file, so
// docking one rig must not force a re-read of every other rig.
func TestTouchDockStamp_ForRigsAreIndependent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	if err := TouchDockStamp(townRoot, "gastown", at); err != nil {
		t.Fatalf("TouchDockStamp = %v", err)
	}
	if got, err := DockStamp(townRoot, "beads"); err != nil || !got.IsZero() {
		t.Errorf("DockStamp for an unstamped rig = %v, %v; want the zero time, nil", got, err)
	}
}

// TestTouchDockStamp_AlwaysAdvances: two docks at the same instant must still be
// two distinguishable stamps. The daemon only drops a memo for a stamp newer
// than the one it recorded, so a second dock that reused the first one's mtime
// would be invisible to a daemon that read the bead in between.
func TestTouchDockStamp_AlwaysAdvances(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	if err := TouchDockStamp(townRoot, "gastown", at); err != nil {
		t.Fatalf("first TouchDockStamp = %v", err)
	}
	first, err := DockStamp(townRoot, "gastown")
	if err != nil {
		t.Fatalf("DockStamp = %v", err)
	}

	// Same instant, and then an earlier one - a clock stepping back between two
	// docks is the same hazard as two docks inside one tick.
	for _, when := range []time.Time{at, at.Add(-time.Hour)} {
		if err := TouchDockStamp(townRoot, "gastown", when); err != nil {
			t.Fatalf("TouchDockStamp(%v) = %v", when, err)
		}
		next, err := DockStamp(townRoot, "gastown")
		if err != nil {
			t.Fatalf("DockStamp = %v", err)
		}
		if !next.After(first) {
			t.Fatalf("stamp for a dock at %v = %v, want strictly after the previous %v", when, next, first)
		}
		first = next
	}
}

// TestDockStamp_UnreadableStampIsAnError: the daemon treats a stamp as the only
// evidence of a dock it has not read yet, so a stat that fails must be reported
// rather than read as "no dock happened" (which would keep the memo alive).
func TestDockStamp_UnreadableStampIsAnError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A regular file where the rig's runtime directory belongs: stat on the
	// stamp below it fails with ENOTDIR, which is not ErrNotExist.
	rigDir := filepath.Dir(DockStampPath(townRoot, "gastown"))
	if err := os.MkdirAll(filepath.Dir(rigDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := DockStamp(townRoot, "gastown"); err == nil {
		t.Errorf("DockStamp with an unreadable path = %v, nil; want an error", got)
	}
}
