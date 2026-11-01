package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/testdb"
)

// runLimit bounds one bd run, so a hung bd fails the recording instead of stalling it.
const runLimit = time.Minute

// childBD is every bd process a recorder starts: the rows' runs, the workspace the guard makes, and
// the read-only answers the guard proves the recording with (gt-p85pw.115, gt-p85pw.117). main wires
// it to the container every child runs in; a unit test injects a fake, since a unit test may not
// start a process.
type childBD struct {
	Run         Runner
	Env         func(dir string) []string
	Where       Resolver
	HostVersion func() ([]byte, error)
}

// whereResult turns how one bd where run ended into the workspace it named. A run that did not
// reach an exit is an error, and so is a non-zero exit other than bd's own "no workspace" answer, so
// a workspace is never read out of an answer bd did not give. "No workspace" is no workspace, not an
// error: a fresh container resolves none until bd init makes one (gt-p85pw.117).
func whereResult(stdout []byte, exit int, runErr error) (string, error) {
	if runErr != nil {
		return "", fmt.Errorf("bd where: %w", runErr)
	}
	if exit != 0 {
		if exit == noWorkspaceExit && noWorkspaceAnswer(stdout) {
			return "", nil
		}
		return "", fmt.Errorf("bd where exited %d", exit)
	}
	return workspaceFromWhere(stdout)
}

// noWorkspaceExit is the exit code bd where gives when it resolves no workspace (probed against bd 1.2.2).
const noWorkspaceExit = 1

// noWorkspaceAnswer reports whether bd where's JSON answer says bd resolved no workspace: the error
// bd names as no_beads_directory.
func noWorkspaceAnswer(stdout []byte) bool {
	var answer struct {
		Data struct {
			Error string `json:"error"`
		} `json:"data"`
	}
	return json.Unmarshal(stdout, &answer) == nil && answer.Data.Error == "no_beads_directory"
}

// workspaceFromWhere reads the workspace out of bd where's JSON answer. An answer that does not
// parse, or that names no path, is an error rather than an empty workspace, so a bd the recorder
// cannot read is refused instead of taken for one that named nothing (gt-p85pw.117).
func workspaceFromWhere(stdout []byte) (string, error) {
	var answer struct {
		Data struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &answer); err != nil {
		return "", fmt.Errorf("bd where did not answer with JSON: %w", err)
	}
	if strings.TrimSpace(answer.Data.Path) == "" {
		return "", errors.New("bd where answered with JSON that names no data.path, so the recorder cannot tell which workspace bd resolves")
	}
	return answer.Data.Path, nil
}

// beadsDirIn names the workspace beside dir, which is the one the recorder makes for a bd that runs
// there.
func beadsDirIn(dir string) string {
	return filepath.Join(dir, ".beads")
}

// exitResult turns how a bd run ended into a recording's stdout and exit code. A non-zero exit is a
// result the golden keeps; a run that timed out or never started is an error, so its partial output
// is never recorded as bd's answer.
func exitResult(stdout []byte, runErr error, timedOut bool) ([]byte, int, error) {
	if timedOut {
		return nil, 0, fmt.Errorf("bd ran past %s", runLimit)
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(runErr, &exitErr) {
		if exitErr.ExitCode() < 0 {
			return nil, 0, errors.New("bd was killed by a signal")
		}
		return stdout, exitErr.ExitCode(), nil
	}
	if runErr != nil {
		return nil, 0, runErr
	}
	return stdout, 0, nil
}

// databasePrefix starts every database a copy is provisioned with. bd refuses a new database on a
// server that already serves other databases unless the name carries a test prefix, and testdb
// holds the one list bd and gt agree on, so a copy needs no opt-in flag (gt-p85pw.92).
const databasePrefix = testdb.MintPrefix + "bdshim_record_"

// provisionFixture returns a Copier that gives each copy a database of its own: a fresh directory
// under scratch, a bd init naming a new database, and the fixture imported into it. The function
// each copy returns drops its database and removes its directory. It also returns every id the
// fixture holds, which a golden keeps as bd printed it (normalizeGolden).
//
// A fixture file that does not hold every bead of fixtureIDs is refused here, before any database
// is made: the preflight would ask bd for such a bead and record its "not found" as the answer.
func provisionFixture(run Runner, fixture, scratch string) (Copier, map[string]bool, error) {
	return provisionFixtureFrom(os.Getwd, run, fixture, scratch)
}

// provisionFixtureFrom is provisionFixture with the working directory injected, so a test drives
// the error a process whose working directory the OS cannot answer for gets.
func provisionFixtureFrom(getwd func() (string, error), run Runner, fixture, scratch string) (Copier, map[string]bool, error) {
	path, err := absolutePath(getwd, fixture)
	if err != nil {
		return nil, nil, err
	}
	held, err := checkFixtureFile(path)
	if err != nil {
		return nil, nil, err
	}
	return func() (string, func() error, error) {
		return provisionCopy(run, path, scratch)
	}, held, nil
}

// absolutePath returns path joined to the working directory when it is relative, since bd import
// runs in the copy rather than in the operator's directory.
func absolutePath(getwd func() (string, error), path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	dir, err := getwd()
	if err != nil {
		return "", fmt.Errorf("name the fixture by an absolute path: %w", err)
	}
	return filepath.Join(dir, path), nil
}

// provisionCopy makes one copy and returns it with the function that removes it.
func provisionCopy(run Runner, fixture, scratch string) (string, func() error, error) {
	name := newDatabaseName()
	dir, err := os.MkdirTemp(scratch, "fixture-")
	if err != nil {
		return "", nil, fmt.Errorf("make a copy in the scratch directory %s: %w", scratch, err)
	}
	if err := initDatabase(run, dir, name); err != nil {
		return "", nil, errors.Join(err, abandonCopy(run, dir, name))
	}
	remove := func() error {
		return errors.Join(dropDatabase(run, dir, name), os.RemoveAll(dir))
	}
	if err := importFixture(run, dir, fixture); err != nil {
		return "", nil, errors.Join(err, remove())
	}
	return dir, remove, nil
}

// abandonCopy clears a copy whose init failed, since an init can fail after it has made its database.
// The drop runs from the workspace the init left in dir. With none there is nowhere to run it from,
// so the error names the database: its name carries testdb's prefix, which a later
// `bd dolt clean-databases` removes (gt-vs7a5).
func abandonCopy(run Runner, dir, name string) error {
	var stranded error
	if _, err := os.Stat(beadsDirIn(dir)); err == nil {
		stranded = dropDatabase(run, dir, name)
	} else {
		stranded = fmt.Errorf("database %s may remain on the server: the failed init left no workspace to drop it from", name)
	}
	return errors.Join(stranded, os.RemoveAll(dir))
}

// initDatabase makes the workspace in dir with bd init, for a copy and for the scratch the guard
// checks. The flags beyond --database and --prefix keep the init off the operator's tree: the
// directory sits inside the worktree, where bd would otherwise install git hooks, and a terminal
// run would otherwise wait for an answer (gt-p85pw.92).
func initDatabase(run Runner, dir, name string) error {
	args := []string{
		"init", "--database", name, "--prefix", fixturePrefix,
		"--non-interactive", "--skip-agents", "--skip-hooks",
	}
	return expectExit(run, dir, args, "init the database "+name)
}

// fixtureInCopy is the fixture's name inside a copy. Only a child's own directory is mounted in its
// container, so the import reads a copy of the fixture the recorder puts there, never the file the
// operator named (gt-p85pw.117).
const fixtureInCopy = "fixture.jsonl"

// importFixture fills the copy's database from the fixture file, copied into the copy first. The
// copy is removed once bd has read it, so a copy outlives the import holding only the database bd
// made, never the operator's fixture data (gt-p85pw.117).
func importFixture(run Runner, dir, fixture string) error {
	path := filepath.Join(dir, fixtureInCopy)
	err := copyFile(fixture, path)
	if err != nil {
		err = fmt.Errorf("put the fixture %s in the copy: %w", fixture, err)
	} else {
		inContainer := workDir + "/" + fixtureInCopy
		err = expectExit(run, dir, []string{"import", "-i", inContainer}, "import the fixture "+filepath.Base(fixture))
	}
	return errors.Join(err, removeFixtureCopy(path))
}

// removeFixtureCopy removes the fixture the recorder put in a copy. Leaving it there is reported
// rather than shrugged off: the copy lives on for the rows, and a read of the fixture would then see
// the operator's data where bd's database should be the only thing there. A path the copy never got
// has nothing to remove (gt-p85pw.117).
func removeFixtureCopy(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the fixture copy %s: %w", path, err)
	}
	return nil
}

