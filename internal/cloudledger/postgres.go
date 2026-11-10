package cloudledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// PostgresDriver is the database/sql driver name the source opens. The program
// that wires the source imports the driver for it, the way internal/beadsql's
// caller imports its own; this package never registers one.
const PostgresDriver = "pgx"

// PostgresReadTimeout bounds one read of the ledger. A database that does not
// answer within it fails the read with ErrReadTimeout, which the readers show
// as UNKNOWN, rather than holding the poll that asked.
const PostgresReadTimeout = 10 * time.Second

// ErrReadTimeout is a ledger read that the read deadline ended.
var ErrReadTimeout = errors.New("cloudledger: ledger read timed out")

// ErrNoDriver is a source whose database/sql driver the binary does not link:
// the program that wires the source must import the driver.
var ErrNoDriver = errors.New("cloudledger: database/sql driver is not linked into this binary")

// ledgerTables are the tables the reader's role must not be able to write in.
var ledgerTables = []string{PatrolReportTable, PatrolHeartbeatTable, ReleaseTable}

// PostgresSource reads the ledger tables in the cluster. The DSN comes from the
// file the setting names on every read, so a file that moved, went missing or
// loosened its mode fails closed at the next poll rather than at startup. The
// pool opened from that DSN is kept for the source's reads, and the reader
// role is proven once per pool, so a poll that reads every app does not open a
// connection per app. Close releases the pool.
type PostgresSource struct {
	dsnFile string
	driver  string
	timeout time.Duration

	// mu serializes reads, so the pool and its role proof change only under it.
	mu sync.Mutex
	// dsn is the DSN db was opened with; a DSN file that changed reopens it.
	dsn    string
	db     *sql.DB
	roleOK bool
}

// NewPostgresSource returns a source that reads the ledger through the DSN in
// dsnFile.
func NewPostgresSource(dsnFile string) *PostgresSource {
	return &PostgresSource{dsnFile: dsnFile, driver: PostgresDriver, timeout: PostgresReadTimeout}
}

// Close releases the pool the source holds. A read after Close opens a new one.
func (p *PostgresSource) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeLocked()
}

// closeLocked closes the held pool, if any, and forgets the role proof with it.
func (p *PostgresSource) closeLocked() error {
	if p.db == nil {
		return nil
	}
	err := p.db.Close()
	dsn := p.dsn
	p.db, p.dsn, p.roleOK = nil, "", false
	if err != nil {
		return postgresError(dsn, err)
	}
	return nil
}

// LatestPatrolReport reads the newest report row.
func (p *PostgresSource) LatestPatrolReport(ctx context.Context) (Report, error) {
	var (
		rep               Report
		projects, finding []byte
	)
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			"SELECT schema_version, projects, findings FROM "+PatrolReportTable+" ORDER BY finished_at DESC LIMIT 1")
		if err := row.Scan(&rep.SchemaVersion, &projects, &finding); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoReport
			}
			return err
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	if err := json.Unmarshal(projects, &rep.Projects); err != nil {
		return Report{}, fmt.Errorf("%w: projects: %v", ErrInvalidReport, err)
	}
	if err := json.Unmarshal(finding, &rep.Findings); err != nil {
		return Report{}, fmt.Errorf("%w: findings: %v", ErrInvalidReport, err)
	}
	return rep, nil
}

// Heartbeat reads the newest patrol heartbeat. The report beside it is the
// newest report any patrol wrote, so the heartbeat is the newest one too: it
// would be wrong to date that report with a run that did not write it.
func (p *PostgresSource) Heartbeat(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			"SELECT at FROM "+PatrolHeartbeatTable+" ORDER BY at DESC LIMIT 1").Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoHeartbeat
		}
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	return at, nil
}

