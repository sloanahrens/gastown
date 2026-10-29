package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeBD stands in for bd. It answers each call from out and records the directory and the
// arguments of every call, in order.
type fakeBD struct {
	out  func(args []string) ([]byte, int, error)
	dirs []string
	args [][]string
}

func (f *fakeBD) Run(dir string, args []string) ([]byte, int, error) {
	f.dirs = append(f.dirs, dir)
	f.args = append(f.args, args)
	return f.out(args)
}

// echoBD answers each call with its own arguments, and exits 1 for list so a non-zero exit is kept.
func echoBD(args []string) ([]byte, int, error) {
	exit := 0
	if len(args) > 0 && args[0] == "list" {
		exit = 1
	}
	return []byte("out: " + strings.Join(args, " ") + "\n"), exit, nil
}

// fakeCopies makes fixture copies as empty directories under base, and records which were made and
// which were removed.
type fakeCopies struct {
	base    string
	made    []string
	removed []string
}

func (c *fakeCopies) Copy() (string, func() error, error) {
	dir, err := os.MkdirTemp(c.base, "copy-")
	if err != nil {
		return "", nil, err
	}
	c.made = append(c.made, dir)
	return dir, func() error {
		c.removed = append(c.removed, dir)
		return os.RemoveAll(dir)
	}, nil
}

// sampleRows has two read rows and two mutating rows.
func sampleRows() []Row {
	return []Row{
		{Verb: "show", Flags: []string{"--json"}, Operands: []string{"fx-1"}},
		{Verb: "list", Flags: []string{"--status", "open"}},
		{Verb: "close", Flags: []string{"--reason", "done"}, Operands: []string{"fx-1"}, Mutates: true},
		{Verb: "reopen", Operands: []string{"fx-1"}, Mutates: true},
	}
}

// newRecorder returns a Recorder over a fresh fixture directory, with bd and the copier faked.
func newRecorder(t *testing.T, bd *fakeBD) (Recorder, *fakeCopies) {
	t.Helper()
	copies := &fakeCopies{base: t.TempDir()}
	return Recorder{Fixture: t.TempDir(), Run: bd.Run, Copy: copies.Copy}, copies
}

// TestCaptureRefusesCollidingGoldenNames: two rows that would write the same golden are refused
// before bd runs, so no run is spent on a table that would overwrite itself.
func TestCaptureRefusesCollidingGoldenNames(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: echoBD}
	rc, _ := newRecorder(t, bd)
	rows := []Row{
		{Verb: "show", Operands: []string{"fx-1"}},
		{Verb: "show", Operands: []string{"fx-2"}},
	}
	if _, err := rc.Capture(rows); err == nil {
		t.Fatal("Capture of two rows with one golden name = nil error, want a refusal")
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %d times for a refused table, want 0", len(bd.args))
	}
}

// TestCaptureRunsReadRowsInTheFixtureAndMutatingRowsInFreshCopies: a read row runs where the fixture
// is; each mutating row runs in its own copy, and every copy is removed.
func TestCaptureRunsReadRowsInTheFixtureAndMutatingRowsInFreshCopies(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: echoBD}
	rc, copies := newRecorder(t, bd)
	if _, err := rc.Capture(sampleRows()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	want := []string{rc.Fixture, rc.Fixture, copies.made[0], copies.made[1]}
	if !reflect.DeepEqual(bd.dirs, want) {
		t.Errorf("bd ran in %v, want the fixture for read rows and a copy for mutating rows: %v", bd.dirs, want)
	}
	if len(copies.made) != 2 || copies.made[0] == copies.made[1] {
		t.Errorf("copies made = %v, want two distinct copies", copies.made)
	}
	if !reflect.DeepEqual(copies.removed, copies.made) {
		t.Errorf("copies removed = %v, want every copy made (%v) removed", copies.removed, copies.made)
	}
}

