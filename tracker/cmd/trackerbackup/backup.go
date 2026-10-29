package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/steveyegge/gastown/tracker/internal/pgdsn"
)

const (
	// dateLayout names a dump after the day it was taken, which is also what
	// makes the names sort chronologically.
	dateLayout = "2006-01-02"
	dumpSuffix = ".dump"
	// partialSuffix is appended to a dump name while it is being written: the
	// file becomes a .dump only once pg_dump has finished, so a reader never
	// opens half a dump.
	partialSuffix = ".partial"

	defaultKeep = 7
)

// run is trackerbackup. The DSN holds a password: it comes from the
// environment only and is never written to output or to an error.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	return runWith(ctx, args, getenv, stdout, stderr, execRunner{})
}

// runWith is run with the pg_dump runner injected, so the tests need neither a
// Postgres server nor the pg_dump binary.
func runWith(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, r runner) error {
	fs := flag.NewFlagSet("trackerbackup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", defaultDir(), "directory that holds the dumps; created 0700 when missing")
	keep := fs.Int("keep", defaultKeep, "how many dumps to keep, tonight's included")
	pgBin := fs.String("pg-bin", "", "directory holding pg_dump; empty uses the pg_dump on PATH")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("-dir is empty and the home directory cannot be found")
	}
	dsn := getenv("TRACKER_DSN")
	if dsn == "" {
		return errors.New("TRACKER_DSN is not set (a Postgres connection string)")
	}
	b := &backup{
		dir:    *dir,
		keep:   *keep,
		pgBin:  *pgBin,
		dsn:    dsn,
		clock:  systemClock{},
		run:    r,
		remove: os.Remove,
		stdout: stdout,
		stderr: stderr,
	}
	return b.Run(ctx)
}

// defaultDir is where the dumps live unless -dir says otherwise.
func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "gt-backups", "tracker")
}

// clock supplies the date a dump is named for.
type clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// runner runs pg_dump. The tests inject a fake that writes the dump file and
// records the argv and environment it was given.
type runner interface {
	Run(ctx context.Context, argv, env []string) error
}

// execRunner runs pg_dump as a child process.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, argv, env []string) error {
	//nolint:gosec // G204: argv[0] is <pg-bin>/pg_dump and the rest is built here from the flags
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
	const maxLine = 400
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

// backup is one nightly dump and the retention that follows it.
type backup struct {
	dir    string
	keep   int
	pgBin  string
	dsn    string
	clock  clock
	run    runner
	remove func(string) error
	stdout io.Writer
	stderr io.Writer
}

// dumpArgv is the pg_dump command. The DSN is passed without its password, so
// argv, which every local user can list, holds no credential; the password
// travels in PGPASSWORD (see dumpEnv). pgBin empty leaves pg_dump to PATH.
func dumpArgv(pgBin, partial, dsnNoPassword string) []string {
	name := "pg_dump"
	if pgBin != "" {
		name = filepath.Join(pgBin, "pg_dump")
	}
	return []string{name, "-Fc", "-f", partial, "-d", dsnNoPassword}
}

