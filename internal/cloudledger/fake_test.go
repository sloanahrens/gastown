package cloudledger_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
)

// The fake ledger is a hand-written database/sql driver, as internal/beadsql's
// tests use: it answers the three table reads and the role probe from memory,
// and records every statement so a test can prove what ran, and in what order,
// before what.
type fakeLedger struct {
	mu         sync.Mutex
	statements []string
	readOnly   bool

	report    *fakeReport
	heartbeat *time.Time
	releases  map[string][]fakeRelease

	// writableTable is the one table the role may INSERT into; probeNull is
	// the table whose privilege read returns NULL; both are refusals.
	writableTable string
	probeNull     string
	// The scan flags return a value of the wrong type in one column, so a
	// caller's scan fails the way a malformed row would.
	reportScanErr  bool
	releaseScanErr bool
	appsScanErr    bool
	// queryErr fails every ledger read and probeErr every probe.
	queryErr error
	probeErr error
}

type fakeReport struct {
	schema             int64
	projects, findings []byte
}

type fakeRelease struct {
	at   time.Time
	line []byte
}

func (l *fakeLedger) record(query string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.statements = append(l.statements, query)
}

// seen returns the statements that reached the ledger so far.
func (l *fakeLedger) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.statements...)
}

// ranReadOnly reports whether the last transaction asked for a read-only one.
func (l *fakeLedger) ranReadOnly() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readOnly
}

// fakes maps each registered DSN to its ledger. The driver is process-wide
// (sql.Register takes one per name), so the DSN is the handle.
var fakes = struct {
	sync.Mutex
	byDSN map[string]*fakeLedger
}{byDSN: map[string]*fakeLedger{}}

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	fakes.Lock()
	l := fakes.byDSN[dsn]
	fakes.Unlock()
	if l == nil {
		return nil, errors.New("fake ledger: no ledger for that dsn")
	}
	return &fakeConn{l: l}, nil
}

func init() { sql.Register(cloudledger.PostgresDriver, fakeDriver{}) }

// dsnSeq numbers the registered DSNs, so two tests running together never
// register over one another as registrations come and go.
var dsnSeq atomic.Int64

// registerDSN registers l under a fresh DSN and returns the 0600 file holding
// it, the way the setting names one.
func registerDSN(t *testing.T, l *fakeLedger) (dsn, path string) {
	t.Helper()
	fakes.Lock()
	dsn = fmt.Sprintf("postgres://reader:%s@fake/ledger%d", "s3krit", dsnSeq.Add(1))
	fakes.byDSN[dsn] = l
	fakes.Unlock()
	t.Cleanup(func() {
		fakes.Lock()
		delete(fakes.byDSN, dsn)
		fakes.Unlock()
	})
	path = filepath.Join(t.TempDir(), "ledger.dsn")
	if err := os.WriteFile(path, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dsn, path
}

type fakeConn struct{ l *fakeLedger }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}

func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

func (c *fakeConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.l.mu.Lock()
	c.l.readOnly = opts.ReadOnly
	c.l.mu.Unlock()
	return fakeTx{}, nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.l.record(query)
	switch {
	case strings.Contains(query, "has_table_privilege"):
		if c.l.probeErr != nil {
			return nil, c.l.probeErr
		}
		table, _ := args[0].Value.(string)
		switch table {
		case c.l.probeNull:
			return &fakeRows{cols: []string{"privilege"}, rows: [][]driver.Value{{nil}}}, nil
		case c.l.writableTable:
			return &fakeRows{cols: []string{"privilege"}, rows: [][]driver.Value{{true}}}, nil
		}
		return &fakeRows{cols: []string{"privilege"}, rows: [][]driver.Value{{false}}}, nil
	case strings.Contains(query, "FROM "+cloudledger.PatrolReportTable):
		if c.l.queryErr != nil {
			return nil, c.l.queryErr
		}
		if c.l.report == nil {
			return &fakeRows{cols: reportColumns}, nil
		}
		if c.l.reportScanErr {
			return &fakeRows{cols: reportColumns, rows: [][]driver.Value{{
				c.l.report.schema, struct{}{}, c.l.report.findings,
			}}}, nil
		}
		return &fakeRows{cols: reportColumns, rows: [][]driver.Value{{
			c.l.report.schema, c.l.report.projects, c.l.report.findings,
		}}}, nil
	case strings.Contains(query, "FROM "+cloudledger.PatrolHeartbeatTable):
		if c.l.queryErr != nil {
			return nil, c.l.queryErr
		}
		if c.l.heartbeat == nil {
			return &fakeRows{cols: []string{"at"}}, nil
		}
		return &fakeRows{cols: []string{"at"}, rows: [][]driver.Value{{*c.l.heartbeat}}}, nil
	case strings.Contains(query, "SELECT DISTINCT app FROM "+cloudledger.ReleaseTable):
		if c.l.queryErr != nil {
			return nil, c.l.queryErr
		}
		names := make([]string, 0, len(c.l.releases))
		for name := range c.l.releases {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([][]driver.Value, 0, len(names))
		for _, name := range names {
			var value driver.Value = name
			if c.l.appsScanErr {
				value = struct{}{} // not a value a string column scans
			}
			rows = append(rows, []driver.Value{value})
		}
		return &fakeRows{cols: []string{"app"}, rows: rows}, nil
	case strings.Contains(query, "FROM "+cloudledger.ReleaseTable):
		if c.l.queryErr != nil {
			return nil, c.l.queryErr
		}
		app, _ := args[0].Value.(string)
		rels := append([]fakeRelease(nil), c.l.releases[app]...)
		sort.Slice(rels, func(i, j int) bool { return rels[i].at.Before(rels[j].at) })
		if len(args) > 1 {
			since, _ := args[1].Value.(time.Time)
			kept := rels[:0]
			for _, r := range rels {
				if r.at.After(since) {
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
			var value driver.Value = r.line
			if c.l.releaseScanErr {
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
