package beadsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeServer answers the schema-level read and records every statement that
// reaches it.
type fakeServer struct {
	level    any   // value of MAX(version); nil for an empty table
	levelErr error // error for the schema-level read
	mu       sync.Mutex
	seen     []string
}

func (s *fakeServer) Connect(context.Context) (driver.Conn, error) { return fakeConn{s}, nil }
func (s *fakeServer) Driver() driver.Driver                        { return fakeDriver{s} }

func (s *fakeServer) statements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

type fakeDriver struct{ s *fakeServer }

func (d fakeDriver) Open(string) (driver.Conn, error) { return fakeConn(d), nil }

type fakeConn struct{ s *fakeServer }

func (c fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("prepare unsupported") }
func (c fakeConn) Close() error                        { return nil }
func (c fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("tx unsupported") }

func (c fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.s.mu.Lock()
	c.s.seen = append(c.s.seen, query)
	c.s.mu.Unlock()
	if query == SchemaLevelQuery {
		if c.s.levelErr != nil {
			return nil, c.s.levelErr
		}
		return &fakeRows{cols: []string{"version"}, rows: [][]driver.Value{{c.s.level}}}, nil
	}
	return &fakeRows{cols: []string{"n"}, rows: [][]driver.Value{{int64(7)}}}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func open(t *testing.T, s *fakeServer) (*DB, error) {
	t.Helper()
	return New(context.Background(), sql.OpenDB(s), "hq")
}

func TestNewAcceptsTheDeclaredLevel(t *testing.T) {
	t.Parallel()
	s := &fakeServer{level: int64(SchemaVersion)}
	db, err := open(t, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM issues").Scan(&n); err != nil || n != 7 {
		t.Fatalf("read = %d, %v", n, err)
	}
	if db.Name() != "hq" {
		t.Fatalf("Name = %q", db.Name())
	}
}

func TestNewRefusesAnyOtherLevel(t *testing.T) {
	t.Parallel()
	for _, level := range []any{int64(SchemaVersion + 1), int64(SchemaVersion - 1), nil} {
		s := &fakeServer{level: level}
		_, err := open(t, s)
		var mismatch *SchemaMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("level %v: err = %v, want *SchemaMismatchError", level, err)
		}
		msg := err.Error()
		if strings.Contains(msg, "\n") || !strings.Contains(msg, `"hq"`) || !strings.Contains(msg, "beadsql.SchemaVersion") {
			t.Errorf("refusal is not one line naming the database and the fix: %q", msg)
		}
		if got := s.statements(); len(got) != 1 {
			t.Errorf("level %v: statements after refusal = %v, want only the level read", level, got)
		}
	}
}

func TestNewNamesANonBeadsDatabase(t *testing.T) {
	t.Parallel()
	_, err := open(t, &fakeServer{levelErr: errors.New("Error 1146 (HY000): table not found: schema_migrations")})
	if !errors.Is(err, ErrNotBeads) {
		t.Fatalf("err = %v, want ErrNotBeads", err)
	}
	_, err = open(t, &fakeServer{levelErr: errors.New("connection refused")})
	if err == nil || errors.Is(err, ErrNotBeads) {
		t.Fatalf("a failed read must be an error and not ErrNotBeads: %v", err)
	}
}

func TestDBRefusesWritesBeforeTheServer(t *testing.T) {
	t.Parallel()
	s := &fakeServer{level: int64(SchemaVersion)}
	db, err := open(t, s)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.QueryContext(ctx, "UPDATE issues SET status = 'closed'"); !errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("QueryContext(UPDATE) = %v, want ErrNotReadOnly", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT DOLT_COMMIT('-am', 'x')").Scan(&n); !errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("QueryRowContext(dolt_commit) = %v, want ErrNotReadOnly", err)
	}
	if got := s.statements(); len(got) != 1 {
		t.Fatalf("refused statements reached the server: %v", got)
	}
}

func TestReadOnly(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"SELECT COUNT(*) FROM issues",
		"  select id from wisps w LEFT JOIN wisp_labels l ON w.id = l.issue_id",
		"(SELECT 1)",
		"SHOW DATABASES",
		"SELECT commit_hash FROM `hq`.dolt_log ORDER BY date LIMIT 2",
		"SELECT id FROM issues WHERE title LIKE 'a; DELETE FROM issues'",
		"SELECT id FROM issues WHERE description LIKE '%dolt_commit(%'",
		"SELECT id FROM issues WHERE title = 'it''s; fine'",
		"SELECT 1;",
		"SELECT updated_at FROM issues",
	} {
		if err := ReadOnly(q); err != nil {
			t.Errorf("ReadOnly(%q) = %v, want nil", q, err)
		}
	}
	for _, q := range []string{
		"UPDATE issues SET status = 'closed'",
		"DELETE FROM wisps",
		"INSERT INTO labels VALUES ('a', 'b')",
		"REPLACE INTO config VALUES ('k', 'v')",
		"CALL DOLT_COMMIT('-am', 'x')",
		"SELECT DOLT_COMMIT('-am', 'x')",
		"select dolt_reset ('--hard')",
		"SELECT 1; DELETE FROM issues",
		"SELECT * FROM issues INTO OUTFILE '/tmp/x'",
		"SELECT id FROM issues FOR UPDATE",
		"WITH x AS (SELECT 1) DELETE FROM issues",
		"SET @@autocommit = 0",
		"",
	} {
		if err := ReadOnly(q); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("ReadOnly(%q) = %v, want ErrNotReadOnly", q, err)
		}
	}
}
