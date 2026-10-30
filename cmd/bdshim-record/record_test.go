package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// echoBD answers each call with its own arguments. A list row exits 1, so a non-zero exit is kept in
// a golden; the preflight's --help calls exit 0, as bd's do for a verb it knows.
func echoBD(args []string) ([]byte, int, error) {
	exit := 0
	if len(args) > 0 && args[0] == "list" && !wantsHelp(args) {
		exit = 1
	}
	return []byte("out: " + strings.Join(args, " ") + "\n"), exit, nil
}

// wantsHelp reports whether a call is the preflight's question whether bd knows a verb.
func wantsHelp(args []string) bool {
	return len(args) > 1 && args[len(args)-1] == "--help"
}

// preflightLen is how many bd calls the preflight makes for rows, one per fixture bead and one per
// distinct verb, before the first row runs.
func preflightLen(rows []Row) int {
	return len(fixtureIDs) + len(distinctVerbs(rows))
}

// rowDirs returns the directories the rows themselves ran in, after the preflight's calls.
func rowDirs(t *testing.T, bd *fakeBD, rows []Row) []string {
	t.Helper()
	if pre := preflightLen(rows); len(bd.dirs) >= pre {
		return bd.dirs[pre:]
	}
	t.Fatalf("bd ran %d times, want at least the %d preflight calls", len(bd.dirs), preflightLen(rows))
	return nil
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

// sameCopies reports whether made and removed name the same copies, in any order: the shared copy is
// made first and removed last.
func sameCopies(made, removed []string) bool {
	a := append([]string(nil), made...)
	b := append([]string(nil), removed...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
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

// newRecorder returns a Recorder with bd and the copier faked.
func newRecorder(t *testing.T, bd *fakeBD) (Recorder, *fakeCopies) {
	t.Helper()
	copies := &fakeCopies{base: t.TempDir()}
	return Recorder{Run: bd.Run, Copy: copies.Copy}, copies
}

// TestCaptureNormalizesTheGoldenItMakes: a golden the recorder makes carries the normalized text,
// so the id bd minted, the timestamps and the revision a run decides never reach a committed golden.
func TestCaptureNormalisesTheGoldenItMakes(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		if args[0] == "create" {
			return []byte(`{"id":"fx-a1b2c3","created_at":"2026-10-30T18:59:04Z","revision":7,"owner":"bdshim-record"}`), 0, nil
		}
		return nil, 0, nil
	}}
	copies := &fakeCopies{base: t.TempDir()}
	keep := map[string]bool{fixtureID: true}
	rc := Recorder{
		Run:       bd.Run,
		Copy:      copies.Copy,
		Normalize: func(stdout []byte) []byte { return normalizeGolden(stdout, keep) },
	}
	goldens, err := rc.Capture([]Row{{Verb: "create", Flags: []string{"--json"}, Mutates: true}})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	want := `{
  "created_at": "2026-01-01T00:00:00Z",
  "id": "fx-new",
  "owner": "bdshim-record",
  "revision": 0
}
`
	if len(goldens) != 1 || string(goldens[0].Stdout) != want {
		t.Errorf("Capture = %v, want the golden normalized to:\n%s", goldens, want)
	}
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

// TestCaptureRunsNoRowInTheFixture: the read rows share one copy of the fixture and each mutating row
// runs in a copy of its own, so no row can reach the fixture; every copy is removed afterwards.
func TestCaptureRunsNoRowInTheFixture(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: echoBD}
	rc, copies := newRecorder(t, bd)
	rows := sampleRows()
	if _, err := rc.Capture(rows); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(copies.made) != 3 {
		t.Fatalf("copies made = %v, want the shared copy and one per mutating row", copies.made)
	}
	want := []string{copies.made[0], copies.made[0], copies.made[1], copies.made[2]}
	got := rowDirs(t, bd, rows)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows ran in %v, want the shared copy for read rows and a copy of its own for each mutating row: %v", got, want)
	}
	if !sameCopies(copies.made, copies.removed) {
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
	if !sameCopies(copies.made, copies.removed) {
		t.Errorf("copies removed = %v, want every copy made (%v) removed on failure", copies.removed, copies.made)
	}
}

// TestCaptureReportsAMutatingRowsCopyThatCannotBeMade: the shared copy is made, and then a mutating
// row has none to run in, so the recording fails naming the row.
func TestCaptureReportsAMutatingRowsCopyThatCannotBeMade(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: echoBD}
	rc, _ := newRecorder(t, bd)
	made := 0
	sharedCopy := rc.Copy
	rc.Copy = func() (string, func() error, error) {
		made++
		if made > 1 {
			return "", nil, errors.New("no space left on device")
		}
		return sharedCopy()
	}
	_, err := rc.Capture(sampleRows())
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("Capture with no copy for a mutating row = %v, want the failure reported", err)
	}
	if !strings.Contains(err.Error(), GoldenName(sampleRows()[2])) {
		t.Errorf("error = %v, want the mutating row it stopped at named", err)
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
	_, err := rc.Capture(sampleRows())
	if err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("Capture with a copy that will not remove = %v, want the removal failure reported", err)
	}
}

