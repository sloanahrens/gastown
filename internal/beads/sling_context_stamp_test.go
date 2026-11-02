package beads

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// stampFS is the real filesystem with every operation a test may need to fail
// left replaceable.
func stampFS() slingContextStampFS {
	return slingContextStampFS{
		stat:      os.Stat,
		mkdirAll:  os.MkdirAll,
		writeFile: os.WriteFile,
		chtimes:   os.Chtimes,
		readFile:  os.ReadFile,
		remove:    os.Remove,
	}
}

// TestSlingContextStamp pins the reader and writer the dispatch heartbeat's
// skip rests on: a touched stamp reads back at the time of the creation, a town
// that never scheduled a context has none (the zero time, not an error), and a
// stamp that exists but cannot be read is an error rather than the zero time,
// which would be read as "nothing was created".
func TestSlingContextStamp(t *testing.T) {
	t.Parallel()
	// Whole seconds keep the comparison clear of filesystem timestamp
	// precision, as the dock stamp's test does.
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	t.Run("touch then read returns the touch time", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := TouchSlingContextStamp(townRoot, at); err != nil {
			t.Fatalf("TouchSlingContextStamp = %v", err)
		}
		got, err := slingContextStamp(townRoot, osSlingContextStampFS)
		if err != nil {
			t.Fatalf("SlingContextStamp = %v", err)
		}
		if !got.Equal(at) {
			t.Errorf("SlingContextStamp = %v, want %v", got, at)
		}
		// The file lives under the runtime directory the scan reads, so the two
		// sides cannot drift apart.
		want := filepath.Join(townRoot, constants.DirRuntime, SlingContextStampFile)
		if _, err := os.Stat(want); err != nil {
			t.Errorf("stamp not at %s: %v", want, err)
		}
	})

	t.Run("a missing stamp is the zero time", func(t *testing.T) {
		t.Parallel()
		got, err := slingContextStamp(t.TempDir(), osSlingContextStampFS)
		if err != nil {
			t.Fatalf("SlingContextStamp with no stamp = %v, want nil", err)
		}
		if !got.IsZero() {
			t.Errorf("SlingContextStamp with no stamp = %v, want the zero time", got)
		}
	})

	t.Run("an unreadable stamp is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("permission denied")
		fsys := stampFS()
		fsys.stat = func(string) (fs.FileInfo, error) { return nil, boom }

		got, err := slingContextStamp("/town", fsys)
		if !errors.Is(err, boom) {
			t.Fatalf("slingContextStamp = %v, want it to wrap %v", err, boom)
		}
		if !got.IsZero() {
			t.Errorf("slingContextStamp with a failing stat = %v, want the zero time", got)
		}
		if want := SlingContextStampPath("/town"); !strings.Contains(err.Error(), want) {
			t.Errorf("slingContextStamp error = %q, want it to name %q", err, want)
		}
	})

	t.Run("two touches at one instant stay distinguishable", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := TouchSlingContextStamp(townRoot, at); err != nil {
			t.Fatalf("first TouchSlingContextStamp = %v", err)
		}
		// A second creation at the same instant, and one after a clock that
		// stepped back, must each leave a stamp newer than the one before: a
		// stamp that ties or retreats could sit at or before an empty scan and
		// hide the creation.
		previous, err := slingContextStamp(townRoot, osSlingContextStampFS)
		if err != nil {
			t.Fatalf("SlingContextStamp = %v", err)
		}
		for _, when := range []time.Time{at, at.Add(-time.Hour)} {
			if err := TouchSlingContextStamp(townRoot, when); err != nil {
				t.Fatalf("TouchSlingContextStamp(%v) = %v", when, err)
			}
			got, err := slingContextStamp(townRoot, osSlingContextStampFS)
			if err != nil {
				t.Fatalf("SlingContextStamp = %v", err)
			}
			if !got.After(previous) {
				t.Fatalf("stamp for a creation at %v = %v, want strictly after the previous %v", when, got, previous)
			}
			previous = got
		}
	})
}