// Run writes today's dump, publishes it only after pg_dump has exited 0 with a
// non-empty file, then deletes the oldest dumps beyond keep. Any failure leaves
// the dumps already in the directory, today's included, untouched.
func (b *backup) Run(ctx context.Context) error {
	if b.keep < 1 {
		return fmt.Errorf("-keep must be at least 1, got %d", b.keep)
	}
	creds, err := parseDSN(b.dsn)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return fmt.Errorf("create backup directory %s: %w", b.dir, err)
	}
	// One run at a time: a second run's stale-partial sweep would otherwise
	// delete the partial this one is writing.
	unlock, err := lockBackupDir(b.dir)
	if err != nil {
		return err
	}
	defer unlock()
	b.removeStalePartials()

	now := b.clock.Now()
	target := filepath.Join(b.dir, now.Format(dateLayout)+dumpSuffix)
	partial := target + partialSuffix
	// The password comes out of the string and travels in PGPASSWORD, so argv,
	// which every local user can list, holds no credential; sslmode and the
	// rest stay in the string pg_dump parses.
	noPassword, err := pgdsn.WithoutPassword(b.dsn)
	if err != nil {
		return errors.New("cannot use TRACKER_DSN as a connection string") // the parse error can echo the DSN
	}

	// The file is opened 0600 here rather than left to pg_dump, whose new file
	// the umask would open to every local user; pg_dump truncates this one and
	// keeps its mode.
	// A leftover path of any kind is removed first, and the file is created
	// exclusively: a symlink planted at the partial name cannot be followed into
	// another file.
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear %s: %w", partial, err)
	}
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // G304: partial is built from the -dir flag
	if err != nil {
		return fmt.Errorf("create %s: %w", partial, err)
	}
	if err := f.Close(); err != nil {
		_ = b.remove(partial)
		return fmt.Errorf("create %s: %w", partial, err)
	}

	argv := dumpArgv(b.pgBin, partial, noPassword)
	if err := b.run.Run(ctx, argv, dumpEnv(os.Environ(), creds)); err != nil {
		_ = b.remove(partial)
		return fmt.Errorf("pg_dump: %s", redact(err.Error(), b.dsn, creds.password))
	}
	info, err := os.Stat(partial)
	if err != nil {
		_ = b.remove(partial)
		return fmt.Errorf("pg_dump exited 0 but wrote no dump at %s: %w", partial, err)
	}
	if info.Size() == 0 {
		_ = b.remove(partial)
		return fmt.Errorf("pg_dump exited 0 but wrote an empty dump at %s", partial)
	}
	// The dump is flushed to disk before it is published under its final name,
	// so a crash cannot leave a .dump whose bytes never reached the disk. The
	// directory is flushed after the rename, so the name itself is durable.
	if err := syncFile(partial); err != nil {
		_ = b.remove(partial)
		return fmt.Errorf("flush %s: %w", partial, err)
	}
	if err := os.Rename(partial, target); err != nil {
		_ = b.remove(partial)
		return fmt.Errorf("publish %s: %w", target, err)
	}
	// The name is published; a failed directory flush is a warning, as a failed
	// retention is, so the operator still gets the dump and the reason.
	if err := syncFile(b.dir); err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "trackerbackup: warning: flush backup directory %s: %v (the dump was published)\n", b.dir, err)
	}
	// The dump is published, so a retention failure is a warning, not a failed
	// run: the operator still gets the dump and the reason its old ones stayed.
	if err := b.prune(now); err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "trackerbackup: warning: %v (the dump was published)\n", err)
	}
	fmt.Fprintf(b.stdout, "wrote %s (%d bytes)\n", target, info.Size())
	return nil
}

// syncFile flushes path's contents to stable storage.
func syncFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is the partial this run wrote
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// removeStalePartials deletes dated .partial files a killed run left behind:
// they are never restorable and would otherwise accumulate forever. Nothing
// without the date shape is touched.
func (b *backup) removeStalePartials() {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		prefix, ok := strings.CutSuffix(e.Name(), dumpSuffix+partialSuffix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if _, err := time.Parse(dateLayout, prefix); err != nil {
			continue
		}
		_ = b.remove(filepath.Join(b.dir, e.Name()))
	}
}

