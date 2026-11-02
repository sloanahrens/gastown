package land

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadyStamp pins the stamp both sides reach: one file per rig under the
// town's runtime directory, beside the dock stamp, so a writer and the landing
// worker cannot drift apart.
func TestReadyStamp(t *testing.T) {
	t.Parallel()

	t.Run("touch then read returns the time", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

		if err := TouchReadyStamp(townRoot, "gastown", at); err != nil {
			t.Fatalf("TouchReadyStamp = %v", err)
		}
		got, err := ReadyStamp(townRoot, "gastown")
		if err != nil {
			t.Fatalf("ReadyStamp = %v", err)
		}
		if !got.Equal(at) {
			t.Errorf("ReadyStamp = %v, want %v", got, at)
		}
		want := filepath.Join(townRoot, ".runtime", "rigs", "gastown", ReadyStampFile)
		if path, err := ReadyStampPath(townRoot, "gastown"); err != nil || path != want {
			t.Errorf("ReadyStampPath = %q, %v; want %q", path, err, want)
		}
	})

	t.Run("a missing stamp is the zero time", func(t *testing.T) {
		t.Parallel()
		got, err := ReadyStamp(t.TempDir(), "gastown")
		if err != nil {
			t.Fatalf("ReadyStamp with no stamp = %v, want nil", err)
		}
		if !got.IsZero() {
			t.Errorf("ReadyStamp with no stamp = %v, want the zero time", got)
		}
	})

	t.Run("an unreadable stamp is an error naming the path", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("permission denied")
		fsys := readyStampFS{stat: func(string) (fs.FileInfo, error) { return nil, boom }}

		got, err := readyStamp("/town", "gastown", fsys)
		if !errors.Is(err, boom) {
			t.Fatalf("readyStamp = %v, want it to wrap %v", err, boom)
		}
		if !got.IsZero() {
			t.Errorf("readyStamp with a failing stat = %v, want the zero time", got)
		}
		wantPath := filepath.Join("/town", ".runtime", "rigs", "gastown", ReadyStampFile)
		if !strings.Contains(err.Error(), wantPath) {
			t.Errorf("readyStamp error = %q, want it to name %q", err, wantPath)
		}
	})

	t.Run("a rig name that could leave its directory is refused", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`, "rig name"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				townRoot := t.TempDir()
				if got, err := ReadyStampPath(townRoot, name); err == nil {
					t.Errorf("ReadyStampPath(%q) = %q, nil; want an error", name, got)
				}
				if err := TouchReadyStamp(townRoot, name, time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)); err == nil {
					t.Errorf("TouchReadyStamp(%q) = nil; want an error", name)
				}
				if got, err := ReadyStamp(townRoot, name); err == nil {
					t.Errorf("ReadyStamp(%q) = %v, nil; want an error", name, got)
				}
				entries, readErr := os.ReadDir(townRoot)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(entries) != 0 {
					t.Errorf("a refused rig name %q left %d entries in the town root, want none", name, len(entries))
				}
			})
		}
	})

	t.Run("an empty town root is refused", func(t *testing.T) {
		t.Parallel()
		if got, err := ReadyStampPath("", "gastown"); err == nil {
			t.Errorf("ReadyStampPath with no town root = %q, nil; want an error", got)
		}
		if err := TouchReadyStamp("", "gastown", time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)); err == nil {
			t.Error("TouchReadyStamp with no town root = nil; want an error")
		}
		if got, err := ReadyStamp("", "gastown"); err == nil {
			t.Errorf("ReadyStamp with no town root = %v, nil; want an error", got)
		}
	})

	t.Run("a touch reports every step it could not take", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("disk full")
		at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
		noStat := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
		noopMkdir := func(string, os.FileMode) error { return nil }
		noopWrite := func(string, []byte, os.FileMode) error { return nil }
		noopChtimes := func(string, time.Time, time.Time) error { return nil }

		cases := []struct {
			name string
			fsys readyStampFS
		}{
			{name: "the runtime directory", fsys: readyStampFS{
				stat: noStat, mkdirAll: func(string, os.FileMode) error { return boom },
				writeFile: noopWrite, chtimes: noopChtimes,
			}},
			{name: "the stamp file", fsys: readyStampFS{
				stat: noStat, mkdirAll: noopMkdir,
				writeFile: func(string, []byte, os.FileMode) error { return boom }, chtimes: noopChtimes,
			}},
			{name: "the stamp time", fsys: readyStampFS{
				stat: noStat, mkdirAll: noopMkdir,
				writeFile: noopWrite, chtimes: func(string, time.Time, time.Time) error { return boom },
			}},
		}
		for _, c := range cases {
			if err := touchReadyStamp("/town", "gastown", at, c.fsys); !errors.Is(err, boom) {
				t.Errorf("touchReadyStamp could not %s: err = %v, want it to wrap %v", c.name, err, boom)
			}
		}
	})

	t.Run("a repeated touch advances past the previous stamp", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

		if err := TouchReadyStamp(townRoot, "gastown", at); err != nil {
			t.Fatalf("first TouchReadyStamp = %v", err)
		}
		first, err := ReadyStamp(townRoot, "gastown")
		if err != nil {
			t.Fatalf("ReadyStamp = %v", err)
		}
		// The same instant, and an earlier one: a worker must see the second
		// submission's stamp whether or not its clock moved.
		for _, when := range []time.Time{at, at.Add(-time.Hour)} {
			if err := TouchReadyStamp(townRoot, "gastown", when); err != nil {
				t.Fatalf("TouchReadyStamp(%v) = %v", when, err)
			}
			next, err := ReadyStamp(townRoot, "gastown")
			if err != nil {
				t.Fatalf("ReadyStamp = %v", err)
			}
			if !next.After(first) {
				t.Fatalf("stamp for a touch at %v = %v, want strictly after %v", when, next, first)
			}
			first = next
		}
	})

	t.Run("each rig's stamp is its own", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

		if err := TouchReadyStamp(townRoot, "gastown", at); err != nil {
			t.Fatalf("TouchReadyStamp = %v", err)
		}
		if got, err := ReadyStamp(townRoot, "beads"); err != nil || !got.IsZero() {
			t.Errorf("ReadyStamp for an unstamped rig = %v, %v; want the zero time, nil", got, err)
		}
	})
}
