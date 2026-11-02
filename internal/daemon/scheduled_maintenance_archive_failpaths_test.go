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

	t.Run("a commit export refuses a commit with no date", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			return []archiveCommit{{Hash: "h1", Committer: "c", Message: "first"}}, nil // zero Date
		}
		err := d.maintenanceArchiveCommits(context.Background(), t.TempDir(), "gt", ArchiveWatermark{})
		if err == nil || !strings.Contains(err.Error(), "with no date") {
			t.Errorf("commit without a date = %v, want it refused", err)
		}
	})

	t.Run("a commit export refuses a commit with no hash", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			return []archiveCommit{{Committer: "c", Date: gcTestNow, Message: "first"}}, nil
		}
		err := d.maintenanceArchiveCommits(context.Background(), t.TempDir(), "gt", ArchiveWatermark{})
		if err == nil || !strings.Contains(err.Error(), "with no hash") {
			t.Errorf("commit without a hash = %v, want it refused", err)
		}
	})

	t.Run("a commit export refuses a commit dated before the watermark", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			return []archiveCommit{{Hash: "h1", Committer: "c", Date: gcTestNow.Add(-48 * time.Hour), Message: "stale"}}, nil
		}
		wm := ArchiveWatermark{Date: gcTestNow.Add(-24 * time.Hour), Hashes: []string{"old"}}
		err := d.maintenanceArchiveCommits(context.Background(), t.TempDir(), "gt", wm)
		if err == nil || !strings.Contains(err.Error(), "before the watermark") {
			t.Errorf("commit dated below the cursor = %v, want it refused rather than written where the cursor cannot pass it", err)
		}
	})

	t.Run("a commit export refuses an unusable watermark", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			t.Error("a read ran with a watermark the cursor cannot use")
			return nil, nil
		}
		err := d.maintenanceArchiveCommits(context.Background(), t.TempDir(), "gt", ArchiveWatermark{Date: gcTestNow})
		if err == nil || !strings.Contains(err.Error(), "lists no hashes") {
			t.Errorf("export with a hashless watermark = %v, want it refused", err)
		}
	})

	t.Run("a commit export reports an archive path it cannot stat", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveRead = func(context.Context, *Daemon, string, ArchiveWatermark) ([]archiveCommit, error) {
			return []archiveCommit{{Hash: "h1", Committer: "c", Date: gcTestNow, Message: "first"}}, nil
		}
		parent := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := d.maintenanceArchiveCommits(context.Background(), parent, "gt", ArchiveWatermark{}); err == nil {
			t.Error("a commit export used an archive path under a regular file")
		}
	})

	t.Run("an events export reports an archive path it cannot stat", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		parent := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := d.maintenanceArchiveEvents(context.Background(), parent, "gt", zero); err == nil {
			t.Error("an events export used an archive path under a regular file")
		}
	})

	t.Run("an events export refuses an uncomparable watermark", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		bad := EventsArchiveWatermark{
			CreatedAt: "yesterday",
			Archived:  []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "1"}},
		}
		if err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", bad); err == nil {
			t.Error("a watermark Dolt cannot compare was used")
		}
	})

	t.Run("an events export refuses a watermark whose date cannot be placed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		// TimestampLiteral accepts this shape; the parser that computes the
		// window does not, so the export cannot start.
		bad := EventsArchiveWatermark{
			CreatedAt: "2026-13-45 99:99:99",
			Archived:  []ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "1"}},
		}
		if err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", bad); err == nil {
			t.Error("a watermark with an impossible date was used")
		}
	})

	t.Run("an events export refuses a table without the watermark columns", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
			return archiveEvents{Columns: []string{"payload"}, Rows: [][]string{{"x"}}}, nil
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "no created_at and id columns") {
			t.Errorf("export of a table without the watermark columns = %v, want it refused", err)
		}
	})

	t.Run("an events export refuses a row with no id value", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
			return archiveEvents{Columns: []string{"id", "created_at"}, Rows: [][]string{{}}}, nil
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "missing column") {
			t.Errorf("export of a row with no id value = %v, want it refused", err)
		}
	})

	t.Run("an events export refuses a row narrower than its header", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
			return archiveEvents{
				Columns: []string{"id", "created_at"},
				Rows:    [][]string{{"1"}},
			}, nil
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "1 values") {
			t.Errorf("export of a short row = %v, want it refused", err)
		}
	})

	t.Run("an events export refuses a row it cannot place in the window", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
			return archiveEvents{
				Columns: []string{"id", "created_at"},
				Rows:    [][]string{{"1", "yesterday"}},
			}, nil
		}
		err := d.maintenanceArchiveEvents(context.Background(), dir, "gt", zero)
		if err == nil || !strings.Contains(err.Error(), "not a timestamp") {
			t.Errorf("export of a row with an unusable created_at = %v, want it refused", err)
		}
	})

	t.Run("an events export reports a watermark it cannot record", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveEventsRead = func(context.Context, *Daemon, string, string) (archiveEvents, error) {
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
			columns: []string{"commit_hash", "committer", "date", "message"},
			rows:    [][]driver.Value{{"h1", "c", "not-a-time", nil}},
		})
		if _, err := d.doltArchiveLog(ctx, "gt", ArchiveWatermark{}); err == nil {
			t.Error("a row whose date is not a time was read")
		}
	})

	t.Run("a row read that failed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"commit_hash", "committer", "date", "message"},
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
			columns: []string{"commit_hash", "committer", "date", "message"},
			rows:    [][]driver.Value{{"h1", "c", when, "first"}},
		})
		got, err := d.doltArchiveLog(ctx, "gt", ArchiveWatermark{})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 || got[0].Hash != "h1" || !got[0].Date.Equal(when) || got[0].Message != "first" {
			t.Errorf("commits = %+v, want the fake row", got)
		}
	})
}