// prune deletes the oldest dumps beyond keep. A name qualifies only when it is
// YYYY-MM-DD.dump and a real calendar date, so every other file in the
// directory, a dump whose date does not parse included, is left alone. A dump
// dated after today is not a candidate and does not count toward keep, so a
// future-dated file can never push today's dump out.
func (b *backup) prune(now time.Time) error {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return fmt.Errorf("read backup directory %s: %w", b.dir, err)
	}
	today := dumpDate(now.Format(dateLayout) + dumpSuffix)
	var dumps []string
	for _, e := range entries {
		date := dumpDate(e.Name())
		if e.Type().IsRegular() && !date.IsZero() && date.After(today) && b.stderr != nil {
			fmt.Fprintf(b.stderr, "trackerbackup: warning: %s is dated after today and is not counted or pruned\n", e.Name())
		}
		if e.Type().IsRegular() && !date.IsZero() && !date.After(today) {
			dumps = append(dumps, e.Name())
		}
	}
	sort.Strings(dumps) // YYYY-MM-DD names sort oldest first
	for len(dumps) > b.keep {
		name := dumps[0]
		if err := b.remove(filepath.Join(b.dir, name)); err != nil {
			return fmt.Errorf("delete old dump %s: %w", name, err)
		}
		dumps = dumps[1:]
	}
	return nil
}

// dumpDate is the date a dump file is named for, or the zero time when name is
// not a dump this program wrote.
func dumpDate(name string) time.Time {
	date, ok := strings.CutSuffix(name, dumpSuffix)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(dateLayout, date)
	if err != nil || t.Format(dateLayout) != date {
		return time.Time{}
	}
	return t
}

// credentials are the connection settings pg_dump reads from its environment.
type credentials struct {
	host, port, user, database, password string
}

// parseDSN splits TRACKER_DSN into the settings pg_dump needs, so the password
// can travel in the environment instead of in argv, where a process listing
// would show it to every local user. Both a postgres:// URI and a libpq
// keyword string are accepted. The error never quotes the DSN.
func parseDSN(dsn string) (credentials, error) {
	if strings.TrimSpace(dsn) == "" {
		return credentials{}, errors.New("TRACKER_DSN is not set (a Postgres connection string)")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return credentials{}, errors.New("cannot use TRACKER_DSN as a connection string") // the parse error can echo the DSN
	}
	return credentials{
		host:     cfg.Host,
		port:     strconv.Itoa(int(cfg.Port)),
		user:     cfg.User,
		database: cfg.Database,
		password: cfg.Password,
	}, nil
}

// dumpEnv is the environment pg_dump runs with. The PG* settings the DSN
// supplies are dropped from the inherited environment first, so a stale
// PGHOST, PGPASSWORD or service file cannot override the job. TRACKER_DSN goes
// too: pg_dump reads its credentials from the settings below, and the child has
// no use for the whole connection string.
func dumpEnv(inherited []string, c credentials) []string {
	env := make([]string, 0, len(inherited)+5)
	for _, kv := range inherited {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGPASSWORD", "PGSERVICE", "PGSERVICEFILE", "TRACKER_DSN":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"PGHOST="+c.host,
		"PGPORT="+c.port,
		"PGUSER="+c.user,
		"PGDATABASE="+c.database,
		"PGPASSWORD="+c.password,
	)
}

// credentialPatterns mask a password in credential position, at any length.
var credentialPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// password=plain, password='quoted value', password="quoted value"
	{regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|"[^"]*"|\S+)`), "${1}***"},
	// postgres://user:password@host
	{regexp.MustCompile(`(://[^/\s:@]+):[^/\s@]*@`), "${1}:***@"},
}

// redact removes the secrets from text that came from outside this process: a
// child can echo the connection string, and a URI DSN carries the password
// percent-encoded, so the escaped spellings are matched too. Every spelling is
// replaced wherever it appears, however short: a masked word in an error line
// costs less than a password left readable in one.
func redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		for _, s := range secretSpellings(secret) {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	for _, p := range credentialPatterns {
		text = p.re.ReplaceAllString(text, p.repl)
	}
	return text
}

// secretSpellings is the secret as written plus the forms a URI or form
// carries it in.
func secretSpellings(secret string) []string {
	if secret == "" {
		return nil
	}
	spellings := []string{secret}
	for _, escaped := range []string{url.QueryEscape(secret), url.PathEscape(secret)} {
		spellings = append(spellings, escaped, strings.ReplaceAll(escaped, "+", "%20"))
	}
	return spellings
}
