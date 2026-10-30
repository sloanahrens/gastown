package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testdb"
)

// exitError is bd's exit code as the error exec.ExitError reports it.
type exitError int

func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// TestExitResultKeepsAnExitAndRefusesARunThatNeverFinished: a non-zero exit is bd's answer, while a
// run that timed out, was killed, or never started is an error. A unit test may not start bd, so
// this drives the converter execBD hands each run's ending to.
func TestExitResultKeepsAnExitAndRefusesARunThatNeverFinished(t *testing.T) {
	t.Parallel()
	stdout := []byte("bd's answer\n")
	tests := []struct {
		name     string
		runErr   error
		timedOut bool
		wantExit int
		wantErr  string
	}{
		{"ran to exit 0", nil, false, 0, ""},
		{"ran to a non-zero exit", exitError(13), false, 13, ""},
		{"ran past the limit", exitError(0), true, 0, "ran past"},
		{"killed by a signal", exitError(-1), false, 0, "signal"},
		{"never started", errors.New("exec: bd: not found"), false, 0, "not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, exit, err := exitResult(stdout, tc.runErr, tc.timedOut)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("exitResult(%s) = %v, want an error naming %q", tc.name, err, tc.wantErr)
				}
				if out != nil || exit != 0 {
					t.Errorf("exitResult(%s) = (%q, %d) with an error, want nothing and 0", tc.name, out, exit)
				}
				return
			}
			if err != nil || exit != tc.wantExit || !bytes.Equal(out, stdout) {
				t.Errorf("exitResult(%s) = (%q, %d, %v), want bd's stdout, exit %d and no error", tc.name, out, exit, err, tc.wantExit)
			}
		})
	}
}

// TestRecordingEnvPinsTheIdentityAndDropsTheStoreSelectors: every bd run sees the recorder's actor
// and git identity in place of the operator's, so a golden's owner and created_by read the same on
// every machine, while the store selectors still go so bd works on the copy it runs in.
func TestRecordingEnvPinsTheIdentityAndDropsTheStoreSelectors(t *testing.T) {
	t.Parallel()
	env := recordingEnv([]string{
		"PATH=/usr/bin",
		"BEADS_DOLT_SERVER_PORT=3307",
		"BEADS_DIR=/home/operator/gt/.beads",
		"BEADS_DOLT_SERVER_DATABASE=hq",
		"BEADS_ACTOR=operator@example.com",
		"BD_ACTOR=operator",
		"GIT_AUTHOR_NAME=Operator",
		"GIT_AUTHOR_EMAIL=operator@example.com",
		"GIT_COMMITTER_NAME=Operator",
		"GIT_COMMITTER_EMAIL=operator@example.com",
	})
	got := envEntries(env)
	want := map[string]string{
		"BEADS_ACTOR":            "bdshim-record",
		"BD_ACTOR":               "bdshim-record",
		"GIT_AUTHOR_NAME":        "bdshim-record",
		"GIT_AUTHOR_EMAIL":       "bdshim-record@example.invalid",
		"GIT_COMMITTER_NAME":     "bdshim-record",
		"GIT_COMMITTER_EMAIL":    "bdshim-record@example.invalid",
		"PATH":                   "/usr/bin",
		"BEADS_DOLT_SERVER_PORT": "3307", // the server address still passes through
	}
	for key, value := range want {
		if values := got[key]; len(values) != 1 || values[0] != value {
			t.Errorf("%s = %v, want exactly [%q]", key, values, value)
		}
	}
	for _, key := range []string{"BEADS_DIR", "BEADS_DOLT_SERVER_DATABASE"} {
		if values := got[key]; len(values) != 0 {
			t.Errorf("%s = %v, want the store selector dropped", key, values)
		}
	}
}

// envEntries returns each variable's values in the order the environment holds them.
func envEntries(env []string) map[string][]string {
	entries := make(map[string][]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		entries[key] = append(entries[key], value)
	}
	return entries
}

// okBD answers every call with exit 0.
func okBD([]string) ([]byte, int, error) { return nil, 0, nil }

// okRun answers every bd call with exit 0.
func okRun(string, []string) ([]byte, int, error) { return nil, 0, nil }

// mustWrite writes content to path, making its directory.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeFixture writes a JSONL fixture holding one bead per id and returns its path.
func writeFixture(t *testing.T, ids ...string) string {
	t.Helper()
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "{\"id\":%q,\"title\":%q}\n", id, "bead "+id)
	}
	path := filepath.Join(t.TempDir(), "fixture.jsonl")
	mustWrite(t, path, b.String())
	return path
}