// TestSlingContextScanMarker pins the mark the skip reads: a full scan's start
// and whether it found a context, the zero value when no scan is on record, and
// an error for a marker that cannot be read. Content this binary does not
// recognize counts as "found", so a marker it cannot understand never orders a
// skip.
func TestSlingContextScanMarker(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	t.Run("no marker is the zero value", func(t *testing.T) {
		t.Parallel()
		got, err := slingContextScanMarker(t.TempDir(), osSlingContextStampFS)
		if err != nil {
			t.Fatalf("SlingContextScanMarker with no marker = %v, want nil", err)
		}
		if !got.StartedAt.IsZero() || got.Found {
			t.Errorf("SlingContextScanMarker with no marker = %+v, want the zero value", got)
		}
	})

	for _, found := range []bool{false, true} {
		found := found
		name := "empty"
		if found {
			name = "found"
		}
		t.Run("round trip records "+name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if err := RecordSlingContextScan(townRoot, at, found); err != nil {
				t.Fatalf("RecordSlingContextScan = %v", err)
			}
			got, err := slingContextScanMarker(townRoot, osSlingContextStampFS)
			if err != nil {
				t.Fatalf("SlingContextScanMarker = %v", err)
			}
			if !got.StartedAt.Equal(at) {
				t.Errorf("StartedAt = %v, want the scan start %v", got.StartedAt, at)
			}
			if got.Found != found {
				t.Errorf("Found = %v, want %v", got.Found, found)
			}
		})
	}

	t.Run("an unreadable marker is an error", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("permission denied")
		fsys := stampFS()
		fsys.stat = func(string) (fs.FileInfo, error) { return nil, boom }

		if _, err := slingContextScanMarker("/town", fsys); !errors.Is(err, boom) {
			t.Fatalf("slingContextScanMarker = %v, want it to wrap %v", err, boom)
		}
	})

	t.Run("a marker whose content cannot be read is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := RecordSlingContextScan(townRoot, at, false); err != nil {
			t.Fatalf("seed marker: %v", err)
		}
		boom := errors.New("i/o error")
		fsys := stampFS()
		fsys.readFile = func(string) ([]byte, error) { return nil, boom }

		if _, err := slingContextScanMarker(townRoot, fsys); !errors.Is(err, boom) {
			t.Fatalf("slingContextScanMarker = %v, want it to wrap %v", err, boom)
		}
	})

	t.Run("unrecognized content reads as found", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(SlingContextScanMarkerPath(townRoot)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(SlingContextScanMarkerPath(townRoot), []byte("a newer gt's marker\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := slingContextScanMarker(townRoot, osSlingContextStampFS)
		if err != nil {
			t.Fatalf("SlingContextScanMarker = %v", err)
		}
		if !got.Found {
			t.Error("a marker this binary does not understand read as empty; it must read as found so the scan is not skipped")
		}
	})
}

// TestTouchSlingContextStampDropsTheEmptyScanMarkerOnFailure: a creation whose
// stamp cannot be written must not leave an empty-scan marker standing, because
// that marker would hide the new context until it aged out (gt-66e5d). The
// write failure is still reported to the caller.
func TestTouchSlingContextStampDropsTheEmptyScanMarkerOnFailure(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	boom := errors.New("disk full")

	tests := []struct {
		name       string
		remove     func(string) error
		wantErr    string
		wantMarker bool
	}{
		{
			name:    "stamp write fails",
			remove:  os.Remove,
			wantErr: "writing the sling-context stamp",
		},
		{
			name:       "stamp write fails and the marker cannot be dropped",
			remove:     func(string) error { return errors.New("read-only directory") },
			wantErr:    "could not be removed",
			wantMarker: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if err := RecordSlingContextScan(townRoot, at.Add(-time.Minute), false); err != nil {
				t.Fatalf("seed empty-scan marker: %v", err)
			}
			fsys := stampFS()
			fsys.writeFile = func(string, []byte, os.FileMode) error { return boom }
			fsys.remove = tc.remove

			err := touchSlingContextStamp(townRoot, at, fsys)
			if !errors.Is(err, boom) {
				t.Fatalf("touchSlingContextStamp = %v, want it to wrap %v", err, boom)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("touchSlingContextStamp error = %q, want it to name %q", err, tc.wantErr)
			}
			_, statErr := os.Stat(SlingContextScanMarkerPath(townRoot))
			wantGone := !tc.wantMarker
			if gone := errors.Is(statErr, fs.ErrNotExist); gone != wantGone {
				t.Errorf("empty-scan marker gone = %v, want %v (stat: %v): a marker that outlives a stamp it never got would hide the new context", gone, wantGone, statErr)
			}
		})
	}
}
