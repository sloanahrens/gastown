package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// migrationLockKey is the advisory-lock key Migrate takes so two processes
// opening the same database do not apply a migration twice.
const migrationLockKey int64 = 0x7472_6b72_6d69_6772 // "trkrmigr"

// undefinedTable is the SQLSTATE for a missing relation.
const undefinedTable = "42P01"

// ErrSchemaTooNew means the database was migrated by a newer binary than this
// one. The binary refuses to run against it rather than guess.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this binary")

type migration struct {
	version int
	name    string
	sql     string
}

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// loadMigrations reads dir of fsys: NNNN_name.sql files numbered 0001 upward
// with no gap or duplicate, each non-empty. Anything else in dir is an error,
// so a misnamed file cannot be skipped silently.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", dir, err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			if strings.HasSuffix(e.Name(), ".sql") || strings.HasPrefix(e.Name(), "0") {
				return nil, fmt.Errorf("store: migration file %s is not named NNNN_name.sql (lower case)", e.Name())
			}
			continue
		}
		body, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("store: migration %s is empty", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		out = append(out, migration{version: v, name: m[2], sql: string(body)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("store: no migrations in %s", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if want := i + 1; m.version != want {
			return nil, fmt.Errorf("store: migrations must be numbered %04d upward with no gap or duplicate; found %04d where %04d belongs", want, m.version, want)
		}
	}
	return out, nil
}

var (
	embeddedOnce sync.Once
	embeddedMs   []migration
	embeddedErr  error
)

// embeddedMigrations are the binary's own migrations, loaded once.
func embeddedMigrations() ([]migration, error) {
	embeddedOnce.Do(func() { embeddedMs, embeddedErr = loadMigrations(schemaFS, "schema") })
	return embeddedMs, embeddedErr
}

// SchemaLevel is the newest schema version this binary knows. The server
// reports it, and clients refuse a server whose level differs from theirs.
//
// It panics when the embedded migrations are invalid. They are fixed at build
// time and TestEmbeddedMigrationsLoad checks them, so a failure here is a
// broken binary; reporting level 0 instead would read as "no schema".
func SchemaLevel() int {
	ms, err := embeddedMigrations()
	return levelOf(ms, err)
}

// levelOf is len(ms), or a panic when the migrations failed to load.
func levelOf(ms []migration, err error) int {
	if err != nil {
		panic(fmt.Sprintf("store: embedded migrations are invalid: %v", err))
	}
	return len(ms)
}

// checkHistory validates the versions a database has applied against this
// binary's migrations: none newer than the binary knows, none missing below
// the newest applied, and every name the same. A violation is an error so a
// migration is never applied out of order or onto a rewritten history.
func checkHistory(ms []migration, applied map[int]string, maxApplied int) error {
	if maxApplied > len(ms) {
		return fmt.Errorf("%w: database is at version %d, this binary knows %d", ErrSchemaTooNew, maxApplied, len(ms))
	}
	for v := 1; v <= maxApplied; v++ {
		n, ok := applied[v]
		if !ok {
			return fmt.Errorf("store: migrate: database history has a gap: version %d is missing below version %d", v, maxApplied)
		}
		if ms[v-1].name != n {
			return fmt.Errorf("store: migrate: database has version %d as %q, this binary has %q", v, n, ms[v-1].name)
		}
	}
	return nil
}

// Migrate brings the database to SchemaLevel. It serializes with other
// migrators through an advisory lock, applies each missing migration in its own
// transaction, and returns ErrSchemaTooNew (wrapped) when the database is
// ahead of this binary. Idempotent.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := embeddedMigrations()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("store: migrate: lock: %w", err)
	}
	defer func() {
		// Best effort: the lock is session-level and dies with the connection.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS tracker;
CREATE TABLE IF NOT EXISTS tracker.schema_migrations (
    version    integer PRIMARY KEY,
    name       text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("store: migrate: bootstrap: %w", err)
	}

	rows, err := conn.Query(ctx, "SELECT version, name FROM tracker.schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("store: migrate: read applied: %w", err)
	}
	applied := map[int]string{}
	maxApplied := 0
	for rows.Next() {
		var v int
		var n string
		if err := rows.Scan(&v, &n); err != nil {
			rows.Close()
			return fmt.Errorf("store: migrate: read applied: %w", err)
		}
		applied[v] = n
		maxApplied = max(maxApplied, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: migrate: read applied: %w", err)
	}
	if err := checkHistory(ms, applied, maxApplied); err != nil {
		return err
	}

	for _, m := range ms {
		if _, done := applied[m.version]; done {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("store: migrate %04d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx, m.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: migrate %04d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO tracker.schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: migrate %04d_%s: record: %w", m.version, m.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: migrate %04d_%s: commit: %w", m.version, m.name, err)
		}
	}
	return nil
}

// CurrentLevel is the newest version applied to the database: 0 when the
// migrations table does not exist yet. Any other failure is an error.
func CurrentLevel(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var level int
	err := pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM tracker.schema_migrations").Scan(&level)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return 0, nil
		}
		return 0, fmt.Errorf("store: schema level: %w", err)
	}
	return level, nil
}