// ReleasesSince returns app's release lines later than since, oldest first.
func (p *PostgresSource) ReleasesSince(ctx context.Context, app string, since time.Time) ([]Release, error) {
	lines := []Release{}
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			"SELECT line FROM "+ReleaseTable+" WHERE app = $1 AND at > $2 ORDER BY at", app, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			r, err := release(raw)
			if err != nil {
				return err
			}
			lines = append(lines, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return lines, nil
}

// LastRelease returns app's newest release line.
func (p *PostgresSource) LastRelease(ctx context.Context, app string) (Release, error) {
	var raw []byte
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			"SELECT line FROM "+ReleaseTable+" WHERE app = $1 ORDER BY at DESC LIMIT 1", app).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoRelease
		}
		return err
	})
	if err != nil {
		return Release{}, err
	}
	return release(raw)
}

// Apps lists the app names the release table holds a line for.
func (p *PostgresSource) Apps(ctx context.Context) ([]string, error) {
	apps := []string{}
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT DISTINCT app FROM "+ReleaseTable+" ORDER BY app")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var app string
			if err := rows.Scan(&app); err != nil {
				return err
			}
			apps = append(apps, app)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// read runs fn in a read-only transaction over the ledger, under the source's
// read deadline. The DSN file is read and its mode checked on every call; the
// pool and the role proof are kept while the DSN is unchanged. A read that the
// deadline ends is ErrReadTimeout, and a read the caller canceled is the
// caller's error as it stands.
func (p *PostgresSource) read(parent context.Context, fn func(context.Context, *sql.Tx) error) error {
	dsn, err := readDSN(p.dsnFile, os.ReadFile)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.openLocked(dsn); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	err = p.runLocked(ctx, fn)
	if err != nil && parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return postgresError(dsn, fmt.Errorf("%w after %s", ErrReadTimeout, p.timeout))
	}
	return err
}

// openLocked opens the pool for dsn, reusing the held one when the DSN has not
// changed. A held pool for another DSN is closed first.
func (p *PostgresSource) openLocked(dsn string) error {
	if p.db != nil && p.dsn == dsn {
		return nil
	}
	if err := p.closeLocked(); err != nil {
		return err
	}
	if !slices.Contains(sql.Drivers(), p.driver) {
		return postgresError(dsn, fmt.Errorf("%w: %q", ErrNoDriver, p.driver))
	}
	db, err := sql.Open(p.driver, dsn)
	if err != nil {
		return postgresError(dsn, err)
	}
	p.db, p.dsn = db, dsn
	return nil
}

// runLocked opens the read-only transaction and, until the role has been
// proven, checks the role first. The transaction rolls back on every path: a
// read needs no commit, and one a failed read left open is one the pool keeps
// holding.
func (p *PostgresSource) runLocked(ctx context.Context, fn func(context.Context, *sql.Tx) error) (err error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return postgresError(p.dsn, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) && err == nil {
			err = postgresError(p.dsn, rbErr)
		}
	}()
	if !p.roleOK {
		if err := checkReaderRole(ctx, tx); err != nil {
			return postgresError(p.dsn, err)
		}
		p.roleOK = true
	}
	return postgresError(p.dsn, fn(ctx, tx))
}

// checkReaderRole refuses a role that can INSERT into any ledger table, naming
// the table before a single ledger row is read. A probe that cannot be read is
// a refusal too: a role the reader cannot prove read-only is not read-only.
func checkReaderRole(ctx context.Context, tx *sql.Tx) error {
	for _, table := range ledgerTables {
		var canInsert sql.NullBool
		err := tx.QueryRowContext(ctx,
			"SELECT has_table_privilege(current_user, $1, 'INSERT')", table).Scan(&canInsert)
		if err != nil {
			return fmt.Errorf("checking the reader role on %s: %w", table, err)
		}
		if !canInsert.Valid || canInsert.Bool {
			return fmt.Errorf("%w: %s", ErrWriterRole, table)
		}
	}
	return nil
}