// initName returns the database a recorded bd init named.
func initName(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "--database" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("bd ran %v with no --database", args)
	return ""
}

// callIndices returns the indices of the calls whose first word is verb.
func callIndices(bd *fakeBD, verb string) []int {
	var found []int
	for i, args := range bd.args {
		if args[0] == verb {
			found = append(found, i)
		}
	}
	return found
}

// TestProvisionFixtureMakesADatabaseOfItsOwn: a copy is a fresh directory under scratch holding a
// database bd init made under a test name, with the fixture imported into it, so no two copies can
// share a store.
func TestProvisionFixtureMakesADatabaseOfItsOwn(t *testing.T) {
	t.Parallel()
	fixture := writeFixture(t, fixtureIDs...)
	scratch := t.TempDir()
	bd := &fakeBD{out: okBD}
	provision, _, err := provisionFixture(bd.Run, fixture, scratch)
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	dir, _, err := provision()
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if filepath.Dir(dir) != scratch {
		t.Errorf("copy made at %s, want it under the scratch directory %s", dir, scratch)
	}
	if len(bd.args) != 2 {
		t.Fatalf("bd ran %v, want the copy's init and import", bd.args)
	}
	name := initName(t, bd.args[0])
	wantInit := []string{"init", "--database", name, "--prefix", fixturePrefix, "--non-interactive", "--skip-agents", "--skip-hooks"}
	if !reflect.DeepEqual(bd.args[0], wantInit) {
		t.Errorf("init ran %v, want %v", bd.args[0], wantInit)
	}
	if !strings.HasPrefix(name, testdb.MintPrefix) {
		t.Errorf("database %q named, want one under the test prefix %q", name, testdb.MintPrefix)
	}
	if want := []string{"import", "-i", fixture}; !reflect.DeepEqual(bd.args[1], want) {
		t.Errorf("import ran %v, want %v", bd.args[1], want)
	}
	for i, got := range bd.dirs {
		if got != dir {
			t.Errorf("call %v ran in %s, want the copy %s", bd.args[i], got, dir)
		}
	}
}

// TestProvisionFixtureGivesEachCopyADifferentDatabase: two copies never share a database, so a
// mutating row in one cannot change what another reads.
func TestProvisionFixtureGivesEachCopyADifferentDatabase(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: okBD}
	provision, _, err := provisionFixture(bd.Run, writeFixture(t, fixtureIDs...), t.TempDir())
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	var names []string
	for range 2 {
		if _, _, err := provision(); err != nil {
			t.Fatalf("provision: %v", err)
		}
	}
	for _, i := range callIndices(bd, "init") {
		names = append(names, initName(t, bd.args[i]))
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Errorf("copies were named %v, want a different database for each", names)
	}
}

// TestProvisionFixtureDropsTheDatabaseAndRemovesTheDirectory: the removal drops the database the
// copy's init made, then removes the directory.
func TestProvisionFixtureDropsTheDatabaseAndRemovesTheDirectory(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: okBD}
	provision, _, err := provisionFixture(bd.Run, writeFixture(t, fixtureIDs...), t.TempDir())
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	dir, remove, err := provision()
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	name := initName(t, bd.args[0])
	if err := remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	drops := callIndices(bd, "sql")
	if len(drops) != 1 {
		t.Fatalf("bd ran %v, want one drop", bd.args)
	}
	want := []string{"sql", "DROP DATABASE IF EXISTS `" + name + "`"}
	if got := bd.args[drops[0]]; !reflect.DeepEqual(got, want) {
		t.Errorf("drop ran %v, want %v", got, want)
	}
	if got := bd.dirs[drops[0]]; got != dir {
		t.Errorf("drop ran in %s, want the copy %s", got, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("copy still exists after remove: %v", err)
	}
}

// TestProvisionFixtureReportsAFailedInit: a copy whose database bd could not make fails, imports
// nothing, and leaves no directory behind.
func TestProvisionFixtureReportsAFailedInit(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		if args[0] == "init" {
			return nil, 1, nil
		}
		return nil, 0, nil
	}}
	scratch := t.TempDir()
	provision, _, err := provisionFixture(bd.Run, writeFixture(t, fixtureIDs...), scratch)
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	_, _, err = provision()
	if err == nil || !strings.Contains(err.Error(), "init the database") {
		t.Fatalf("provision over a failed init = %v, want the init named", err)
	}
	if imports := callIndices(bd, "import"); len(imports) != 0 {
		t.Errorf("bd ran %v, want no import over a database that was never made", bd.args)
	}
	if left := fileNames(t, scratch); len(left) != 0 {
		t.Errorf("scratch holds %v after a failed copy, want nothing left", left)
	}
}

