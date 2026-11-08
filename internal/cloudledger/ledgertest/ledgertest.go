// Package ledgertest is the fake ledger the cloud step's tests read through: a
// hand-written database/sql driver registered under cloudledger.PostgresDriver,
// so a test drives the Postgres source, and the daemon and dashboard above it,
// without a database. It is test-only; no production package imports it.
package ledgertest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
)

// The ledger tables the fake answers for, as cloudledger names them.
const (
	patrolReportTable    = cloudledger.PatrolReportTable
	patrolHeartbeatTable = cloudledger.PatrolHeartbeatTable
	releaseTable         = cloudledger.ReleaseTable
)

// Ledger is one fake ledger. The zero value is an empty ledger whose role can
// only read; each field set makes the ledger answer that way.
type Ledger struct {
	// Report is the newest patrol report row; nil is an empty table.
	Report *ReportRow
	// Heartbeat is the newest heartbeat; nil is an empty table.
	Heartbeat *time.Time
	// Releases holds each app's release rows, in any order.
	Releases map[string][]ReleaseRow

	// WritableTable is the one table the role may INSERT into: a refusal.
	WritableTable string
	// ProbeNull is the table whose privilege read returns NULL: a refusal.
	ProbeNull string

	// The scan flags return a value of the wrong type in one column, so a
	// caller's scan fails the way a malformed row would.
	ReportScanErr  bool
	ReleaseScanErr bool
	AppsScanErr    bool

	// QueryErr fails every ledger read and ProbeErr every privilege probe.
	QueryErr error
	ProbeErr error
	// Block makes every transaction wait for its context and then fail with
	// the context's error: a database that does not answer.
	Block bool
	// CloseErr is what closing a connection to this ledger returns: a pool that
	// will not close.
	CloseErr error

	mu         sync.Mutex
	statements []string
	readOnly   bool
	opens      int
}

// ReportRow is the patrol report row: the schema version and the JSON of the
// projects and findings columns.
type ReportRow struct {
	SchemaVersion int64
	Projects      []byte
	Findings      []byte
}

// ReleaseRow is one release row: its time and the raw ledger line.
type ReleaseRow struct {
	At   time.Time
	Line []byte
}

// Statements returns the statements that reached the ledger so far, in order.
func (l *Ledger) Statements() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.statements...)
}

// ReadOnly reports whether the last transaction asked for a read-only one.
func (l *Ledger) ReadOnly() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readOnly
}

// Opens returns how many connections database/sql opened to this ledger.
func (l *Ledger) Opens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens
}

func (l *Ledger) record(query string) {
	calls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.statements = append(l.statements, query)
}

// calls counts every connection opened and every statement run against any
// fake ledger, so a test can prove a path touched no database at all.
var calls atomic.Int64

// Calls returns the connections opened and statements run across every fake
// ledger in this process.
func Calls() int64 { return calls.Load() }

// fakes maps each registered DSN to its ledger. The driver is process-wide
// (sql.Register takes one per name), so the DSN is the handle.
var fakes = struct {
	sync.Mutex
	byDSN map[string]*Ledger
}{byDSN: map[string]*Ledger{}}

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	fakes.Lock()
	l := fakes.byDSN[dsn]
	fakes.Unlock()
	if l == nil {
		return nil, errors.New("fake ledger: no ledger for that dsn")
	}
	l.mu.Lock()
	l.opens++
	l.mu.Unlock()
	calls.Add(1)
	return &fakeConn{l: l}, nil
}

// registerOnce registers the fake under cloudledger.PostgresDriver the first
// time a test needs it. It is not done in init: a binary that links the real
// driver registers the same name, and a second sql.Register panics at start.
// taken records that the real driver got there first, so a test names the
// clash instead of reading the real database.
var (
	registerOnce sync.Once
	taken        bool
)

func registerFake() {
	registerOnce.Do(func() {
		if slices.Contains(sql.Drivers(), cloudledger.PostgresDriver) {
			taken = true
			return
		}
		sql.Register(cloudledger.PostgresDriver, fakeDriver{})
	})
}

// dsnSeq numbers the registered DSNs, so two tests running together never
// register over one another as registrations come and go.
var dsnSeq atomic.Int64

// Register registers l under a fresh DSN and returns it with the 0600 file
// holding it, the way the setting names one.
func Register(t testing.TB, l *Ledger) (dsn, path string) {
	t.Helper()
	return RegisterDSN(t, l, fmt.Sprintf("postgres://reader:%s@fake/ledger%d", "s3krit", dsnSeq.Add(1)))
}

