package cloudledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// PostgresDriver is the database/sql driver name the source opens. The program
// that wires the source imports the driver for it, the way internal/beadsql's
// caller imports its own; this package never registers one.
const PostgresDriver = "pgx"

// ledgerTables are the tables the reader's role must not be able to write in.
var ledgerTables = []string{PatrolReportTable, PatrolHeartbeatTable, ReleaseTable}

// PostgresSource reads the ledger tables in the cluster. The DSN comes from the
// file the setting names on every read, so a file that moved, went missing or
// loosened its mode fails closed at the next poll rather than at startup.
type PostgresSource struct {
	dsnFile string
}

// NewPostgresSource returns a source that reads the ledger through the DSN in
// dsnFile.
func NewPostgresSource(dsnFile string) *PostgresSource { return &PostgresSource{dsnFile: dsnFile} }

// LatestPatrolReport reads the newest report row.
func (p *PostgresSource) LatestPatrolReport(ctx context.Context) (Report, error) {
	var (
		rep               Report
		projects, finding []byte
	)
	err := p.read(ctx, func(tx *sql.Tx) error {
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
	err := p.read(ctx, func(tx *sql.Tx) error {
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
	err := p.read(ctx, func(tx *sql.Tx) error {
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
	err := p.read(ctx, func(tx *sql.Tx) error {
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
	err := p.read(ctx, func(tx *sql.Tx) error {
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

// read opens a read-only transaction over the ledger, proves the role cannot
// write it, and runs fn. It rolls back on every path: a read needs no commit,
// and a transaction left open by a failed read is one the pool keeps holding.
func (p *PostgresSource) read(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	dsn, err := readDSN(p.dsnFile, os.ReadFile)
	if err != nil {
		return err
	}
	db, err := sql.Open(PostgresDriver, dsn)
	if err != nil {
		return postgresError(dsn, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = postgresError(dsn, cerr)
		}
	}()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return postgresError(dsn, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) && err == nil {
			err = postgresError(dsn, rbErr)
		}
	}()
	if err := p.checkReaderRole(ctx, tx); err != nil {
		return postgresError(dsn, err)
	}
	return postgresError(dsn, fn(tx))
}

// checkReaderRole refuses a role that can INSERT into any ledger table, naming
// the table before a single ledger row is read. A probe that cannot be read is
// a refusal too: a role the reader cannot prove read-only is not read-only.
func (p *PostgresSource) checkReaderRole(ctx context.Context, tx *sql.Tx) error {
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
// file, never the DSN. readFile is injected so a test can drive the read
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
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("cloudledger: postgres source: DSN file %s is readable by group or others (mode %04o); want 0600 or stricter", path, perm)
	}
	b, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("cloudledger: postgres source: reading DSN file %s: %w", path, err)
	}
	dsn := strings.TrimSpace(string(b))
	if dsn == "" {
		return "", fmt.Errorf("cloudledger: postgres source: DSN file %s is empty", path)
	}
	return dsn, nil
}

// postgresError wraps a driver or query error, removing the DSN from its text:
// a driver is free to quote what it was given, and the DSN is a credential.
func postgresError(dsn string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("cloudledger: postgres source: %w", redactedError{dsn: dsn, err: err})
}

// redactedError reports err's text with every occurrence of the DSN replaced,
// keeping the chain so callers can still match a sentinel.
type redactedError struct {
	dsn string
	err error
}

func (e redactedError) Error() string {
	if e.dsn == "" {
		return e.err.Error()
	}
	return strings.ReplaceAll(e.err.Error(), e.dsn, "<dsn>")
}

func (e redactedError) Unwrap() error { return e.err }
