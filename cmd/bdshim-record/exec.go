package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testdb"
)

// runLimit bounds one bd run, so a hung bd fails the recording instead of stalling it.
const runLimit = time.Minute

// execBD returns a Runner that runs the bd binary bin in dir with the Dolt server's address passed
// through from the environment and the store selectors dropped (gt-p85pw.92).
//
// The address passes through because a fresh copy has no .beads of its own when bd init makes its
// database, so the environment is the only thing that can name the server. The selectors are
// dropped because a BEADS_DIR or BEADS_DOLT_SERVER_DATABASE inherited from the operator's shell
// would otherwise aim every row at the live store instead of the copy it runs in. bdshim.ExecRunner
// cannot serve here: it strips every BEADS_DOLT_* variable, leaving bd init with no server.
func execBD(bin string) Runner {
	return func(dir string, args []string) ([]byte, int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is the bd binary the operator names; args are the recording's fixed rows
		c.Dir = dir
		c.Env = recordingEnv(os.Environ())
		c.WaitDelay = time.Second
		var stdout bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = os.Stderr
		err := c.Run()
		return exitResult(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded))
	}
}

// storeSelectors are the variables that name the store bd works on, as a .beads directory, a
// database file, or a server database.
var storeSelectors = []string{
	"BEADS_DIR",
	"BEADS_DB",
	"BD_DB",
	"BEADS_SHARED_SERVER_DIR",
	"GT_DOLT_DATA",
	"BEADS_DOLT_SERVER_DATABASE",
}

// recordingEnv returns base without the store selectors, so bd works on the copy it runs in.
func recordingEnv(base []string) []string {
	for _, key := range storeSelectors {
		base = beads.StripEnvKey(base, key)
	}
	return base
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
// each copy returns drops its database and removes its directory.
//
// A fixture file that does not hold every bead of fixtureIDs is refused here, before any database
// is made: the preflight would ask bd for such a bead and record its "not found" as the answer.
func provisionFixture(run Runner, fixture, scratch string) (Copier, error) {
	return provisionFixtureFrom(os.Getwd, run, fixture, scratch)
}

// provisionFixtureFrom is provisionFixture with the working directory injected, so a test drives
// the error a process whose working directory the OS cannot answer for gets.
func provisionFixtureFrom(getwd func() (string, error), run Runner, fixture, scratch string) (Copier, error) {
	path, err := absolutePath(getwd, fixture)
	if err != nil {
		return nil, err
	}
	if err := checkFixtureFile(path); err != nil {
		return nil, err
	}
	return func() (string, func() error, error) {
		return provisionCopy(run, path, scratch)
	}, nil
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
		// A failed init leaves no .beads, so there is nowhere for the drop to run from. A name
		// under testdb's prefix is what lets a later `bd dolt clean-databases` remove a database
		// an init made just before it failed.
		return "", nil, errors.Join(err, os.RemoveAll(dir))
	}
	remove := func() error {
		return errors.Join(dropDatabase(run, dir, name), os.RemoveAll(dir))
	}
	if err := importFixture(run, dir, fixture); err != nil {
		return "", nil, errors.Join(err, remove())
	}
	return dir, remove, nil
}

// initDatabase makes the copy's database with bd init. The flags beyond --database and --prefix
// keep the init off the operator's tree: a copy sits inside the worktree, where bd would otherwise
// install git hooks, and a terminal run would otherwise wait for an answer (gt-p85pw.92).
func initDatabase(run Runner, dir, name string) error {
	args := []string{
		"init", "--database", name, "--prefix", fixturePrefix,
		"--non-interactive", "--skip-agents", "--skip-hooks",
	}
	return expectExit(run, dir, args, "init the database "+name)
}

// importFixture fills the copy's database from the fixture file.
func importFixture(run Runner, dir, fixture string) error {
	return expectExit(run, dir, []string{"import", "-i", fixture}, "import the fixture "+filepath.Base(fixture))
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
// lacks a bead of fixtureIDs.
func checkFixtureFile(path string) error {
	file, err := os.Open(path) //nolint:gosec // G304: the operator names the fixture on the command line
	if err != nil {
		return fmt.Errorf("open the fixture %s: %w", path, err)
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
		var bead struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(text, &bead); err != nil {
			return fmt.Errorf("fixture %s line %d is not a bead: %w", path, line, err)
		}
		held[bead.ID] = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read the fixture %s: %w", path, err)
	}
	for _, id := range fixtureIDs {
		if !held[id] {
			return fmt.Errorf("fixture %s holds no bead %s, so its row would record bd's \"not found\"", path, id)
		}
	}
	return nil
}
