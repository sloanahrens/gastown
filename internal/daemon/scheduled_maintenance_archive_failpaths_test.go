package daemon

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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The failure paths of the archive's readers and writers. The readers run on
// an injected pool opener over a fake driver, so every refusal is reachable
// without a Dolt server (gt-7nxj4).

// refusingWriter fails every write, the way a full disk does.
type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) {
	return 0, errors.New("no space left on device")
}

// TestWriteArchiveMemberReportsWriterFailures: a destination that refuses the
// header, a row, or the flush is an error from the member writer rather than a
// member silently cut short.
func TestWriteArchiveMemberReportsWriterFailures(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 5000) // past bufio's 4 KiB buffer, so the write flushes
	tests := []struct {
		name    string
		columns []string
		rows    [][]string
		newFile bool
	}{
		{"the header cannot be written", []string{big}, nil, true},
		{"a row cannot be written", []string{"a"}, [][]string{{big}}, false},
		{"the flush cannot be written", []string{"a"}, [][]string{{"b"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := writeArchiveMember(refusingWriter{}, tt.columns, tt.rows, tt.newFile); err == nil {
				t.Error("a member the destination refused reported success")
			}
		})
	}
}

// TestAppendArchiveTableRefusesARowWidthMismatch: the table writer refuses a
// row whose values do not line up with its header.
func TestAppendArchiveTableRefusesARowWidthMismatch(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "archive.csv.gz")
	if err := appendArchiveTable(path, []string{"a", "b"}, [][]string{{"1"}}); err == nil {
		t.Error("a row with one value was written against two columns")
	}
}

// truncateFailingTarget refuses both the write and the rollback.
type truncateFailingTarget struct{ closed bool }

func (*truncateFailingTarget) Write([]byte) (int, error) { return 0, errors.New("write refused") }
func (*truncateFailingTarget) Truncate(int64) error      { return errors.New("truncate refused") }
func (*truncateFailingTarget) Sync() error               { return nil }
func (t *truncateFailingTarget) Close() error            { t.closed = true; return nil }

// syncFailingTarget accepts the member and refuses the sync, the way a full
// disk's write-behind does; it records the rollback the sync failure must
// trigger.
type syncFailingTarget struct {
	closed    bool
	truncated int64
}

func (*syncFailingTarget) Write(p []byte) (int, error) { return len(p), nil }
func (t *syncFailingTarget) Truncate(size int64) error {
	t.truncated = size
	return nil
}
func (*syncFailingTarget) Sync() error { return errors.New("sync refused") }
func (t *syncFailingTarget) Close() error {
	t.closed = true
	return nil
}

// TestAppendArchiveMemberReportsTargetFailures: one target whose partial write
// cannot be rolled back, and one whose sync fails, are errors that say so; the
// target is closed either way.
func TestAppendArchiveMemberReportsTargetFailures(t *testing.T) {
	t.Parallel()

	t.Run("the partial write cannot be rolled back", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "archive.csv.gz")
		if err := os.WriteFile(path, []byte("earlier"), 0o644); err != nil {
			t.Fatal(err)
		}
		target := &truncateFailingTarget{}
		err := appendArchiveMember(path, []string{"a"}, [][]string{{"1"}},
			func(string) (archiveAppendTarget, error) { return target, nil })
		if err == nil || !strings.Contains(err.Error(), "could not be rolled back") {
			t.Errorf("append with a failed rollback = %v, want it named", err)
		}
		if !target.closed {
			t.Error("failed append left the target open")
		}
	})

	t.Run("the sync fails", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "archive.csv.gz")
		if err := os.WriteFile(path, []byte("earlier"), 0o644); err != nil {
			t.Fatal(err)
		}
		target := &syncFailingTarget{}
		err := appendArchiveMember(path, []string{"a"}, [][]string{{"1"}},
			func(string) (archiveAppendTarget, error) { return target, nil })
		if err == nil || !strings.Contains(err.Error(), "sync refused") {
			t.Errorf("append with a failed sync = %v, want the sync error", err)
		}
		if target.truncated != 7 {
			t.Errorf("sync failure rolled back to %d, want the pre-append size 7", target.truncated)
		}
		if !target.closed {
			t.Error("failed append left the target open")
		}
	})
}

