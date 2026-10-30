package rig

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDockStampPath pins where both sides look: under the town's runtime
// directory, keyed by rig, so the daemon and `gt rig dock` cannot drift apart.
func TestDockStampPath(t *testing.T) {
	t.Parallel()
	got, err := DockStampPath("/town", "gastown")
	if err != nil {
		t.Fatalf("DockStampPath = %v", err)
	}
	want := filepath.Join("/town", ".runtime", "rigs", "gastown", DockStampFile)
	if got != want {
		t.Errorf("DockStampPath = %q, want %q", got, want)
	}
}

// TestDockStampPath_RefusesNamesThatLeaveTheDirectory: the stamp path is built
// from a rig name, so a name that is a path segment, holds a separator, or is not
// made of plain name characters must be refused rather than joined into a file
// outside the rig's directory.
func TestDockStampPath_RefusesNamesThatLeaveTheDirectory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`, "rig name", "rig:1"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := DockStampPath("/town", name)
			if err == nil {
				t.Fatalf("DockStampPath(%q) = %q, nil; want an error", name, got)
			}
			if got != "" {
				t.Errorf("DockStampPath(%q) = %q alongside an error; want the empty string", name, got)
			}
		})
	}
}

// TestDockStamp_RefusesABadRigName is the refusal at the production entry point:
// the reader must not silently look at a path the name built.
func TestDockStamp_RefusesABadRigName(t *testing.T) {
	t.Parallel()
	if got, err := DockStamp(t.TempDir(), "../x"); err == nil {
		t.Errorf("DockStamp with a traversal name = %v, nil; want an error", got)
	}
}

// TestDockStamp_ReadFailureCarriesThePath drives the reader's failure branch
// through a stat that always fails: a stat error that is not "no such file" is an
// error naming the stamp, because the daemon reports an unreadable stamp
// differently from a rig that was never docked.
func TestDockStamp_ReadFailureCarriesThePath(t *testing.T) {
	t.Parallel()
	boom := errors.New("permission denied")
	fsys := dockStampFS{stat: func(string) (fs.FileInfo, error) { return nil, boom }}

	got, err := dockStamp("/town", "gastown", fsys)
	if !errors.Is(err, boom) {
		t.Fatalf("dockStamp = %v, want it to wrap %v", err, boom)
	}
	if !got.IsZero() {
		t.Errorf("dockStamp with a failing stat = %v, want the zero time", got)
	}
	wantPath := filepath.Join("/town", ".runtime", "rigs", "gastown", DockStampFile)
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("dockStamp error = %q, want it to name %q", err, wantPath)
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
	stampPath, err := DockStampPath(townRoot, "gastown")
	if err != nil {
		t.Fatalf("DockStampPath = %v", err)
	}
	rigDir := filepath.Dir(stampPath)
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

// TestTouchDockStamp_FailuresAreReported: each step of writing the stamp can
// fail, and none may pass silently. The dock itself has already happened by the
// time gt calls this, so an announced failure is the only signal an operator
// gets that the daemon will keep its old verdict until its window expires.
func TestTouchDockStamp_FailuresAreReported(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	boom := errors.New("disk full")
	tests := []struct {
		name    string
		fsys    dockStampFS
		wantErr string
	}{
		{
			name: "runtime directory cannot be made",
			fsys: dockStampFS{
				stat:      os.Stat,
				mkdirAll:  func(string, os.FileMode) error { return boom },
				writeFile: os.WriteFile,
				chtimes:   os.Chtimes,
			},
			wantErr: "creating the runtime directory",
		},
		{
			name: "stamp cannot be written",
			fsys: dockStampFS{
				stat:      os.Stat,
				mkdirAll:  func(string, os.FileMode) error { return nil },
				writeFile: func(string, []byte, os.FileMode) error { return boom },
				chtimes:   os.Chtimes,
			},
			wantErr: "writing the dock stamp",
		},
		{
			name: "stamp cannot be dated",
			fsys: dockStampFS{
				stat:      os.Stat,
				mkdirAll:  os.MkdirAll,
				writeFile: os.WriteFile,
				chtimes:   func(string, time.Time, time.Time) error { return boom },
			},
			wantErr: "stamping the dock file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := touchDockStamp(t.TempDir(), "gastown", at, tc.fsys)
			if !errors.Is(err, boom) {
				t.Fatalf("touchDockStamp = %v, want it to wrap %v", err, boom)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("touchDockStamp = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}
}