// RegisterDSN registers l under dsn and returns dsn with the 0600 file holding
// it. A test that needs a particular password in the DSN passes it here.
func RegisterDSN(t testing.TB, l *Ledger, dsn string) (string, string) {
	t.Helper()
	registerFake()
	if taken {
		t.Fatalf("ledgertest: database/sql driver %q is already registered by the real driver; this binary cannot run the fake ledger", cloudledger.PostgresDriver)
	}
	fakes.Lock()
	fakes.byDSN[dsn] = l
	fakes.Unlock()
	t.Cleanup(func() {
		fakes.Lock()
		delete(fakes.byDSN, dsn)
		fakes.Unlock()
	})
	path := filepath.Join(t.TempDir(), "ledger.dsn")
	if err := os.WriteFile(path, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dsn, path
}

type fakeConn struct{ l *Ledger }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}

func (c *fakeConn) Close() error              { return c.l.CloseErr }
func (c *fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

func (c *fakeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.l.Block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c.l.mu.Lock()
	c.l.readOnly = opts.ReadOnly
	c.l.mu.Unlock()
	return fakeTx{}, nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.l.record(query)
	if c.l.QueryErr != nil && !strings.Contains(query, "has_table_privilege") {
		return nil, c.l.QueryErr
	}
	switch {
	case strings.Contains(query, "has_table_privilege"):
		if c.l.ProbeErr != nil {
			return nil, c.l.ProbeErr
		}
		table, _ := args[0].Value.(string)
		switch table {
		case c.l.ProbeNull:
			return &fakeRows{cols: []string{"privilege"}, rows: [][]driver.Value{{nil}}}, nil
		case c.l.WritableTable:
			return &fakeRows{cols: []string{"privilege"}, rows: [][]driver.Value{{true}}}, nil
		}
		return &fakeRows{cols: []string{"privilege"}, rows: [][]driver.Value{{false}}}, nil
	case strings.Contains(query, "FROM "+patrolReportTable):
		if c.l.Report == nil {
			return &fakeRows{cols: reportColumns}, nil
		}
		if c.l.ReportScanErr {
			return &fakeRows{cols: reportColumns, rows: [][]driver.Value{{
				c.l.Report.SchemaVersion, struct{}{}, c.l.Report.Findings,
			}}}, nil
		}
		return &fakeRows{cols: reportColumns, rows: [][]driver.Value{{
			c.l.Report.SchemaVersion, c.l.Report.Projects, c.l.Report.Findings,
		}}}, nil
	case strings.Contains(query, "FROM "+patrolHeartbeatTable):
		if c.l.Heartbeat == nil {
			return &fakeRows{cols: []string{"at"}}, nil
		}
		return &fakeRows{cols: []string{"at"}, rows: [][]driver.Value{{*c.l.Heartbeat}}}, nil
	case strings.Contains(query, "SELECT DISTINCT app FROM "+releaseTable):
		names := make([]string, 0, len(c.l.Releases))
		for name := range c.l.Releases {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([][]driver.Value, 0, len(names))
		for _, name := range names {
			var value driver.Value = name
			if c.l.AppsScanErr {
				value = struct{}{} // not a value a string column scans
			}
			rows = append(rows, []driver.Value{value})
		}
		return &fakeRows{cols: []string{"app"}, rows: rows}, nil
	case strings.Contains(query, "FROM "+releaseTable):
		app, _ := args[0].Value.(string)
		rels := append([]ReleaseRow(nil), c.l.Releases[app]...)
		sort.Slice(rels, func(i, j int) bool { return rels[i].At.Before(rels[j].At) })
		if len(args) > 1 {
			since, _ := args[1].Value.(time.Time)
			kept := rels[:0]
			for _, r := range rels {
				if r.At.After(since) {
					kept = append(kept, r)
				}
			}
			rels = kept
		} else if len(rels) > 1 {
			rels = rels[len(rels)-1:]
		}
		if len(rels) == 0 {
			return &fakeRows{cols: []string{"line"}}, nil
		}
		rows := make([][]driver.Value, 0, len(rels))
		for _, r := range rels {
			var value driver.Value = r.Line
			if c.l.ReleaseScanErr {
				value = struct{}{} // not a value a byte column scans
			}
			rows = append(rows, []driver.Value{value})
		}
		return &fakeRows{cols: []string{"line"}, rows: rows}, nil
	}
	return nil, fmt.Errorf("fake ledger: unexpected query %q", query)
}

func (c *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, errors.New("the reader never writes")
}

var reportColumns = []string{"schema_version", "projects", "findings"}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

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