// TestCaptureFailsWhenBDDoesNotRunAndRemovesItsCopy: a bd that does not run is an error, not a
// golden with an empty stdout, and the copy it was given is still removed.
func TestCaptureFailsWhenBDDoesNotRunAndRemovesItsCopy(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		if args[0] == "reopen" {
			return nil, 0, errors.New("exec: bd not found")
		}
		return echoBD(args)
	}}
	rc, copies := newRecorder(t, bd)
	goldens, err := rc.Capture(sampleRows())
	if err == nil {
		t.Fatalf("Capture with bd not running = %v, nil error; want an error", goldens)
	}
	if goldens != nil {
		t.Errorf("Capture returned %d goldens on failure, want none", len(goldens))
	}
	if !reflect.DeepEqual(copies.removed, copies.made) {
		t.Errorf("copies removed = %v, want every copy made (%v) removed on failure", copies.removed, copies.made)
	}
}

// TestCaptureReportsACopyThatWillNotRemove: a copy that cannot be removed fails the recording, so a
// leftover fixture is never reported as a clean run.
func TestCaptureReportsACopyThatWillNotRemove(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	rc.Copy = func() (string, func() error, error) {
		return t.TempDir(), func() error { return errors.New("device busy") }, nil
	}
	if _, err := rc.Capture(sampleRows()); err == nil {
		t.Fatal("Capture with a copy that will not remove = nil error, want an error")
	}
}

// TestWriteOneGoldenAndExitFilePerRow: a recorded table writes two files per row, the stdout as
// <name>.json and the exit code as <name>.exit.
func TestWriteOneGoldenAndExitFilePerRow(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: echoBD}
	rc, _ := newRecorder(t, bd)
	goldens, err := rc.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	out := t.TempDir()
	if err := Write(out, goldens); err != nil {
		t.Fatalf("Write: %v", err)
	}
	wantFiles := []string{
		"close-reason-done.exit", "close-reason-done.json",
		"list-status-open.exit", "list-status-open.json",
		"reopen.exit", "reopen.json",
		"show-json.exit", "show-json.json",
	}
	if got := fileNames(t, out); !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("files written = %v, want %v", got, wantFiles)
	}
	if got := readFile(t, filepath.Join(out, "show-json.json")); got != "out: show --json fx-1\n" {
		t.Errorf("show-json.json = %q, want bd's stdout", got)
	}
	if got := readFile(t, filepath.Join(out, "show-json.exit")); got != "0\n" {
		t.Errorf("show-json.exit = %q, want \"0\\n\"", got)
	}
	if got := readFile(t, filepath.Join(out, "list-status-open.exit")); got != "1\n" {
		t.Errorf("list-status-open.exit = %q, want the non-zero exit kept as \"1\\n\"", got)
	}
}

// TestCheckListsOnlyTheGoldensThatChanged: with -check, a golden whose stdout changed and a golden
// whose file is gone are listed in table order, the unchanged ones are not, and nothing is written.
func TestCheckListsOnlyTheGoldensThatChanged(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	goldens, err := rc.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	out := t.TempDir()
	if err := Write(out, goldens); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.Remove(filepath.Join(out, "reopen.json")); err != nil {
		t.Fatalf("remove golden: %v", err)
	}
	before := snapshot(t, out)

	drifted := func(args []string) ([]byte, int, error) {
		if args[0] == "close" {
			return []byte("bd changed its close output\n"), 0, nil
		}
		return echoBD(args)
	}
	rc2, _ := newRecorder(t, &fakeBD{out: drifted})
	changed, err := rc2.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture with drift: %v", err)
	}
	differ, err := Differ(out, changed)
	if err != nil {
		t.Fatalf("Differ: %v", err)
	}
	if want := []string{"close-reason-done", "reopen"}; !reflect.DeepEqual(differ, want) {
		t.Errorf("Differ = %v, want %v", differ, want)
	}
	if after := snapshot(t, out); !reflect.DeepEqual(before, after) {
		t.Errorf("-check changed the goldens directory: before %v, after %v", before, after)
	}
}

