package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// exitError is a bd exit status, matched the way *exec.ExitError is. Only a real process makes an
// *exec.ExitError, and a unit test runs none, so a fake carries the code instead.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

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

// TestCopyFixtureMakesAMissingScratch: copies are made in a scratch directory that need not exist
// yet, since the copier makes it.
func TestCopyFixtureMakesAMissingScratch(t *testing.T) {
	t.Parallel()
	fixture := t.TempDir()
	mustWrite(t, filepath.Join(fixture, "a.txt"), "x\n")
	scratch := filepath.Join(t.TempDir(), "missing")

	dir, remove, err := copyFixture(fixture, scratch)()
	if err != nil {
		t.Fatalf("copyFixture into a missing scratch: %v", err)
	}
	defer func() {
		if err := remove(); err != nil {
			t.Errorf("remove: %v", err)
		}
	}()
	if got := filepath.Dir(dir); got != scratch {
		t.Errorf("copy made in %s, want the scratch directory %s", got, scratch)
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

// TestCopyFileRefusesWhatItCannotCopy: a missing source, a destination that already exists, and a
// source that cannot be read all fail the copy.
func TestCopyFileRefusesWhatItCannotCopy(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "a.txt")
	mustWrite(t, src, "x\n")
	existing := filepath.Join(t.TempDir(), "b.txt")
	mustWrite(t, existing, "already here\n")
	tests := []struct {
		name     string
		src, dst string
	}{
		{"missing source", filepath.Join(t.TempDir(), "missing.txt"), filepath.Join(t.TempDir(), "b.txt")},
		{"destination exists", src, existing},
		{"source cannot be read", t.TempDir(), filepath.Join(t.TempDir(), "b.txt")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := copyFile(tc.src, tc.dst); err == nil {
				t.Errorf("copyFile(%s) = nil error, want an error", tc.name)
			}
		})
	}
}

// TestInterpretRunTurnsHowBDEndedIntoAGolden: a clean run and a non-zero exit are results the golden
// keeps; a run that timed out, was killed, or never started is an error and records no output.
func TestInterpretRunTurnsHowBDEndedIntoAGolden(t *testing.T) {
	t.Parallel()
	out := []byte("partial\n")
	tests := []struct {
		name     string
		runErr   error
		timedOut bool
		wantExit int
		wantErr  string
	}{
		{"clean run", nil, false, 0, ""},
		{"non-zero exit", exitError(3), false, 3, ""},
		{"timed out", exitError(0), true, 0, "ran past"},
		{"killed by a signal", exitError(-1), false, 0, "killed by a signal"},
		{"never started", errors.New(`exec: "bd": executable file not found in $PATH`), false, 0, "not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, exit, err := interpretRun(out, tc.runErr, tc.timedOut)
			if tc.wantErr == "" {
				if err != nil || exit != tc.wantExit || string(stdout) != string(out) {
					t.Errorf("interpretRun = (%q, %d, %v), want bd's output and exit %d", stdout, exit, err, tc.wantExit)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("interpretRun error = %v, want one naming %q", err, tc.wantErr)
			}
			if stdout != nil {
				t.Errorf("interpretRun returned %q on failure, want no output recorded", stdout)
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