// TestCaptureChecksTheFixtureAndTheVerbsBeforeAnyRow: before the first row, bd is asked for each bead
// the fixture must hold and for each verb the table names, in the shared copy.
func TestCaptureChecksTheFixtureAndTheVerbsBeforeAnyRow(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: echoBD}
	rc, copies := newRecorder(t, bd)
	rows := []Row{
		{Verb: "show", Operands: []string{fixtureID}},
		{Verb: "show", Flags: []string{"--json"}},
		{Verb: "close", Operands: []string{fixtureID}, Mutates: true},
	}
	if _, err := rc.Capture(rows); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	want := [][]string{
		{"show", fixtureID, "--json"},
		{"show", fixtureDepID, "--json"},
		{"show", fixtureParent, "--json"},
		{"show", "--help"},
		{"close", "--help"},
	}
	if len(bd.args) != len(want)+len(rows) {
		t.Fatalf("bd ran %d times, want %d preflight calls and %d rows", len(bd.args), len(want), len(rows))
	}
	if got := bd.args[:len(want)]; !reflect.DeepEqual(got, want) {
		t.Errorf("preflight ran %v, want %v", got, want)
	}
	for _, dir := range bd.dirs[:len(want)] {
		if dir != copies.made[0] {
			t.Errorf("preflight ran in %s, want the shared copy %s", dir, copies.made[0])
		}
	}
}

// TestCaptureRefusesAFixtureMissingABead: a fixture that does not hold a bead the rows name fails the
// recording, since the row would otherwise record bd's "not found" as its answer.
func TestCaptureRefusesAFixtureMissingABead(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		if len(args) == 3 && args[0] == "show" && args[2] == "--json" && args[1] == fixtureDepID {
			return nil, 1, nil
		}
		return echoBD(args)
	}}
	rc, copies := newRecorder(t, bd)
	_, err := rc.Capture(sampleRows())
	if err == nil || !strings.Contains(err.Error(), fixtureDepID) {
		t.Fatalf("Capture on a fixture missing %s = %v, want an error naming it", fixtureDepID, err)
	}
	want := [][]string{
		{"show", fixtureID, "--json"},
		{"show", fixtureDepID, "--json"},
	}
	if !reflect.DeepEqual(bd.args, want) {
		t.Errorf("bd ran %v, want the check of the fixture to stop at the missing bead: %v", bd.args, want)
	}
	if !sameCopies(copies.made, copies.removed) {
		t.Errorf("copies removed = %v, want every copy made (%v) removed after the refusal", copies.removed, copies.made)
	}
}

// TestCaptureRefusesAVerbBDDoesNotKnow: a row whose verb bd refuses fails the recording, naming the
// verb, before any row is recorded.
func TestCaptureRefusesAVerbBDDoesNotKnow(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		if args[len(args)-1] == "--help" && args[0] == "frobnicate" {
			return nil, 1, nil
		}
		return echoBD(args)
	}}
	rc, _ := newRecorder(t, bd)
	rows := []Row{
		{Verb: "show", Operands: []string{fixtureID}},
		{Verb: "frobnicate", Operands: []string{fixtureID}},
	}
	if _, err := rc.Capture(rows); err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("Capture with a verb bd does not know = %v, want an error naming the verb", err)
	}
	for _, args := range bd.args {
		if args[0] == "frobnicate" && !wantsHelp(args) {
			t.Errorf("bd ran the row %v, want the recording refused at the preflight", args)
		}
	}
}