// TestDoltArchiveLogReadsPastItsCursor: the read binds the watermark's date
// and drops the commits the watermark already holds at that date, so the
// boundary commits are neither repeated nor hidden behind the cursor.
func TestDoltArchiveLogReadsPastItsCursor(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	boundary := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	later := boundary.Add(time.Hour)
	server := &archiveFakeServer{
		columns: []string{"commit_hash", "committer", "date", "message"},
		rows: [][]driver.Value{
			{"archived", "c", boundary, "already out"},
			{"same-second", "c", boundary, "new, same date"},
			{"after", "c", later, "new, later date"},
		},
	}
	d.maint.archiveOpenDB = archiveFakeOpener(t, server)
	wm := ArchiveWatermark{Date: boundary, Hashes: []string{"archived"}}

	got, err := d.doltArchiveLog(context.Background(), "gt", wm)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var hashes []string
	for _, c := range got {
		hashes = append(hashes, c.Hash)
	}
	if !slices.Equal(hashes, []string{"same-second", "after"}) {
		t.Errorf("commits read = %v, want the boundary commit already archived dropped and both new commits kept", hashes)
	}
	for _, want := range []string{"date > ?", "date = ?", "commit_hash NOT IN (?)", "ORDER BY date ASC, commit_hash ASC",
		fmt.Sprintf("LIMIT %d", maintenanceArchiveCommitLimit)} {
		if !strings.Contains(server.query, want) {
			t.Errorf("read query = %q, want it to hold %q", server.query, want)
		}
	}
	if len(server.args) != 3 ||
		!server.args[0].Value.(time.Time).Equal(boundary) ||
		!server.args[1].Value.(time.Time).Equal(boundary) ||
		server.args[2].Value.(string) != "archived" {
		t.Errorf("read args = %v, want the date twice and the archived boundary hash: a full limit of archived rows must not stall the read", server.args)
	}
}