// TestExecuteCheckPrintsDriftAndExitsNonZero: -check prints each drifted name on stdout and exits 1;
// with no drift it prints nothing and exits 0.
func TestExecuteCheckPrintsDriftAndExitsNonZero(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	goldens, err := rc.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	out := t.TempDir()
	if err := Write(out, goldens); err != nil {
		t.Fatalf("Write: %v", err)
	}
	opts := options{out: out, check: true}

	var stdout, stderr bytes.Buffer
	if code := execute(opts, rc, sampleRows(), &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Errorf("clean -check = %d with stdout %q, want 0 and nothing printed", code, stdout.String())
	}

	drifted, _ := newRecorder(t, &fakeBD{out: func(args []string) ([]byte, int, error) {
		if args[0] == "show" {
			return []byte("drifted\n"), 0, nil
		}
		return echoBD(args)
	}})
	stdout.Reset()
	stderr.Reset()
	if code := execute(opts, drifted, sampleRows(), &stdout, &stderr); code != 1 {
		t.Errorf("-check with drift = %d, want 1", code)
	}
	if got := stdout.String(); got != "show-json\n" {
		t.Errorf("-check printed %q, want the drifted golden name only", got)
	}
}

// TestExecuteRecordWritesTheGoldens: without -check the goldens are written and the command exits 0.
func TestExecuteRecordWritesTheGoldens(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := execute(options{out: out}, rc, sampleRows(), &stdout, &stderr); code != 0 {
		t.Fatalf("record = %d, stderr %q; want 0", code, stderr.String())
	}
	if got := len(fileNames(t, out)); got != 2*len(sampleRows()) {
		t.Errorf("files written = %d, want two per row (%d)", got, 2*len(sampleRows()))
	}
}

// TestDifferFailsClosedOnAGoldenItCannotRead: a golden that exists but cannot be read is an error,
// not a difference that a clean-looking report would hide.
func TestDifferFailsClosedOnAGoldenItCannotRead(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	goldens, err := rc.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	out := t.TempDir()
	// A directory where the golden file should be is present but not readable as a file.
	if err := os.Mkdir(filepath.Join(out, "show-json.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := Differ(out, goldens); err == nil {
		t.Error("Differ over an unreadable golden = nil error, want an error")
	}
}

// TestCaptureReportsACopyThatCannotBeMade: a mutating row that has no copy to run in fails the
// recording, and bd is not run for it.
func TestCaptureReportsACopyThatCannotBeMade(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: echoBD}
	rc, _ := newRecorder(t, bd)
	rc.Copy = func() (string, func() error, error) {
		return "", nil, errors.New("no space left on device")
	}
	if _, err := rc.Capture(sampleRows()); err == nil {
		t.Fatal("Capture with no fixture copy = nil error, want an error")
	}
	if len(bd.args) != 2 {
		t.Errorf("bd ran %d times, want only the 2 read rows before the copy failed", len(bd.args))
	}
}

// TestWriteFailsWhenAGoldenCannotBeWritten: a golden directory that cannot be made, or a golden file
// that cannot be written, fails the write instead of leaving a partial set.
func TestWriteFailsWhenAGoldenCannotBeWritten(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	goldens, err := rc.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, out string) string
	}{
		{"directory under a file", func(t *testing.T, out string) string {
			mustWrite(t, filepath.Join(out, "blocker"), "a file\n")
			return filepath.Join(out, "blocker", "golden")
		}},
		{"json file blocked by a directory", func(t *testing.T, out string) string {
			mustMkdir(t, filepath.Join(out, "show-json.json"))
			return out
		}},
		{"exit file blocked by a directory", func(t *testing.T, out string) string {
			mustMkdir(t, filepath.Join(out, "show-json.exit"))
			return out
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := tc.setup(t, t.TempDir())
			if err := Write(dir, goldens); err == nil {
				t.Errorf("Write(%s) = nil error, want an error", tc.name)
			}
		})
	}
}

// TestDifferFailsClosedOnAnUnreadableExitFile: an exit file that exists but cannot be read is an
// error, the same as an unreadable stdout file.
func TestDifferFailsClosedOnAnUnreadableExitFile(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	goldens, err := rc.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	out := t.TempDir()
	mustWrite(t, filepath.Join(out, "show-json.json"), "out: show --json fx-1\n")
	mustMkdir(t, filepath.Join(out, "show-json.exit"))
	if _, err := Differ(out, goldens); err == nil {
		t.Error("Differ over an unreadable exit file = nil error, want an error")
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// snapshot maps each file in dir to its contents.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := make(map[string]string)
	for _, name := range fileNames(t, dir) {
		got[name] = readFile(t, filepath.Join(dir, name))
	}
	return got
}