// TestMaintenanceArchiveExportRefusals: the export refuses a database name it
// cannot use, an uncomparable events watermark, an events table without the
// cursor columns, columns that change between batches, and a row it cannot
// turn into a cursor; each failure is the export's error, never a silent
// "nothing new".
func TestMaintenanceArchiveExportRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	zero := EventsArchiveWatermark{}

	t.Run("a commit export refuses a path-like database name", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		if err := d.maintenanceArchiveCommits(context.Background(), dir, "../gt", ArchiveWatermark{}); err == nil {
			t.Error("a path-escaping database name was exported")
		}
	})

	t.Run("an events export refuses a path-like database name", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		if err := d.maintenanceArchiveEvents(context.Background(), dir, "../gt", zero); err == nil {
			t.Error("a path-escaping database name was exported")
		}
	})

	t.Run("an events export refuses an uncomparable watermark", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		bad := EventsArchiveWatermark{CreatedAt: "yesterday", ID: "1"}
		if err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", bad); err == nil {
			t.Error("a watermark Dolt cannot compare was used")
		}
	})

	t.Run("an events export refuses a table without the cursor columns", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
			return archiveEvents{Columns: []string{"payload"}, Rows: [][]string{{"x"}}}, nil
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "no created_at and id columns") {
			t.Errorf("export of a table without the cursor columns = %v, want it refused", err)
		}
	})

	t.Run("an events export refuses columns that change between batches", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		calls := 0
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
			calls++
			if calls == 1 {
				rows := make([][]string, maintenanceArchiveEventsBatch)
				for i := range rows {
					rows[i] = []string{strconv.Itoa(i + 1), "2026-10-01 00:00:00"}
				}
				return archiveEvents{Columns: []string{"id", "created_at"}, Rows: rows}, nil
			}
			return archiveEvents{
				Columns: []string{"id", "created_at", "payload"},
				Rows:    [][]string{{"1", "2026-10-01 00:00:00", "x"}},
			}, nil
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "columns changed") {
			t.Errorf("export across changing columns = %v, want it refused", err)
		}
	})

	t.Run("an events export refuses a row it cannot turn into a cursor", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
			return archiveEvents{
				Columns: []string{"id", "created_at"},
				Rows:    [][]string{{"1", "yesterday"}},
			}, nil
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "not a timestamp") {
			t.Errorf("export of a row with an unusable cursor = %v, want it refused", err)
		}
	})

	t.Run("an events export reports a watermark it cannot record", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, EventsArchiveWatermark, int) (archiveEvents, error) {
			return archiveEvents{
				Columns: []string{"id", "created_at"},
				Rows:    [][]string{{"1", "2026-10-01 00:00:00"}},
			}, nil
		}
		statePath := maintenanceGCStatePath(d.config.TownRoot)
		if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
			t.Fatal(err)
		}
		// A directory where the state file belongs makes the record fail while
		// the archive file itself can still be written.
		if err := os.Mkdir(statePath, 0o755); err != nil {
			t.Fatal(err)
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "cannot be recorded") {
			t.Errorf("export whose watermark cannot be recorded = %v, want it reported", err)
		}
	})
}

// archiveFakeServer scripts one archive read's result and failures. query and
// args record what the read sent, for a test of the cursor it bound.
type archiveFakeServer struct {
	connectErr error
	queryErr   error
	columns    []string
	rows       [][]driver.Value
	nextErr    error
	query      string
	args       []driver.NamedValue
}

// archiveFakeDriverID keeps each registration's driver name its own, so tests
// that run in parallel cannot see each other's fake.
var archiveFakeDriverID uint64

// archiveFakeOpener registers a fake driver for one test and returns the pool
// opener the daemon should use.
func archiveFakeOpener(t *testing.T, server *archiveFakeServer) func(string) (*sql.DB, error) {
	t.Helper()
	name := fmt.Sprintf("gt_archive_fake_%d", atomic.AddUint64(&archiveFakeDriverID, 1))
	sql.Register(name, archiveFakeDriver{server: server})
	return func(string) (*sql.DB, error) { return sql.Open(name, "") }
}

type archiveFakeDriver struct{ server *archiveFakeServer }

func (d archiveFakeDriver) Open(string) (driver.Conn, error) {
	if d.server.connectErr != nil {
		return nil, d.server.connectErr
	}
	return archiveFakeConn{server: d.server}, nil
}

type archiveFakeConn struct{ server *archiveFakeServer }

func (archiveFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by the archive read")
}

func (archiveFakeConn) Close() error { return nil }

func (archiveFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the archive read")
}

func (c archiveFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.server.query = query
	c.server.args = args
	if c.server.queryErr != nil {
		return nil, c.server.queryErr
	}
	return &archiveFakeRows{server: c.server}, nil
}

type archiveFakeRows struct {
	server *archiveFakeServer
	next   int
}

func (r *archiveFakeRows) Columns() []string { return r.server.columns }

