package cmd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// compareStatement is one statement kind the reader issues, so a fake server
// can be scripted to fail it.
type compareStatement string

const (
	stmtDatabases compareStatement = "SHOW DATABASES"
	stmtTables    compareStatement = "information_schema tables"
	stmtCount     compareStatement = "COUNT(*) of a table"
	stmtHash      compareStatement = "DOLT_HASHOF_DB()"
)

// compareFakeServer answers the reader's statements from memory. Its errs,
// unscannable and useErr maps make one statement fail, so each failure path
// of the reader is reachable without a server.
type compareFakeServer struct {
	databases   []string
	tables      map[string][]string
	counts      map[string]int64
	hashes      map[string]string
	errs        map[compareStatement]error
	unscannable map[compareStatement]bool
	useErr      map[string]error
	connectErr  error
}

// compareFakeDriverID keeps each registration's driver name its own, so tests
// that run in parallel cannot see each other's fake.
var compareFakeDriverID uint64

// newFakeSnapshotReader is a mysqlSnapshotReader over a fake server: it opens
// the fake driver instead of the MySQL one, so no connection leaves the
// process.
func newFakeSnapshotReader(t *testing.T, server *compareFakeServer) *mysqlSnapshotReader {
	t.Helper()
	name := fmt.Sprintf("gt_dolt_compare_fake_%d", atomic.AddUint64(&compareFakeDriverID, 1))
	sql.Register(name, &compareFakeDriver{server: server})
	return &mysqlSnapshotReader{
		label:   "fake:3307",
		timeout: time.Minute,
		openDB: func(string) (*sql.DB, error) {
			return sql.Open(name, "")
		},
	}
}

type compareFakeDriver struct{ server *compareFakeServer }

func (d *compareFakeDriver) Open(string) (driver.Conn, error) {
	if d.server.connectErr != nil {
		return nil, d.server.connectErr
	}
	return &compareFakeConn{server: d.server}, nil
}

// compareFakeConn answers the statements mysqlSnapshotReader issues and tracks
// the session's current database, the way USE does.
type compareFakeConn struct {
	server   *compareFakeServer
	database string
}

func (c *compareFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by the reader")
}

func (c *compareFakeConn) Close() error { return nil }

func (c *compareFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the reader")
}