// TestProvisionFixtureReportsAFailedImportAndDropsTheDatabase: a copy the import could not fill is
// an error, and its database is dropped so a failed run leaves nothing on the server.
func TestProvisionFixtureReportsAFailedImportAndDropsTheDatabase(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		if args[0] == "import" {
			return nil, 3, nil
		}
		return nil, 0, nil
	}}
	scratch := t.TempDir()
	provision, _, err := provisionFixture(bd.Run, writeFixture(t, fixtureIDs...), scratch)
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	dir, _, err := provision()
	if err == nil || !strings.Contains(err.Error(), "import the fixture") {
		t.Fatalf("provision over a failed import = %v, want the import named", err)
	}
	if drops := callIndices(bd, "sql"); len(drops) != 1 {
		t.Errorf("bd ran %v, want the copy's database dropped", bd.args)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Errorf("copy %s still exists after a failed import", dir)
	}
}

// TestProvisionFixtureReportsACleanupThatCannotComplete: a drop that fails is reported, so a
// database left on the server is never a clean run.
func TestProvisionFixtureReportsACleanupThatCannotComplete(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
		if args[0] == "sql" {
			return nil, 1, nil
		}
		return nil, 0, nil
	}}
	provision, _, err := provisionFixture(bd.Run, writeFixture(t, fixtureIDs...), t.TempDir())
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	dir, remove, err := provision()
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	err = remove()
	if err == nil || !strings.Contains(err.Error(), "drop the database") {
		t.Fatalf("remove over a failed drop = %v, want the drop named", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("copy still exists after remove: %v", statErr)
	}
}

// TestProvisionFixtureRefusesAFixtureMissingABead: a fixture file that lacks a bead the table names
// fails before any database is made, since its row would record bd's "not found" as the answer.
func TestProvisionFixtureRefusesAFixtureMissingABead(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: okBD}
	fixture := writeFixture(t, fixtureID, fixtureParent)
	_, _, err := provisionFixture(bd.Run, fixture, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), fixtureDepID) {
		t.Fatalf("provisionFixture of a fixture missing %s = %v, want an error naming it", fixtureDepID, err)
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v for a refused fixture, want nothing run", bd.args)
	}
}

// TestProvisionFixtureRefusesALineThatIsNotABead: a fixture line that is not JSON fails the
// recording and names the line, since bd import would answer with its own error instead.
func TestProvisionFixtureRefusesALineThatIsNotABead(t *testing.T) {
	t.Parallel()
	fixture := writeFixture(t, fixtureIDs...)
	mustWrite(t, fixture, "{\"id\":\"fx-1\"}\nnot a bead\n")
	_, _, err := provisionFixture(okRun, fixture, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("provisionFixture over a line that is not a bead = %v, want the line named", err)
	}
}

