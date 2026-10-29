// Package restorecheck proves a tracker dump restores and reads.
//
// Check restores one pg_dump into a new scratch database on the same cluster,
// reads a small contract subset from that copy, and drops the database
// whatever the outcome. docs/tracker-restore.md is the operator's runbook; this
// package is its proof.
package restorecheck

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/tracker/internal/pgdsn"
)

const (
	// scratchPrefix marks every database this check creates, so an operator can
	// find one that a killed run left behind.
	scratchPrefix = "restorecheck_"
	// dropTimeout bounds the cleanup drop, which runs after the caller's
	// context is canceled.
	dropTimeout = 30 * time.Second
	// maxLine caps how much of a client command's last line an error carries.
	maxLine = 400
)

// Runner runs one PostgreSQL client command. The tests inject a fake that
// records every command, which is what proves the order and the drop.
type Runner interface {
	Run(ctx context.Context, argv, env []string) error
}

// Exec runs a client command as a child process.
type Exec struct{}

// Run runs argv with env and returns its failure, naming the command and its
// last line of stderr: the caller acts on that line and has nothing else to go
// on.
func (Exec) Run(ctx context.Context, argv, env []string) error {
	//nolint:gosec // G204: argv[0] is <pg-bin>/<client> and the rest is built here from the config
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if line := lastLine(stderr.String()); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}
		return err
	}
	return nil
}

// lastLine returns the last non-empty line of a child's output, capped so a
// child that prints a page cannot bury the failure it is reporting.
func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > maxLine {
			return line[:maxLine]
		}
		return line
	}
	return ""
}

// Config is one check: the dump to restore and how to reach the cluster.
type Config struct {
	// DSN reaches the cluster as a role that may create and drop databases.
	// The scratch database is created on the same cluster.
	DSN string
	// Dump is the pg_dump file to restore.
	Dump string
	// PGBin is the directory holding createdb, pg_restore and dropdb; empty
	// uses the commands on PATH.
	PGBin string
	// Name names the scratch database; nil uses a random name. createdb refuses
	// a name that is taken, so the check fails rather than restore into a
	// database it did not create.
	Name func() string
	// Read verifies the restored copy. It receives the scratch database's
	// connection string, which carries no password; nil uses ReadOnlySubset
	// through the store's contract.
	Read func(ctx context.Context, dsn string) error
	// Env is the environment the client commands inherit; nil uses
	// os.Environ(). The connection string travels as an argument, and libpq
	// lets every setting in it win over these.
	Env []string
}

// Check restores c.Dump into a new scratch database, runs the read subset
// against it, and drops the scratch database even when a step fails. The error
// names the step that failed; a drop that fails as well is joined to it.
func Check(ctx context.Context, r Runner, c Config) (err error) {
	if r == nil {
		return errors.New("restorecheck: no runner")
	}
	if c.DSN == "" {
		return errors.New("restorecheck: no connection string")
	}
	if c.Dump == "" {
		return errors.New("restorecheck: no dump to restore")
	}
	if _, err := os.Stat(c.Dump); err != nil {
		return fmt.Errorf("restorecheck: dump: %w", err)
	}
	conninfo, err := pgdsn.WithoutPassword(c.DSN)
	if err != nil {
		return fmt.Errorf("restorecheck: %w", err)
	}
	password, err := pgdsn.Password(c.DSN)
	if err != nil {
		return fmt.Errorf("restorecheck: %w", err)
	}
	name := randomName
	if c.Name != nil {
		name = c.Name
	}
	scratch := name()
	scratchDSN, err := pgdsn.WithDatabase(conninfo, scratch)
	if err != nil {
		return fmt.Errorf("restorecheck: %w", err)
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	env = childEnv(env, password)
	read := c.Read
	if read == nil {
		read = readSubset
	}

	// created says whether this run made the scratch database. Only a database
	// this run made is ever dropped: createdb failing on a name that was taken
	// must not turn the cleanup into a drop of somebody else's database.
	created := false
	defer func() {
		if !created {
			return
		}
		// A failed restore or read must not leave a scratch database behind, so
		// the drop runs whatever happened above. It keeps working after the
		// caller's context is canceled, which is when a killed run needs it.
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropTimeout)
		defer cancel()
		if dropErr := r.Run(dropCtx, dropdbArgv(c.PGBin, conninfo, scratch), env); dropErr != nil {
			err = errors.Join(err, fmt.Errorf("restorecheck: drop %s: %w", scratch, dropErr))
		}
	}()

	if err := r.Run(ctx, createdbArgv(c.PGBin, conninfo, scratch), env); err != nil {
		return fmt.Errorf("restorecheck: create %s: %w", scratch, err)
	}
	created = true
	if err := r.Run(ctx, restoreArgv(c.PGBin, scratchDSN, c.Dump), env); err != nil {
		return fmt.Errorf("restorecheck: restore %s: %w", c.Dump, err)
	}
	if err := read(ctx, scratchDSN); err != nil {
		return fmt.Errorf("restorecheck: read %s: %w", scratch, err)
	}
	return nil
}

// clientPath is the client command in pgBin, or the one on PATH when pgBin is
// empty.
func clientPath(pgBin, name string) string {
	if pgBin == "" {
		return name
	}
	return filepath.Join(pgBin, name)
}

// createdbArgv creates the scratch database. The connection string reaches
// createdb as its maintenance database, so every setting in it applies.
func createdbArgv(pgBin, conninfo, name string) []string {
	return []string{clientPath(pgBin, "createdb"), "--maintenance-db", conninfo, name}
}

// restoreArgv restores into the scratch database. --exit-on-error stops at the
// first error and makes pg_restore exit non-zero when it would otherwise have
// ignored one, so a copy with holes fails the check instead of reading back.
// --no-owner gives the restored objects to the connecting role, which need not
// be the role the dump records as their owner.
func restoreArgv(pgBin, scratchDSN, dump string) []string {
	return []string{clientPath(pgBin, "pg_restore"), "--exit-on-error", "--no-owner", "-d", scratchDSN, dump}
}

// dropdbArgv drops the scratch database, present or not, with whatever still
// holds it disconnected.
func dropdbArgv(pgBin, conninfo, name string) []string {
	return []string{clientPath(pgBin, "dropdb"), "--maintenance-db", conninfo, "--if-exists", "--force", name}
}

// childEnv is the environment the client commands run with: env, and
// PGPASSWORD when the connection string carried a password. libpq takes every
// setting the connection string names from the string itself, so an inherited
// PGHOST or PGSSLMODE cannot redirect the command.
func childEnv(env []string, password string) []string {
	if password == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == "PGPASSWORD" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PGPASSWORD="+password)
}

// randomName names a scratch database: the prefix, this process's id, and
// eight random bytes. Two runs cannot draw the same name, and createdb refuses
// one already taken.
func randomName() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // Read never fails: it fills b entirely
	return fmt.Sprintf("%s%d_%s", scratchPrefix, os.Getpid(), hex.EncodeToString(b[:]))
}
