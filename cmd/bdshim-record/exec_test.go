package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestCopyFixtureMakesAnIndependentCopy: a mutating row changes only its copy; the fixture keeps its
// files, and removing the copy leaves the fixture in place.
func TestCopyFixtureMakesAnIndependentCopy(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, ".beads", "config.yaml"), "prefix: fx\n")
	mustWrite(t, filepath.Join(fixture, ".beads", "nested", "data.txt"), "row data\n")
	scratch := t.TempDir()

	dir, remove, err := copyFixture(fixture, scratch)()
	if err != nil {
		t.Fatalf("copyFixture: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, ".beads", "nested", "data.txt")); got != "row data\n" {
		t.Errorf("copied data.txt = %q, want the fixture's contents", got)
	}
	mustWrite(t, filepath.Join(dir, ".beads", "config.yaml"), "changed by a mutating row\n")
	if got := readFile(t, filepath.Join(fixture, ".beads", "config.yaml")); got != "prefix: fx\n" {
		t.Errorf("fixture config.yaml = %q after a change in the copy, want it untouched", got)
	}
	if err := remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("copy still exists after remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture, ".beads", "config.yaml")); err != nil {
		t.Errorf("fixture gone after removing its copy: %v", err)
	}
}

// TestCopyFixtureRefusesASymlinkAndLeavesNoCopy: a symlink in the fixture could point the copy back
// at the live town, so the copy is refused and the partial copy is removed.
func TestCopyFixtureRefusesASymlinkAndLeavesNoCopy(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, ".beads", "config.yaml"), "prefix: fx\n")
	mustWrite(t, filepath.Join(fixture, "real.txt"), "data\n")
	if err := os.Symlink(filepath.Join(fixture, "real.txt"), filepath.Join(fixture, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	scratch := t.TempDir()

	_, _, err := copyFixture(fixture, scratch)()
	if err == nil || !strings.Contains(err.Error(), "neither a directory nor a regular file") {
		t.Fatalf("copyFixture over a symlink = %v, want the refusal", err)
	}
	if left := fileNames(t, scratch); len(left) != 0 {
		t.Errorf("scratch holds %v after a refused copy, want nothing left", left)
	}
}

// TestCopyFixtureFailsWhenScratchIsMissing: a copy cannot be made in a scratch directory that does
// not exist; run creates it before the recording starts.
func TestCopyFixtureFailsWhenScratchIsMissing(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, ".beads", "config.yaml"), "prefix: fx\n")
	if _, _, err := copyFixture(fixture, filepath.Join(t.TempDir(), "missing"))(); err == nil {
		t.Error("copyFixture into a missing scratch = nil error, want an error")
	}
}

// TestCopyFixtureReportsAScratchItCannotStat: a scratch path the OS cannot answer for is an error,
// not a path declared outside the fixture.
func TestCopyFixtureReportsAScratchItCannotStat(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "blocker")
	mustWrite(t, blocker, "a file\n")
	if _, _, err := copyFixture(fixture, filepath.Join(blocker, "scratch"))(); err == nil {
		t.Error("copyFixture with a scratch path under a file = nil error, want an error")
	}
}

// TestCopyFixtureRefusesAScratchAtOrInsideTheFixture: a copy made inside the fixture sits in the tree
// the next copy walks, so that walk would copy the copy into itself and never finish. The refusal
// comes before anything is created, so the fixture is left as it was.
func TestCopyFixtureRefusesAScratchAtOrInsideTheFixture(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, ".beads", "config.yaml"), "prefix: fx\n")
	tests := []struct {
		name    string
		scratch string
	}{
		{"scratch is the fixture", fixture},
		{"scratch under the fixture", filepath.Join(fixture, "scratch")},
		{"scratch deep under the fixture", filepath.Join(fixture, ".beads", "tmp")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := fileNames(t, fixture)
			if _, _, err := copyFixture(fixture, tc.scratch)(); err == nil {
				t.Fatalf("copyFixture with %s = nil error, want the refusal", tc.name)
			}
			if after := fileNames(t, fixture); !reflect.DeepEqual(before, after) {
				t.Errorf("fixture holds %v after a refused copy, want it untouched at %v", after, before)
			}
		})
	}
}

// TestCopyFixtureAcceptsAScratchBesideOrAboveTheFixture: the refusal covers the tree the walk covers,
// not the fixture's neighbours, so a sibling or a parent directory is a valid scratch.
func TestCopyFixtureAcceptsAScratchBesideOrAboveTheFixture(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	fixture := filepath.Join(parent, "fixture")
	mustWrite(t, filepath.Join(fixture, ".beads", "config.yaml"), "prefix: fx\n")
	mustWrite(t, filepath.Join(fixture, "a.txt"), "x\n")
	tests := []struct {
		name    string
		scratch string
	}{
		{"sibling of the fixture", filepath.Join(parent, "scratch")},
		{"parent of the fixture", parent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := os.MkdirAll(tc.scratch, 0o755); err != nil {
				t.Fatalf("make the scratch directory: %v", err)
			}
			dir, remove, err := copyFixture(fixture, tc.scratch)()
			if err != nil {
				t.Fatalf("copyFixture with %s: %v", tc.name, err)
			}
			if got := readFile(t, filepath.Join(dir, "a.txt")); got != "x\n" {
				t.Errorf("copied a.txt = %q, want the fixture's contents", got)
			}
			if err := remove(); err != nil {
				t.Errorf("remove: %v", err)
			}
		})
	}
}