// TestProvisionFixtureReportsAScratchItCannotMake: a copy needs its directory under scratch, and
// when the scratch path cannot hold one the run stops naming the directory.
func TestProvisionFixtureReportsAScratchItCannotMake(t *testing.T) {
	t.Parallel()
	provision, _, err := provisionFixture(okRun, writeFixture(t, fixtureIDs...), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	if _, _, err := provision(); err == nil || !strings.Contains(err.Error(), "scratch directory") {
		t.Fatalf("provision with a missing scratch directory = %v, want an error naming it", err)
	}
}

// TestProvisionFixtureReportsAFixtureItCannotRead: a fixture path that opens but cannot be read is
// an error, not a fixture holding no beads.
func TestProvisionFixtureReportsAFixtureItCannotRead(t *testing.T) {
	t.Parallel()
	// A directory opens as a file but fails the first read, on Linux and macOS alike.
	_, _, err := provisionFixture(okRun, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read the fixture") {
		t.Fatalf("provisionFixture of a fixture that cannot be read = %v, want an error naming the read", err)
	}
}

// TestProvisionFixtureReportsAWorkingDirectoryItCannotRead: a relative fixture is resolved against
// the working directory, and a process the OS will not name one for fails rather than guessing.
func TestProvisionFixtureReportsAWorkingDirectoryItCannotRead(t *testing.T) {
	t.Parallel()
	getwd := func() (string, error) { return "", errors.New("getwd: i/o error") }
	_, _, err := provisionFixtureFrom(getwd, okRun, "fixture.jsonl", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "i/o error") {
		t.Fatalf("provisionFixtureFrom without a working directory = %v, want the getwd failure reported", err)
	}
}

// TestProvisionFixtureRefusesAMissingFixture: a fixture that is not there is an error, not a run
// over an empty database.
func TestProvisionFixtureRefusesAMissingFixture(t *testing.T) {
	t.Parallel()
	_, _, err := provisionFixture(okRun, filepath.Join(t.TempDir(), "missing.jsonl"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "open the fixture") {
		t.Fatalf("provisionFixture of a missing fixture = %v, want an error naming it", err)
	}
}

// TestProvisionFixtureNamesTheFixtureOutsideItsWorkingDirectory: bd import runs in the copy, so the
// fixture has to reach it by a path that does not depend on the operator's working directory.
func TestProvisionFixtureNamesTheFixtureOutsideItsWorkingDirectory(t *testing.T) {
	t.Parallel()
	abs := writeFixture(t, fixtureIDs...)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		t.Fatalf("path to the fixture from %s: %v", cwd, err)
	}
	if rel == abs {
		t.Fatalf("the fixture %s is named absolutely already", abs)
	}
	bd := &fakeBD{out: okBD}
	provision, _, err := provisionFixture(bd.Run, rel, t.TempDir())
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	if _, _, err := provision(); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if want := []string{"import", "-i", abs}; !reflect.DeepEqual(bd.args[1], want) {
		t.Errorf("import ran %v, want the fixture named absolutely: %v", bd.args[1], want)
	}
}

// TestCaptureGivesEachMutatingRowItsOwnDatabase: with the provisioner in place, the read rows share
// one copy and each mutating row gets a database of its own.
func TestCaptureGivesEachMutatingRowItsOwnDatabase(t *testing.T) {
	t.Parallel()
	fixture := writeFixture(t, fixtureIDs...)
	bd := &fakeBD{out: echoBD}
	provision, _, err := provisionFixture(bd.Run, fixture, t.TempDir())
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	rows := sampleRows()
	if _, err := (Recorder{Run: bd.Run, Copy: provision}).Capture(rows); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	inits := callIndices(bd, "init")
	if len(inits) != 3 {
		t.Fatalf("bd ran %d inits, want the shared copy and one per mutating row", len(inits))
	}
	shared := bd.dirs[inits[0]]
	for _, row := range rows[:2] {
		if got := rowDir(t, bd, row); got != shared {
			t.Errorf("read row %v ran in %s, want the shared copy %s", row.Args(), got, shared)
		}
	}
	first, second := bd.dirs[inits[1]], bd.dirs[inits[2]]
	if first == shared || second == shared || first == second {
		t.Fatalf("mutating rows ran in %s and %s, want a copy of their own, apart from %s", first, second, shared)
	}
	if got := rowDir(t, bd, rows[2]); got != first {
		t.Errorf("mutating row %v ran in %s, want its own copy %s", rows[2].Args(), got, first)
	}
	if got := rowDir(t, bd, rows[3]); got != second {
		t.Errorf("mutating row %v ran in %s, want its own copy %s", rows[3].Args(), got, second)
	}
	for i, call := range callIndices(bd, "import") {
		if got := bd.dirs[call]; got != bd.dirs[inits[i]] {
			t.Errorf("import %d ran in %s, want the copy its init made in %s", i, got, bd.dirs[inits[i]])
		}
	}
}

// TestCaptureDropsEveryDatabaseAfterTheRows: every copy a recording made is dropped and removed
// once the rows that ran in it are done, so a recording leaves nothing on the server.
func TestCaptureDropsEveryDatabaseAfterTheRows(t *testing.T) {
	t.Parallel()
	fixture := writeFixture(t, fixtureIDs...)
	bd := &fakeBD{out: echoBD}
	provision, _, err := provisionFixture(bd.Run, fixture, t.TempDir())
	if err != nil {
		t.Fatalf("provisionFixture: %v", err)
	}
	rows := sampleRows()
	if _, err := (Recorder{Run: bd.Run, Copy: provision}).Capture(rows); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	inits := callIndices(bd, "init")
	drops := callIndices(bd, "sql")
	if len(drops) != len(inits) {
		t.Fatalf("bd ran %d drops, want one per copy made (%d)", len(drops), len(inits))
	}
	dropped := make(map[string]bool, len(drops))
	for _, call := range drops {
		want := dbNameIn(bd.args[call])
		dropped[want] = true
		dir := bd.dirs[call]
		if dir != bd.dirs[initFor(t, bd, want)] {
			t.Errorf("drop of %s ran in %s, want the copy its init made", want, dir)
		}
		for _, row := range rows {
			if at := callIndex(t, bd, row.Args()); bd.dirs[at] == dir && at > call {
				t.Errorf("drop of %s ran at %d, before the row %v it ran in that copy (%s)", want, call, row.Args(), dir)
			}
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("copy of %s still exists after Capture: %v", want, err)
		}
	}
	for _, call := range inits {
		if name := initName(t, bd.args[call]); !dropped[name] {
			t.Errorf("database %s was never dropped", name)
		}
	}
}

// fixturePath is the fixture file the recording is made against, as the operator names it from the
// command's directory.
var fixturePath = filepath.Join("..", "bdshim", "testdata", "fixture.jsonl")

// fixtureBead is the part of a fixture line the golden table and the preflight rely on.
type fixtureBead struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
	Deps   []struct {
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	} `json:"dependencies"`
	Comments []struct {
		Text string `json:"text"`
	} `json:"comments"`
}

