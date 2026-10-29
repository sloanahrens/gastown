package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	mustWrite(t, filepath.Join(fixture, "a.txt"), "x\n")
	if _, _, err := copyFixture(fixture, filepath.Join(t.TempDir(), "missing"))(); err == nil {
		t.Error("copyFixture into a missing scratch = nil error, want an error")
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

// TestInterpretRunTurnsHowBDEndedIntoAGolden: a clean run and a non-zero exit are results; a run
// that timed out, was killed, or never started is an error and records no output.
func TestInterpretRunTurnsHowBDEndedIntoAGolden(t *testing.T) {
	t.Parallel()
	out := []byte("partial\n")
	tests := []struct {
		name     string
		runErr   error
		timedOut bool
		wantErr  string
	}{
		{"clean run", nil, false, ""},
		{"timed out", &exec.ExitError{}, true, "ran past"},
		{"killed by a signal", &exec.ExitError{}, false, "killed by a signal"},
		{"never started", errors.New(`exec: "bd": executable file not found in $PATH`), false, "not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, exit, err := interpretRun(out, tc.runErr, tc.timedOut)
			if tc.wantErr == "" {
				if err != nil || exit != 0 || string(stdout) != string(out) {
					t.Errorf("interpretRun = (%q, %d, %v), want bd's output and exit 0", stdout, exit, err)
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