// TestCopyFixtureFailsWhenTheFixtureIsMissing: a fixture that is not there is an error, and the copy
// made for it is removed.
func TestCopyFixtureFailsWhenTheFixtureIsMissing(t *testing.T) {
	t.Parallel()
	scratch := t.TempDir()
	if _, _, err := copyFixture(filepath.Join(t.TempDir(), "missing"), scratch)(); err == nil {
		t.Fatal("copyFixture of a missing fixture = nil error, want an error")
	}
	if left := fileNames(t, scratch); len(left) != 0 {
		t.Errorf("scratch holds %v after a failed copy, want nothing left", left)
	}
}

// TestCopyTreeCopiesARelativelyNamedFixture: an operator names the fixture relative to where they
// stand, and the copy must still hold the fixture's entries at its root. A copy that carried part of
// the fixture's own path into it would leave bd answering from a database above the copy
// (gt-p85pw.87).
func TestCopyTreeCopiesARelativelyNamedFixture(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, ".beads", "config.yaml"), "prefix: fx\n")
	mustWrite(t, filepath.Join(fixture, "notes.txt"), "row data\n")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(cwd, fixture)
	if err != nil {
		t.Fatalf("path to the fixture from %s: %v", cwd, err)
	}
	for _, src := range []string{rel, "." + string(filepath.Separator) + rel} {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			dst := t.TempDir()
			if err := copyTree(src, dst); err != nil {
				t.Fatalf("copyTree(%q): %v", src, err)
			}
			if got := readFile(t, filepath.Join(dst, ".beads", "config.yaml")); got != "prefix: fx\n" {
				t.Errorf("copy of %q holds %q at .beads/config.yaml, want the fixture's file there", src, got)
			}
			if got := readFile(t, filepath.Join(dst, "notes.txt")); got != "row data\n" {
				t.Errorf("copy of %q holds %q at notes.txt, want the fixture's file there", src, got)
			}
			if got := fileNames(t, dst); !reflect.DeepEqual(got, []string{".beads", "notes.txt"}) {
				t.Errorf("copy of %q holds %v at its root, want only the fixture's entries", src, got)
			}
		})
	}
}

// TestCopyFixtureRefusesAFixtureWithoutADatabase: bd picks a database by walking up from the copy it
// runs in, so a fixture that holds none would have the recording answered from a database above it.
// The fixture is refused before a copy is made, so scratch stays empty.
func TestCopyFixtureRefusesAFixtureWithoutADatabase(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, "notes.txt"), "row data\n")
	scratch := t.TempDir()
	_, _, err := copyFixture(fixture, scratch)()
	if err == nil || !strings.Contains(err.Error(), fixtureDatabase) {
		t.Fatalf("copyFixture of a fixture without %s = %v, want a refusal naming it", fixtureDatabase, err)
	}
	if left := fileNames(t, scratch); len(left) != 0 {
		t.Errorf("scratch holds %v after a refused copy, want nothing left", left)
	}
}

// TestCopyTreeFailsWhenTheCopyCannotBeOpened: a copy directory that is not there is an error, not a
// copy that silently holds nothing.
func TestCopyTreeFailsWhenTheCopyCannotBeOpened(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "x\n")
	if err := copyTree(src, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("copyTree into a missing copy directory = nil error, want an error")
	}
}

// TestCopyFixtureFailsWhenTheFixtureIsMissing: a walk that cannot read the fixture at all fails the copy
// instead of copying nothing and reporting success.
func TestCopyTreeFailsWhenTheFixtureIsMissing(t *testing.T) {
	t.Parallel()
	if err := copyTree(filepath.Join(t.TempDir(), "missing"), t.TempDir()); err == nil {
		t.Error("copyTree of a missing fixture = nil error, want an error")
	}
}

// TestCopyTreeFailsWhenADirectoryCannotBeMade: a file in the way of a fixture directory stops the
// copy with an error.
func TestCopyTreeFailsWhenADirectoryCannotBeMade(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "sub", "a.txt"), "x\n")
	dst := t.TempDir()
	mustWrite(t, filepath.Join(dst, "sub"), "a file where the directory goes\n")
	if err := copyTree(src, dst); err == nil {
		t.Error("copyTree with a file in the way of a directory = nil error, want an error")
	}
}

// unreadableFS is a fixture whose file stops answering mid-copy, standing in for a disk that fails
// the read a real fixture would finish.
type unreadableFS struct{}

func (unreadableFS) Open(string) (fs.File, error) { return unreadableFile{}, nil }

type unreadableFile struct{}

func (unreadableFile) Stat() (fs.FileInfo, error) { return nil, errors.New("stat: not a real file") }
func (unreadableFile) Read([]byte) (int, error)   { return 0, errors.New("read: disk stopped answering") }
func (unreadableFile) Close() error               { return nil }

// TestCopyFileRefusesWhatItCannotCopy: a missing source, a destination that already exists, and a
// source that stops answering mid-copy all fail it rather than leaving half a file in the copy.
func TestCopyFileRefusesWhatItCannotCopy(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, "b.txt"), "x\n")
	tests := []struct {
		name    string
		fixture fs.FS
		already string // a file the copy holds before the write, if any
	}{
		{"missing source", os.DirFS(t.TempDir()), ""},
		{"destination exists", os.DirFS(fixture), "b.txt"},
		{"source cannot be read", unreadableFS{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dst := t.TempDir()
			if tc.already != "" {
				mustWrite(t, filepath.Join(dst, tc.already), "already here\n")
			}
			root, err := os.OpenRoot(dst)
			if err != nil {
				t.Fatalf("open the copy: %v", err)
			}
			defer root.Close() //nolint:errcheck // a test's read of the copy, not a write whose loss matters
			if err := copyFile(root, tc.fixture, "b.txt"); err == nil {
				t.Errorf("copyFile(%s) = nil error, want an error", tc.name)
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