// TestCaptureReportsABeadCheckThatDoesNotRun: a check that cannot run at all is an error, not a
// fixture that passed.
func TestCaptureReportsABeadCheckThatDoesNotRun(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		return nil, 0, errors.New("exec: bd not found")
	}}
	rc, _ := newRecorder(t, bd)
	if _, err := rc.Capture(sampleRows()); err == nil {
		t.Fatal("Capture with a preflight that cannot run = nil error, want an error")
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

// TestRemoveDroppedDeletesTheGoldensOfRowsTheTableNoLongerHas: a dropped row leaves its two files
// behind, and the trim deletes them and names them, so the directory holds only what this table
// recorded.
func TestRemoveDroppedDeletesTheGoldensOfRowsTheTableNoLongerHas(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	dropped := Row{Verb: "count", Flags: []string{"--by-status"}}
	wider, err := rc.Capture(append(sampleRows(), dropped))
	if err != nil {
		t.Fatalf("Capture of the wider table: %v", err)
	}
	out := t.TempDir()
	if err := Write(out, wider); err != nil {
		t.Fatalf("Write of the wider table: %v", err)
	}
	if err := os.Remove(filepath.Join(out, GoldenName(dropped)+".json")); err != nil {
		t.Fatalf("remove one file of the dropped golden: %v", err)
	}

	narrower, err := rc.Capture(sampleRows())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := Write(out, narrower); err != nil {
		t.Fatalf("Write: %v", err)
	}
	droppedNames, err := RemoveDropped(out, narrower)
	if err != nil {
		t.Fatalf("RemoveDropped: %v", err)
	}
	if want := []string{GoldenName(dropped)}; !reflect.DeepEqual(droppedNames, want) {
		t.Errorf("RemoveDropped removed %v, want the dropped row's golden %v", droppedNames, want)
	}
	wantFiles := []string{
		"close-reason-done.exit", "close-reason-done.json",
		"list-status-open.exit", "list-status-open.json",
		"reopen.exit", "reopen.json",
		"show-json.exit", "show-json.json",
	}
	if got := fileNames(t, out); !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("files left = %v, want only this table's %v", got, wantFiles)
	}
}

// TestRemoveDroppedReportsWhatItCannotRemoveOrList: a goldens directory that cannot be listed, and a
// golden file that cannot be deleted, both fail the trim instead of leaving half a golden behind.
func TestRemoveDroppedReportsWhatItCannotRemoveOrList(t *testing.T) {
	t.Parallel()
	t.Run("directory that cannot be listed", func(t *testing.T) {
		t.Parallel()
		out := filepath.Join(t.TempDir(), "golden")
		mustWrite(t, out, "a file where the directory goes\n")
		if _, err := RemoveDropped(out, nil); err == nil {
			t.Error("RemoveDropped over a directory that is a file = nil error, want an error")
		}
	})
	t.Run("golden that cannot be removed", func(t *testing.T) {
		t.Parallel()
		out := t.TempDir()
		mustWrite(t, filepath.Join(out, "gone.exit"), "0\n")
		mustWrite(t, filepath.Join(out, "gone.json", "kept.txt"), "a directory where the golden's stdout goes\n")
		if _, err := RemoveDropped(out, nil); err == nil {
			t.Error("RemoveDropped over a golden that cannot be removed = nil error, want an error")
		}
	})
}

// TestOrphansReportsEachDroppedRowOnce: a golden whose row is gone is one golden whether one or both
// of its files are left, and a file that is not part of a golden is not one.
func TestOrphansReportsEachDroppedRowOnce(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	mustWrite(t, filepath.Join(out, "gone.json"), "{}\n")
	mustWrite(t, filepath.Join(out, "gone.exit"), "0\n")
	mustWrite(t, filepath.Join(out, "README.md"), "how the goldens are recorded\n")
	mustMkdir(t, filepath.Join(out, ".git"))
	kept := []Golden{{Name: "kept", Stdout: []byte("{}\n"), Exit: 0}}
	mustWrite(t, filepath.Join(out, "kept.json"), "{}\n")

	orphans, err := Orphans(out, kept)
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if want := []string{"gone"}; !reflect.DeepEqual(orphans, want) {
		t.Errorf("Orphans = %v, want %v", orphans, want)
	}
}