func (r *archiveFakeRows) Close() error { return nil }

func (r *archiveFakeRows) Next(dest []driver.Value) error {
	if r.next < len(r.server.rows) {
		copy(dest, r.server.rows[r.next])
		r.next++
		return nil
	}
	if r.server.nextErr != nil {
		return r.server.nextErr
	}
	return io.EOF
}

// TestEventsArchiveRowsKeepsNullDistinctFromEmpty: a NULL cell and an empty
// string archive as different fields, and a value that is literally the NULL
// marker is escaped rather than read back as a NULL (gt-7nxj4).
func TestEventsArchiveRowsKeepsNullDistinctFromEmpty(t *testing.T) {
	t.Parallel()
	open := archiveFakeOpener(t, &archiveFakeServer{
		columns: []string{"a", "b", "c"},
		rows:    [][]driver.Value{{nil, "", archiveNullCell}},
	})
	db, err := open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT a, b, c FROM events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got, err := eventsArchiveRows(rows)
	if err != nil {
		t.Fatalf("reading rows: %v", err)
	}
	want := []string{archiveNullCell, "", `\` + archiveNullCell}
	if len(got.Rows) != 1 || !slices.Equal(got.Rows[0], want) {
		t.Errorf("row = %v, want %v (NULL, empty, and the escaped literal must differ)", got.Rows, want)
	}
}

// TestEventsArchiveRowsReportsAClosedResultSet: a result set closed before it
// was read is an error, not an empty batch that the watermark would walk past.
func TestEventsArchiveRowsReportsAClosedResultSet(t *testing.T) {
	t.Parallel()
	open := archiveFakeOpener(t, &archiveFakeServer{columns: []string{"id", "created_at"}})
	db, err := open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT id, created_at FROM events")
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := eventsArchiveRows(rows); err == nil {
		t.Error("a closed result set was read as an empty batch")
	}
}

// TestDoltArchiveLogFailures: every failure of the dolt_log read is an error
// the archive cannot mistake for "no new commits"; an unreadable row is not
// dropped silently either.
func TestDoltArchiveLogFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a path-like database name", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		if _, err := d.doltArchiveLog(ctx, "../gt", ArchiveWatermark{}); err == nil {
			t.Error("a path-escaping database name was read")
		}
	})

	t.Run("a pool that cannot be opened", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = func(string) (*sql.DB, error) { return nil, errors.New("no pool") }
		if _, err := d.doltArchiveLog(ctx, "gt", ArchiveWatermark{}); err == nil || !strings.Contains(err.Error(), "no pool") {
			t.Errorf("read with an unopenable pool = %v, want the open error", err)
		}
	})

	t.Run("a read that timed out", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{queryErr: errors.New("i/o timeout")})
		expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		if _, err := d.doltArchiveLog(expired, "gt", ArchiveWatermark{}); err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Errorf("read after its deadline = %v, want it reported as a timeout", err)
		}
	})

	t.Run("a read that failed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{queryErr: errors.New("query failed")})
		if _, err := d.doltArchiveLog(ctx, "gt", ArchiveWatermark{}); err == nil || !strings.Contains(err.Error(), "query failed") {
			t.Errorf("failed read = %v, want the server error", err)
		}
	})

	t.Run("a row that cannot be scanned", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"commit_hash", "committer", "date", "message", "commit_order"},
			rows:    [][]driver.Value{{"h1", "c", "not-a-time", nil, int64(1)}},
		})
		if _, err := d.doltArchiveLog(ctx, "gt", ArchiveWatermark{}); err == nil {
			t.Error("a row whose date is not a time was read")
		}
	})

	t.Run("a row read that failed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"commit_hash", "committer", "date", "message", "commit_order"},
			nextErr: errors.New("row read failed"),
		})
		if _, err := d.doltArchiveLog(ctx, "gt", ArchiveWatermark{}); err == nil || !strings.Contains(err.Error(), "row read failed") {
			t.Errorf("failed row read = %v, want the row error", err)
		}
	})

	t.Run("a read that succeeds", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		when := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"commit_hash", "committer", "date", "message", "commit_order"},
			rows:    [][]driver.Value{{"h1", "c", when, "first", int64(7)}},
		})
		got, err := d.doltArchiveLog(ctx, "gt", ArchiveWatermark{})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 || got[0].Hash != "h1" || !got[0].Date.Equal(when) || got[0].Message != "first" || got[0].Order != 7 {
			t.Errorf("commits = %+v, want the fake row", got)
		}
	})
}

// TestDoltArchiveLogUsesCommitOrder: the cursor is (commit_order, commit_hash),
// not the commit date, so a commit whose date is older than the watermark's
// but whose height is greater is still read (gt-7nxj4).
func TestDoltArchiveLogUsesCommitOrder(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	server := &archiveFakeServer{
		columns: []string{"commit_hash", "committer", "date", "message", "commit_order"},
	}
	d.maint.archiveOpenDB = archiveFakeOpener(t, server)
	wm := ArchiveWatermark{
		Hash:  "h9",
		Date:  time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		Order: 42,
	}

	if _, err := d.doltArchiveLog(context.Background(), "gt", wm); err != nil {
		t.Fatalf("read: %v", err)
	}

	if strings.Contains(server.query, "date >") {
		t.Errorf("query still bounds by date: %s", server.query)
	}
	for _, want := range []string{"commit_order > ?", "ORDER BY commit_order ASC, commit_hash ASC"} {
		if !strings.Contains(server.query, want) {
			t.Errorf("query %q does not hold %q", server.query, want)
		}
	}
	if len(server.args) != 3 || server.args[0].Value != int64(wm.Order) || server.args[1].Value != int64(wm.Order) || server.args[2].Value != wm.Hash {
		t.Errorf("query args = %v, want the watermark's order twice and its hash", server.args)
	}
}

// TestDoltArchiveLogRefusesAWatermarkWithoutOrder: a watermark from before
// commit_order tracking cannot bound the read, so it is refused rather than
// treated as the start of the log (which would export everything twice).
func TestDoltArchiveLogRefusesAWatermarkWithoutOrder(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	d.maint.archiveOpenDB = func(string) (*sql.DB, error) {
		t.Error("a connection was opened for a watermark without a commit_order")
		return nil, errors.New("not reached")
	}
	wm := ArchiveWatermark{Hash: "h9", Date: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	if _, err := d.doltArchiveLog(context.Background(), "gt", wm); err == nil || !strings.Contains(err.Error(), "no commit_order") {
		t.Errorf("read with an orderless watermark = %v, want it refused", err)
	}
}

// TestDoltArchiveEventsFailures: every failure of the events read is an error
// the archive cannot mistake for "no new rows"; a query the package refuses is
// never sent.
func TestDoltArchiveEventsFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	zero := EventsArchiveWatermark{}

	t.Run("a pool that cannot be opened", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = func(string) (*sql.DB, error) { return nil, errors.New("no pool") }
		if _, err := d.doltArchiveEvents(ctx, "gt", zero, 10); err == nil || !strings.Contains(err.Error(), "no pool") {
			t.Errorf("read with an unopenable pool = %v, want the open error", err)
		}
	})

	t.Run("a query that cannot be built", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		// "gt;" passes the maintenance name check but not beadsql's identifier.
		if _, err := d.doltArchiveEvents(ctx, "gt;", zero, 10); err == nil {
			t.Error("a database name beadsql refuses was queried")
		}
	})

	t.Run("a read that timed out", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{queryErr: errors.New("i/o timeout")})
		expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		if _, err := d.doltArchiveEvents(expired, "gt", zero, 10); err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Errorf("read after its deadline = %v, want it reported as a timeout", err)
		}
	})

	t.Run("a read that failed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{queryErr: errors.New("query failed")})
		if _, err := d.doltArchiveEvents(ctx, "gt", zero, 10); err == nil || !strings.Contains(err.Error(), "query failed") {
			t.Errorf("failed read = %v, want the server error", err)
		}
	})

	t.Run("a row that cannot be scanned", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"id", "created_at"},
			rows:    [][]driver.Value{{struct{}{}, "2026-10-01 00:00:00"}},
		})
		if _, err := d.doltArchiveEvents(ctx, "gt", zero, 10); err == nil {
			t.Error("a row whose value is not a string was read")
		}
	})

	t.Run("a row read that failed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"id", "created_at"},
			nextErr: errors.New("row read failed"),
		})
		if _, err := d.doltArchiveEvents(ctx, "gt", zero, 10); err == nil || !strings.Contains(err.Error(), "row read failed") {
			t.Errorf("failed row read = %v, want the row error", err)
		}
	})

	t.Run("a read that succeeds", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"id", "created_at"},
			rows:    [][]driver.Value{{"1", "2026-10-01 00:00:00"}},
		})
		got, err := d.doltArchiveEvents(ctx, "gt", zero, 10)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got.Rows) != 1 || got.Columns[0] != "id" || got.Rows[0][0] != "1" {
			t.Errorf("events = %+v, want the fake row", got)
		}
	})
}