// readFixture parses every line of the fixture file as one bead.
func readFixture(t *testing.T) map[string]fixtureBead {
	t.Helper()
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	beads := make(map[string]fixtureBead)
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var bead fixtureBead
		if err := json.Unmarshal([]byte(line), &bead); err != nil {
			t.Fatalf("fixture line %d is not a bead: %v", i+1, err)
		}
		beads[bead.ID] = bead
	}
	return beads
}

// TestFixtureHoldsWhatTheTableAsksFor: every line of the fixture is a bead, and the fixture answers
// the rows that name a bead or filter on one: the preflight's ids, a label, an agent bead, one
// open bead, one closed one, a comment and a dependency.
func TestFixtureHoldsWhatTheTableAsksFor(t *testing.T) {
	t.Parallel()
	beads := readFixture(t)
	for _, id := range fixtureIDs {
		if _, ok := beads[id]; !ok {
			t.Errorf("fixture holds no bead %s, which the preflight asks bd for", id)
		}
	}
	var open, closed, agent bool
	for _, bead := range beads {
		switch bead.Status {
		case "open":
			open = true
		case "closed":
			closed = true
		}
		for _, label := range bead.Labels {
			if label == "gt:agent" {
				agent = true
			}
		}
	}
	if !open || !closed {
		t.Errorf("fixture holds open=%v closed=%v, want one bead of each so a list and a count differ", open, closed)
	}
	if !agent {
		t.Error("fixture holds no bead labeled gt:agent, so the label-filtered list row has no answer")
	}
	named := beads[fixtureID]
	if len(named.Comments) == 0 {
		t.Errorf("fixture bead %s carries no comment, so the comments row has no answer", fixtureID)
	}
	if len(named.Deps) == 0 {
		t.Errorf("fixture bead %s carries no dependency, so the dep list row has no answer", fixtureID)
	}
}

// dbNameIn returns the database name a recorded drop names.
func dbNameIn(args []string) string {
	statement := args[len(args)-1]
	_, name, _ := strings.Cut(statement, "`")
	name, _, _ = strings.Cut(name, "`")
	return name
}

// initFor returns the index of the init that made database name.
func initFor(t *testing.T, bd *fakeBD, name string) int {
	t.Helper()
	for _, call := range callIndices(bd, "init") {
		if initName(t, bd.args[call]) == name {
			return call
		}
	}
	t.Fatalf("no init made database %s", name)
	return 0
}

// callIndex returns the index of the one call that ran args.
func callIndex(t *testing.T, bd *fakeBD, args []string) int {
	t.Helper()
	for i, got := range bd.args {
		if reflect.DeepEqual(got, args) {
			return i
		}
	}
	t.Fatalf("bd never ran %v", args)
	return 0
}

// rowDir returns the directory the one call that ran row's arguments ran in.
func rowDir(t *testing.T, bd *fakeBD, row Row) string {
	t.Helper()
	return bd.dirs[callIndex(t, bd, row.Args())]
}