// TestOrphansFailsClosedOnADirectoryItCannotList: a goldens directory that cannot be listed is an
// error, not an empty set of orphans that a clean report would hide.
func TestOrphansFailsClosedOnADirectoryItCannotList(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "golden")
	mustWrite(t, out, "a file where the directory goes\n")
	if _, err := Orphans(out, nil); err == nil {
		t.Error("Orphans of a directory that is a file = nil error, want an error")
	}
}

// TestOrphansOfAMissingDirectoryIsNone: nothing has been recorded yet, so there is nothing to report.
func TestOrphansOfAMissingDirectoryIsNone(t *testing.T) {
	t.Parallel()
	orphans, err := Orphans(filepath.Join(t.TempDir(), "missing"), nil)
	if err != nil || len(orphans) != 0 {
		t.Errorf("Orphans of a missing directory = (%v, %v), want nothing and no error", orphans, err)
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

// TestExecuteCheckReportsTheGoldensOfRowsTheTableNoLongerHas: -check lists the golden of a dropped
// row, says so, exits 1, and leaves the directory as it was.
func TestExecuteCheckReportsTheGoldensOfRowsTheTableNoLongerHas(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	out := t.TempDir()
	dropped := Row{Verb: "count", Flags: []string{"--by-status"}}
	wider, err := rc.Capture(append(sampleRows(), dropped))
	if err != nil {
		t.Fatalf("Capture of the wider table: %v", err)
	}
	if err := Write(out, wider); err != nil {
		t.Fatalf("Write of the wider table: %v", err)
	}
	before := snapshot(t, out)

	var stdout, stderr bytes.Buffer
	if code := execute(options{out: out, check: true}, rc, sampleRows(), &stdout, &stderr); code != 1 {
		t.Errorf("-check with a dropped row = %d, want 1", code)
	}
	if got, want := stdout.String(), GoldenName(dropped)+"\n"; got != want {
		t.Errorf("-check printed %q, want the dropped row's golden %q", got, want)
	}
	if !strings.Contains(stderr.String(), "no longer has") {
		t.Errorf("stderr = %q, want the dropped golden reported", stderr.String())
	}
	if after := snapshot(t, out); !reflect.DeepEqual(before, after) {
		t.Errorf("-check changed the goldens directory: before %v, after %v", before, after)
	}
}

// TestExecuteRecordWritesTheGoldens: without -check the goldens are written, the golden of a row the
// table no longer has is removed and reported, and the command exits 0.
func TestExecuteRecordWritesTheGoldens(t *testing.T) {
	t.Parallel()
	rc, _ := newRecorder(t, &fakeBD{out: echoBD})
	out := t.TempDir()
	mustWrite(t, filepath.Join(out, "dropped-row.json"), "{}\n")
	mustWrite(t, filepath.Join(out, "dropped-row.exit"), "0\n")
	var stdout, stderr bytes.Buffer
	if code := execute(options{out: out}, rc, sampleRows(), &stdout, &stderr); code != 0 {
		t.Fatalf("record = %d, stderr %q; want 0", code, stderr.String())
	}
	if got := len(fileNames(t, out)); got != 2*len(sampleRows()) {
		t.Errorf("files written = %d, want two per row (%d)", got, 2*len(sampleRows()))
	}
	if !strings.Contains(stderr.String(), "no longer has") {
		t.Errorf("stderr = %q, want the removed golden reported", stderr.String())
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

// TestCaptureReportsACopyThatCannotBeMade: with no copy of the fixture there is nowhere for a row to
// run, so the recording fails before bd runs at all.
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
	if len(bd.args) != 0 {
		t.Errorf("bd ran %d times with no copy to run in, want 0", len(bd.args))
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