// TestDoltArchiveLogQueryIsBoundedWithNoWatermark: a first read of the whole
// history still carries the limit, so a large log is read in bounded runs
// rather than all at once (gt-98eug).
func TestDoltArchiveLogQueryIsBoundedWithNoWatermark(t *testing.T) {
	t.Parallel()
	query, args, err := doltArchiveLogQuery(ArchiveWatermark{})
	if err != nil {
		t.Fatalf("building the query: %v", err)
	}
	if strings.Contains(query, "WHERE") {
		t.Errorf("query = %q, want no date bound for an empty watermark", query)
	}
	if !strings.HasSuffix(query, fmt.Sprintf("LIMIT %d", maintenanceArchiveCommitLimit)) {
		t.Errorf("query = %q, want it bounded by the commit limit", query)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// TestDoltArchiveLogRefusesAWatermarkWithoutHashes: a watermark with a date
// but no hashes cannot say which boundary commits are archived, so it is
// refused rather than treated as the start of the log (which would export them
// twice).
func TestDoltArchiveLogRefusesAWatermarkWithoutHashes(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	d.maint.archiveOpenDB = func(string) (*sql.DB, error) {
		t.Error("a connection was opened for a watermark with no hashes")
		return nil, errors.New("not reached")
	}
	wm := ArchiveWatermark{Date: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	if _, err := d.doltArchiveLog(context.Background(), "gt", wm); err == nil || !strings.Contains(err.Error(), "lists no hashes") {
		t.Errorf("read with a hashless watermark = %v, want it refused", err)
	}
}

// TestDoltArchiveLogKeepsNullMessageDistinctFromEmpty: a commit whose message
// is NULL and one whose message is empty string archive as different fields,
// as the events archive does (gt-7nxj4).
func TestDoltArchiveLogKeepsNullMessageDistinctFromEmpty(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	when := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{
		columns: []string{"commit_hash", "committer", "date", "message"},
		rows: [][]driver.Value{
			{"h1", "c", when, nil},
			{"h2", "c", when, ""},
		},
	})
	got, err := d.doltArchiveLog(context.Background(), "gt", ArchiveWatermark{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 || got[0].Message != archiveNullCell || got[1].Message != "" {
		t.Errorf("messages = %v, want NULL as %q and empty as a distinct empty field", got, archiveNullCell)
	}
}

// TestDoltArchiveEventsFailures: every failure of the events read is an error
// the archive cannot mistake for "no new rows"; a query the package refuses is
// never sent.
func TestDoltArchiveEventsFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const since = ""

	t.Run("a pool that cannot be opened", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = func(string) (*sql.DB, error) { return nil, errors.New("no pool") }
		if _, err := d.doltArchiveEvents(ctx, "gt", since); err == nil || !strings.Contains(err.Error(), "no pool") {
			t.Errorf("read with an unopenable pool = %v, want the open error", err)
		}
	})

	t.Run("a query that cannot be built", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		// "gt;" passes the maintenance name check but not beadsql's identifier.
		if _, err := d.doltArchiveEvents(ctx, "gt;", since); err == nil {
			t.Error("a database name beadsql refuses was queried")
		}
	})

	t.Run("a read that timed out", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{queryErr: errors.New("i/o timeout")})
		expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		if _, err := d.doltArchiveEvents(expired, "gt", since); err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Errorf("read after its deadline = %v, want it reported as a timeout", err)
		}
	})

	t.Run("a read that failed", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		d.maint.archiveOpenDB = archiveFakeOpener(t, &archiveFakeServer{queryErr: errors.New("query failed")})
		if _, err := d.doltArchiveEvents(ctx, "gt", since); err == nil || !strings.Contains(err.Error(), "query failed") {
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
		if _, err := d.doltArchiveEvents(ctx, "gt", since); err == nil {
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
		if _, err := d.doltArchiveEvents(ctx, "gt", since); err == nil || !strings.Contains(err.Error(), "row read failed") {
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
		got, err := d.doltArchiveEvents(ctx, "gt", since)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got.Rows) != 1 || got.Columns[0] != "id" || got.Rows[0][0] != "1" {
			t.Errorf("events = %+v, want the fake row", got)
		}
	})
}

// TestUnarchivedEvents: the status count reads an open events result set with
// the archive's own scanner, counts only the ids the watermark does not hold,
// and refuses a result set it cannot read or one without the key columns
// rather than reporting zero (gt-98eug).
func TestUnarchivedEvents(t *testing.T) {
	t.Parallel()

	t.Run("counts only ids outside the archived set", func(t *testing.T) {
		t.Parallel()
		db, err := archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"id", "created_at"},
			rows: [][]driver.Value{
				{"archived", "2026-10-01 00:00:00"},
				{"late", "2026-10-01 00:00:00"},
			},
		})("")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.QueryContext(context.Background(), "SELECT * FROM events")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got, err := UnarchivedEvents(rows, map[string]bool{"archived": true})
		if err != nil {
			t.Fatalf("counting: %v", err)
		}
		if got != 1 {
			t.Errorf("unarchived events = %d, want the one id outside the set", got)
		}
	})

	t.Run("a result set it cannot read is refused", func(t *testing.T) {
		t.Parallel()
		db, err := archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"id", "created_at"},
			nextErr: errors.New("row read failed"),
		})("")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.QueryContext(context.Background(), "SELECT * FROM events")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if _, err := UnarchivedEvents(rows, nil); err == nil || !strings.Contains(err.Error(), "row read failed") {
			t.Errorf("count of an unreadable result set = %v, want the read error", err)
		}
	})

	t.Run("a result set without the key columns is refused", func(t *testing.T) {
		t.Parallel()
		db, err := archiveFakeOpener(t, &archiveFakeServer{
			columns: []string{"payload"},
			rows:    [][]driver.Value{{"x"}},
		})("")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.QueryContext(context.Background(), "SELECT * FROM events")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if _, err := UnarchivedEvents(rows, nil); err == nil {
			t.Error("a result set without created_at and id was counted")
		}
	})
}
