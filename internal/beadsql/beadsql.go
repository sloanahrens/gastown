// Package beadsql is gastown's one door for raw SQL against bd's tables
// (ADR 0001, D1; gt-7iwy0.5). bd owns its schema and every write: gastown
// changes beads only through bd verbs. Where a read needs SQL (the reaper's
// candidate selection, health counts, history, raw dependency rows), it goes
// through a DB from this package, which
//
//   - declares the bd schema level its callers' queries were written against
//     (SchemaVersion) and refuses, fail closed and in one line, a database
//     at any other level; and
//   - runs reads only: a statement that is not a single SELECT, SHOW,
//     DESCRIBE or EXPLAIN, or that calls a dolt_* procedure, is refused
//     before it reaches the server.
//
// The startup handshake (internal/deps) holds the bd on PATH, and so every
// database it serves, to SchemaVersion too, which covers the reads that go
// through `bd sql`.
package beadsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	_ "github.com/go-sql-driver/mysql" // registers the "mysql" driver Open uses
)

// SchemaVersion is the bd schema level (MAX(version) in schema_migrations,
// bd's db_schema_version) that gastown's SQL reads were written against.
// When bd migrates past it, gastown stops reading until someone checks
// every query against the new schema and bumps this.
const SchemaVersion = 66

// SchemaLevelQuery reads a bd database's migration level.
const SchemaLevelQuery = "SELECT MAX(version) AS version FROM schema_migrations"

// ErrNotBeads marks a database with no schema_migrations table: not a bd
// database, so there is nothing for gastown to read in it.
var ErrNotBeads = errors.New("not a bd database")

// SchemaMismatchError is a database at a bd schema level other than
// SchemaVersion.
type SchemaMismatchError struct {
	Database string
	Found    int
}

func (e *SchemaMismatchError) Error() string {
	return fmt.Sprintf("beadsql: database %q is at bd schema %d but gastown's SQL reads are written for %d; refusing to read it (check the queries against the new schema, then bump beadsql.SchemaVersion)",
		e.Database, e.Found, SchemaVersion)
}

// CheckLevel returns a *SchemaMismatchError unless level is SchemaVersion.
func CheckLevel(database string, level int) error {
	if level != SchemaVersion {
		return &SchemaMismatchError{Database: database, Found: level}
	}
	return nil
}

// DB is a read-only handle on one bd database whose schema level matched
// SchemaVersion when it was opened.
type DB struct {
	db   *sql.DB
	name string
}

// Open connects to dsn (a go-sql-driver/mysql DSN naming database) and
// checks the database's schema level. The connection is closed on any
// refusal.
func Open(ctx context.Context, dsn, database string) (*DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("beadsql: open %s: %w", database, err)
	}
	return New(ctx, db, database)
}

// New checks db's schema level and wraps it. db must already be scoped to
// database (the DSN names it). It takes ownership of db: on a refusal db is
// closed, and DB.Close closes it.
func New(ctx context.Context, db *sql.DB, database string) (*DB, error) {
	var level sql.NullInt64
	if err := db.QueryRowContext(ctx, SchemaLevelQuery).Scan(&level); err != nil {
		_ = db.Close()
		if isTableNotFound(err) {
			return nil, fmt.Errorf("beadsql: %s: %w", database, ErrNotBeads)
		}
		return nil, fmt.Errorf("beadsql: reading %s schema level: %w", database, err)
	}
	if err := CheckLevel(database, int(level.Int64)); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{db: db, name: database}, nil
}

// Name is the database the handle reads.
func (d *DB) Name() string { return d.name }

// Close closes the connection pool.
func (d *DB) Close() error { return d.db.Close() }

// QueryContext runs a read-only query.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := ReadOnly(query); err != nil {
		return nil, err
	}
	return d.db.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a read-only query expected to return at most one row.
// A refused statement surfaces from Scan.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if err := ReadOnly(query); err != nil {
		return &Row{err: err}
	}
	return &Row{row: d.db.QueryRowContext(ctx, query, args...)}
}

// Row is the result of QueryRowContext.
type Row struct {
	row *sql.Row
	err error
}

// Scan copies the row's columns into dest, as sql.Row.Scan does
// (sql.ErrNoRows when there is no row).
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

// ErrNotReadOnly marks a statement ReadOnly refused.
var ErrNotReadOnly = errors.New("beadsql: not a read-only statement")

var (
	readVerbs = map[string]bool{"SELECT": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXPLAIN": true}
	// A Dolt stored procedure is callable as a function in a SELECT
	// (SELECT DOLT_COMMIT(...)), and some of them write.
	doltCall = regexp.MustCompile(`(?i)\bdolt_\w+\s*\(`)
	// Reads that write or lock.
	writingRead = regexp.MustCompile(`(?i)\bINTO\s+(OUTFILE|DUMPFILE)\b|\bFOR\s+(UPDATE|SHARE)\b|\bLOCK\s+IN\s+SHARE\s+MODE\b`)
)

// ReadOnly returns an error wrapping ErrNotReadOnly unless query is a single
// SELECT, SHOW, DESCRIBE or EXPLAIN statement that calls no dolt_* procedure
// and neither writes a file nor takes a lock. Quoted text is ignored, so a
// literal like 'a;b' or 'dolt_commit(' does not trip it.
func ReadOnly(query string) error {
	bare := stripQuoted(query)
	trimmed := strings.TrimSpace(bare)
	if i := strings.IndexByte(trimmed, ';'); i >= 0 && strings.TrimSpace(trimmed[i+1:]) != "" {
		return fmt.Errorf("%w: more than one statement: %s", ErrNotReadOnly, firstWords(query))
	}
	verb := strings.ToUpper(strings.TrimLeft(firstField(trimmed), "("))
	if !readVerbs[verb] {
		return fmt.Errorf("%w: %s", ErrNotReadOnly, firstWords(query))
	}
	if m := doltCall.FindString(bare); m != "" {
		return fmt.Errorf("%w: calls %s: %s", ErrNotReadOnly, strings.TrimSpace(m), firstWords(query))
	}
	if m := writingRead.FindString(bare); m != "" {
		return fmt.Errorf("%w: %s: %s", ErrNotReadOnly, m, firstWords(query))
	}
	return nil
}

// stripQuoted blanks the contents of '...', "..." and `...` spans
// (backslash escapes and doubled quotes included), keeping the quotes.
func stripQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == 0:
			if c == '\'' || c == '"' || c == '`' {
				quote = c
			}
			b.WriteByte(c)
		case c == '\\' && quote != '`' && i+1 < len(s):
			i++
			b.WriteString("  ")
		case c == quote:
			if i+1 < len(s) && s[i+1] == quote {
				i++
				b.WriteString("  ")
				continue
			}
			quote = 0
			b.WriteByte(c)
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// firstWords is the start of query for an error message.
func firstWords(query string) string {
	q := strings.Join(strings.Fields(query), " ")
	if len(q) > 60 {
		q = q[:60] + "..."
	}
	return fmt.Sprintf("%q", q)
}

// isTableNotFound reports a MySQL/Dolt missing-table error.
func isTableNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "table not found") || strings.Contains(msg, "doesn't exist")
}