// copyFile copies src to dst, so a file outside the container's mounted directory reaches bd inside
// it (gt-p85pw.117).
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: the operator names the fixture on the command line
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()                                            // a read of the fixture: a close failure cannot lose the copy
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, goldenMode) //nolint:gosec // G304: dst is a file in the copy the recorder made
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return errors.Join(err, out.Close())
	}
	return out.Close()
}

// dropDatabase removes the copy's database. bd has no verb that drops one database by name, so the
// drop goes through bd sql, the only shell bd offers for server DDL; the statement names the
// database this copy minted and nothing else, and IF EXISTS keeps a repeated cleanup harmless.
func dropDatabase(run Runner, dir, name string) error {
	statement := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", name)
	return expectExit(run, dir, []string{"sql", statement}, "drop the database "+name)
}

// newDatabaseName returns a name no other copy holds, so two copies never share a store.
func newDatabaseName() string {
	return databasePrefix + strings.ToLower(rand.Text())
}

// fixtureLineMax is the longest fixture line checkFixtureFile reads. A bead's description can be
// long, and a line past this fails the read instead of being cut short.
const fixtureLineMax = 1 << 20

// checkFixtureFile refuses a fixture that is not readable, holds a line that is not a bead, or
// lacks a bead of fixtureIDs, and returns every id the fixture holds: a bead's own, and those of the
// comments and dependencies nested under it, since a golden keeps each of them as bd printed it.
func checkFixtureFile(path string) (map[string]bool, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the operator names the fixture on the command line
	if err != nil {
		return nil, fmt.Errorf("open the fixture %s: %w", path, err)
	}
	defer func() { _ = file.Close() }() // a read of the fixture: a close failure cannot lose a copy

	held := make(map[string]bool, len(fixtureIDs))
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, fixtureLineMax)
	for line := 1; scanner.Scan(); line++ {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var bead map[string]any
		if err := json.Unmarshal(text, &bead); err != nil {
			return nil, fmt.Errorf("fixture %s line %d is not a bead: %w", path, line, err)
		}
		collectIDs(bead, held)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the fixture %s: %w", path, err)
	}
	for _, id := range fixtureIDs {
		if !held[id] {
			return nil, fmt.Errorf("fixture %s holds no bead %s, so its row would record bd's \"not found\"", path, id)
		}
	}
	return held, nil
}

// collectIDs adds to held every id value the fixture line holds at any depth, so a comment's id is
// kept by a golden as well as the bead's own.
func collectIDs(value any, held map[string]bool) {
	switch v := value.(type) {
	case map[string]any:
		for key, field := range v {
			if key == "id" {
				if id, ok := field.(string); ok {
					held[id] = true
				}
			}
			collectIDs(field, held)
		}
	case []any:
		for _, item := range v {
			collectIDs(item, held)
		}
	}
}
