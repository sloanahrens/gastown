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

// childBD is every bd process a recorder starts: the rows' runs, the workspace the guard makes, and
// the read-only answer the guard resolves (gt-p85pw.115). main wires it to the binary -bd names; a
// unit test injects a fake, since a unit test may not start a process.
type childBD struct {
	Run   Runner
	Env   func(dir string) []string
	Where Resolver
}

// execChild returns the child bd the binary bin names, every call carrying the recording
// environment built from base.
func execChild(bin string) childBD {
	base := os.Environ()
	return childBD{
		Run:   execBD(bin, base),
		Env:   func(dir string) []string { return childEnv(base, dir) },
		Where: whereBD(bin),
	}
}

// execBD returns a Runner that runs the bd binary bin in dir with the Dolt server's address passed
// through from base and the workspace beside dir pinned (gt-p85pw.92, gt-p85pw.115).
//
// The address passes through because a fresh copy has no .beads of its own when bd init makes its
// database, so the environment is the only thing that can name the server. bdshim.ExecRunner cannot
// serve here: it strips every BEADS_DOLT_* variable, leaving bd init with no server.
func execBD(bin string, base []string) Runner {
	return func(dir string, args []string) ([]byte, int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c := bdCommand(ctx, bin, dir, args, base)
		var stdout bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = os.Stderr
		err := c.Run()
		return exitResult(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded))
	}
}

// whereBD returns a Resolver that asks bd which workspace it resolves in dir: bd where --json names
// the active beads location, which is the one read-only answer the guard trusts (gt-p85pw.115).
func whereBD(bin string) Resolver {
	return func(dir string, env []string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c := bdCommandWith(ctx, bin, dir, []string{"where", "--json"}, env)
		var stdout bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = os.Stderr
		err := c.Run()
		return whereResult(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded))
	}
}

// whereResult turns how the bd where run ended into the workspace it named. A run that timed out or
// never finished is an error, so a workspace is never read out of an answer bd did not give.
func whereResult(stdout []byte, runErr error, timedOut bool) (string, error) {
	if timedOut {
		return "", fmt.Errorf("bd where ran past %s", runLimit)
	}
	if runErr != nil {
		return "", fmt.Errorf("bd where: %w", runErr)
	}
	return workspaceFromWhere(stdout)
}

// workspaceFromWhere reads the workspace out of bd where's JSON answer. An answer that does not
// parse is an error rather than an empty workspace, so a bd the recorder cannot read is refused
// instead of taken for one that named nothing.
func workspaceFromWhere(stdout []byte) (string, error) {
	var answer struct {
		Data struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &answer); err != nil {
		return "", fmt.Errorf("bd where did not answer with JSON: %w", err)
	}
	return answer.Data.Path, nil
}

// bdCommand returns the command one bd run is: bin with args, run in dir, carrying the recording
// environment built from base, so bd works on the workspace beside dir and records the recorder as
// the author of every bead it writes (gt-p85pw.103).
func bdCommand(ctx context.Context, bin, dir string, args, base []string) *exec.Cmd {
	return bdCommandWith(ctx, bin, dir, args, childEnv(base, dir))
}

// bdCommandWith returns a bd process run in dir with env already built, so the guard can resolve
// with the exact environment a child is given.
func bdCommandWith(ctx context.Context, bin, dir string, args, env []string) *exec.Cmd {
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is the bd binary the operator names; args are the recording's fixed rows
	c.Dir = dir
	c.Env = env
	c.WaitDelay = time.Second
	return c
}

// beadsDirIn names the workspace beside dir, which is the one the recorder makes for a bd that runs
// there.
func beadsDirIn(dir string) string {
	return filepath.Join(dir, ".beads")
}

// childEnv is the environment a bd run in dir gets: the recording environment with the workspace
// beside dir pinned into it (gt-p85pw.115).
func childEnv(base []string, dir string) []string {
	return recordingEnv(base, beadsDirIn(dir))
}

// workspaceVars are the variables the beads fork reads to find the workspace a run works in: the
// .beads directory, the database file, the shared server's directory and the Dolt data directory.
// The list is a copy of bdTargetEnvKeys, internal/beads's one list of the variables it strips from a
// bd subprocess's environment (internal/beads/database.go); it is copied because that list is
// unexported, so the two change together. Each is pinned to the workspace the recorder made, so a
// value inherited from the operator's shell cannot aim a run at another store (gt-p85pw.115).
var workspaceVars = []string{
	"BEADS_DIR",
	"BEADS_DB",
	"BD_DB",
	"BEADS_SHARED_SERVER_DIR",
	"GT_DOLT_DATA",
}

// storeSelectors are the variables naming a store that is not the workspace the recorder made: the
// server database a run would otherwise be aimed at. They are dropped rather than pinned, since a
// copy's own database is the one its .beads metadata names (gt-p85pw.92).
var storeSelectors = []string{
	"BEADS_DOLT_SERVER_DATABASE",
}

// pinnedIdentity is the environment every bd run gets, so the actor and the git identity bd records
// are the recorder's own rather than the operator's: a golden's owner and created_by then read the
// same on every machine (gt-p85pw.103).
var pinnedIdentity = [][2]string{
	{"BEADS_ACTOR", "bdshim-record"},
	{"BD_ACTOR", "bdshim-record"},
	{"GIT_AUTHOR_NAME", "bdshim-record"},
	{"GIT_AUTHOR_EMAIL", "bdshim-record@example.invalid"},
	{"GIT_COMMITTER_NAME", "bdshim-record"},
	{"GIT_COMMITTER_EMAIL", "bdshim-record@example.invalid"},
}

// recordingEnv returns base with every workspace variable pinned to beadsDir and the store selectors
// dropped, so bd works on the workspace the recorder made, and with the identity pinned, so bd
// records the recorder as the author of every bead it writes.
func recordingEnv(base []string, beadsDir string) []string {
	for _, key := range storeSelectors {
		base = beads.StripEnvKey(base, key)
	}
	for _, key := range workspaceVars {
		base = append(beads.StripEnvKey(base, key), key+"="+beadsDir)
	}
	for _, pinned := range pinnedIdentity {
		base = append(beads.StripEnvKey(base, pinned[0]), pinned[0]+"="+pinned[1])
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