func (c *compareFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case query == "SHOW DATABASES":
		return c.rowsFor(stmtDatabases, "Database", stringsToValues(c.server.databases))
	case query == doltCompareTableListQuery:
		database, _ := args[0].Value.(string)
		return c.rowsFor(stmtTables, "table_name", stringsToValues(c.server.tables[database]))
	case strings.HasPrefix(query, "SELECT COUNT(*) FROM "):
		table := strings.Trim(strings.TrimPrefix(query, "SELECT COUNT(*) FROM "), "`")
		if err := c.server.errs[stmtCount]; err != nil {
			return nil, err
		}
		return &compareFakeRows{
			cols: []string{"COUNT(*)"},
			rows: [][]driver.Value{{c.server.counts[table]}},
		}, nil
	case query == doltCompareContentHashQuery:
		if err := c.server.errs[stmtHash]; err != nil {
			return nil, err
		}
		return &compareFakeRows{
			cols: []string{"DOLT_HASHOF_DB()"},
			rows: [][]driver.Value{{c.server.hashes[c.database]}},
		}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

// rowsFor answers a listing statement, honouring its scripted failure and its
// scripted unscannable row.
func (c *compareFakeConn) rowsFor(kind compareStatement, column string, rows [][]driver.Value) (driver.Rows, error) {
	if err := c.server.errs[kind]; err != nil {
		return nil, err
	}
	if c.server.unscannable[kind] {
		rows = [][]driver.Value{{nil}}
	}
	return &compareFakeRows{cols: []string{column}, rows: rows}, nil
}

func (c *compareFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if !strings.HasPrefix(query, "USE ") {
		return nil, fmt.Errorf("unexpected statement: %s", query)
	}
	database := strings.Trim(strings.TrimPrefix(query, "USE "), "`")
	if err := c.server.useErr[database]; err != nil {
		return nil, err
	}
	c.database = database
	return driver.RowsAffected(0), nil
}

// stringsToValues is a listing as driver rows.
func stringsToValues(names []string) [][]driver.Value {
	rows := make([][]driver.Value, len(names))
	for i, name := range names {
		rows[i] = []driver.Value{name}
	}
	return rows
}

// compareFakeRows is a scripted result set: a nil value cannot scan into the
// string columns the reader reads, which is how the scan failure is reached.
type compareFakeRows struct {
	cols []string
	rows [][]driver.Value
	next int
}

func (r *compareFakeRows) Columns() []string { return r.cols }

func (r *compareFakeRows) Close() error { return nil }

func (r *compareFakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

// The reader sums the row counts of every base table a database lists and
// reports the database's content hash, per database the server serves.
func TestMySQLSnapshotReaderReadsCountsAndHashes(t *testing.T) {
	t.Parallel()
	reader := newFakeSnapshotReader(t, &compareFakeServer{
		databases: []string{"hq", "gt"},
		tables:    map[string][]string{"hq": {"issues", "comments"}, "gt": {"beads"}},
		counts:    map[string]int64{"issues": 3, "comments": 2, "beads": 5},
		hashes:    map[string]string{"hq": "hash-hq", "gt": "hash-gt"},
	})
	got, err := reader.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []dbSnapshot{
		{Name: "hq", Rows: 5, Hash: "hash-hq"},
		{Name: "gt", Rows: 5, Hash: "hash-gt"},
	}
	if len(got) != len(want) {
		t.Fatalf("Read returned %d snapshots, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("snapshot %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Every failure of a read is an error the caller cannot mistake for a match.
func TestMySQLSnapshotReaderFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		server *compareFakeServer
		open   error
		want   string
	}{
		{
			name:   "the pool cannot be opened",
			server: &compareFakeServer{},
			open:   errors.New("no driver"),
			want:   "opening connection",
		},
		{
			name:   "the server cannot be reached",
			server: &compareFakeServer{connectErr: errors.New("connection refused")},
			want:   "connecting",
		},
		{
			name:   "the database list cannot be read",
			server: &compareFakeServer{errs: map[compareStatement]error{stmtDatabases: errors.New("show failed")}},
			want:   "listing databases",
		},
		{
			name:   "a database name cannot be scanned",
			server: &compareFakeServer{unscannable: map[compareStatement]bool{stmtDatabases: true}},
			want:   "listing databases",
		},
		{
			name:   "a database name is refused",
			server: &compareFakeServer{databases: []string{"bad name"}},
			want:   "refusing identifier",
		},
		{
			name:   "the database cannot be selected",
			server: &compareFakeServer{databases: []string{"hq"}, useErr: map[string]error{"hq": errors.New("unknown database")}},
			want:   "using database hq",
		},
		{
			name:   "the table list cannot be read",
			server: &compareFakeServer{databases: []string{"hq"}, errs: map[compareStatement]error{stmtTables: errors.New("tables failed")}},
			want:   "counting rows in hq",
		},
		{
			name:   "a table name cannot be scanned",
			server: &compareFakeServer{databases: []string{"hq"}, unscannable: map[compareStatement]bool{stmtTables: true}},
			want:   "counting rows in hq",
		},
		{
			name:   "a table name is refused",
			server: &compareFakeServer{databases: []string{"hq"}, tables: map[string][]string{"hq": {"bad name"}}},
			want:   "refusing identifier",
		},
		{
			name:   "a row count cannot be read",
			server: &compareFakeServer{databases: []string{"hq"}, tables: map[string][]string{"hq": {"issues"}}, errs: map[compareStatement]error{stmtCount: errors.New("count failed")}},
			want:   "table issues",
		},
		{
			name:   "the content hash cannot be read",
			server: &compareFakeServer{databases: []string{"hq"}, errs: map[compareStatement]error{stmtHash: errors.New("hash failed")}},
			want:   "reading the content hash of hq",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := newFakeSnapshotReader(t, tt.server)
			if tt.open != nil {
				reader.openDB = func(string) (*sql.DB, error) { return nil, tt.open }
			}
			got, err := reader.Read(context.Background())
			if err == nil {
				t.Fatalf("Read = %+v, want an error", got)
			}
			if got != nil {
				t.Errorf("Read returned %+v beside the error, want no snapshot", got)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should name %q", err, tt.want)
			}
		})
	}
}