// readDSN reads the DSN the setting names. A file that is missing, not a
// regular file, empty, or readable by group or others is an error naming the
// file, never the DSN. A file with no bytes is ErrEmptyDSN whatever its mode:
// the mode refusal would hide the state the operator has to fill, and an empty
// file holds nothing to leak. readFile is injected so a test can drive the read
// failure a readable regular file cannot be made to produce.
func readDSN(path string, readFile func(string) ([]byte, error)) (string, error) {
	if path == "" {
		return "", errors.New("cloudledger: postgres source: no DSN file is set")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cloudledger: postgres source: DSN file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("cloudledger: postgres source: DSN file %s: %w", path, ErrNotRegular)
	}
	if fi.Size() == 0 {
		return "", fmt.Errorf("%w: %s", ErrEmptyDSN, path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("cloudledger: postgres source: DSN file %s is readable by group or others (mode %04o); want 0600 or stricter", path, perm)
	}
	b, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("cloudledger: postgres source: reading DSN file %s: %w", path, err)
	}
	dsn := strings.TrimSpace(string(b))
	if dsn == "" {
		return "", fmt.Errorf("%w: %s", ErrEmptyDSN, path)
	}
	return dsn, nil
}

// CheckDSNFile applies the refusals a ledger read gives the DSN file at path and
// returns the first one, or nil when the file holds a DSN. It opens no
// connection, and its error never carries the DSN.
func CheckDSNFile(path string) error {
	_, err := readDSN(path, os.ReadFile)
	return err
}

// postgresError wraps a driver or query error, removing the DSN from its text:
// a driver is free to quote what it was given, and the DSN is a credential.
func postgresError(dsn string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("cloudledger: postgres source: %w", redactedError{secrets: secretsOf(dsn), err: err})
}

// secretsOf returns the text of dsn a driver error could quote: the whole DSN,
// and the password in each form the DSN carries it. A URL carries it raw and
// percent-encoded; a keyword-form DSN carries it as a value, quoted or not. A
// password is redacted on its own so an error that names only it is caught.
func secretsOf(dsn string) []string {
	out := []string{dsn}
	if dsn == "" {
		return out
	}
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			out = append(out, pw, url.QueryEscape(pw), url.PathEscape(pw))
		}
	}
	return append(out, keywordPassword(dsn)...)
}

// keywordPassword returns the password of a keyword-form DSN ("host=h
// password=p"), in the form the DSN wrote it and in the form it means. A value
// with a space or a quote is single-quoted, and a backslash escapes inside the
// quotes. Each step consumes one key and its value, so the loop ends.
func keywordPassword(dsn string) []string {
	var out []string
	rest := dsn
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return out
		}
		key := strings.TrimSpace(rest[:eq])
		var raw, val string
		raw, val, rest = keywordValue(strings.TrimLeft(rest[eq+1:], " \t\r\n"))
		if key == "password" {
			out = append(out, raw, val)
		}
	}
}

// keywordValue splits the value at the start of s from the text after it,
// returning the value as written, the value it means, and the rest.
func keywordValue(s string) (raw, val, rest string) {
	if !strings.HasPrefix(s, "'") {
		end := strings.IndexAny(s, " \t\r\n")
		if end < 0 {
			end = len(s)
		}
		return s[:end], s[:end], s[end:]
	}
	var b strings.Builder
	i := 1
	for ; i < len(s) && s[i] != '\''; i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	end := min(i+1, len(s))
	return s[:end], b.String(), s[end:]
}

// redactedError reports err's text with every secret replaced, keeping the
// chain so callers can still match a sentinel. The longest secret goes first,
// so a whole DSN is replaced before the password inside it.
type redactedError struct {
	secrets []string
	err     error
}

func (e redactedError) Error() string {
	msg := e.err.Error()
	secrets := slices.Clone(e.secrets)
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "<dsn>")
		}
	}
	return msg
}

func (e redactedError) Unwrap() error { return e.err }
