package cmd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// The status printer's archive-lag reader, over an injected pool opener with a
// fake driver: every failure is shown to the operator rather than counted as
// zero (gt-7nxj4).

// archiveCountServer scripts the one count the reader asks for, and records
// the statement it sent.
type archiveCountServer struct {
	queryErr error
	null     bool
	query    string
	args     []driver.NamedValue
}

// archiveCountDriverID keeps each registration's driver name its own, so the
// parallel subtests never share a fake.
var archiveCountDriverID uint64

func archiveCountOpener(t *testing.T, server *archiveCountServer) func(string) (*sql.DB, error) {
	t.Helper()
	name := fmt.Sprintf("gt_archive_count_fake_%d", atomic.AddUint64(&archiveCountDriverID, 1))
	sql.Register(name, archiveCountDriver{server: server})
	return func(string) (*sql.DB, error) { return sql.Open(name, "") }
}

type archiveCountDriver struct{ server *archiveCountServer }

func (d archiveCountDriver) Open(string) (driver.Conn, error) {
	return archiveCountConn{server: d.server}, nil
}

type archiveCountConn struct{ server *archiveCountServer }

func (archiveCountConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by the archive count")
}

func (archiveCountConn) Close() error { return nil }

func (archiveCountConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the archive count")
}

func (c archiveCountConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.server.query = query
	c.server.args = args
	if c.server.queryErr != nil {
		return nil, c.server.queryErr
	}
	return &archiveCountRows{null: c.server.null}, nil
}

type archiveCountRows struct{ null, done bool }

func (*archiveCountRows) Columns() []string { return []string{"cnt"} }

func (*archiveCountRows) Close() error { return nil }

func (r *archiveCountRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	if r.null {
		return nil // NULL scans into *int64 as an error, not as zero
	}
	dest[0] = int64(3)
	return nil
}

// TestArchiveNotesReportsUnreadableWatermarks: an unreadable state file is an
// error the status prints, and every listed database still carries its state
// as a note rather than going missing from the lines the flatten check reads.
func TestArchiveNotesReportsUnreadableWatermarks(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := filepath.Join(townRoot, "daemon", "maintenance_state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	notes, err := archiveNotes(townRoot, &doltserver.Config{}, []string{"gt", "hq"})
	if err == nil {
		t.Fatal("an unreadable watermark file was read as no watermarks")
	}
	for _, db := range []string{"gt", "hq"} {
		line := databaseStatusLine(db, "", notes[db])
		if !strings.Contains(line, "archive state unreadable") {
			t.Errorf("%s line = %q, want it to say the archive state cannot be read", db, line)
		}
		if strings.Contains(line, "0 commits behind") {
			t.Errorf("%s line = %q, want no reading that could pass the pre-flatten check", db, line)
		}
	}
	if header := archiveUnreadableLine(err); !strings.Contains(header, "Archive watermarks unreadable") || !strings.Contains(header, err.Error()) {
		t.Errorf("header = %q, want the unreadable watermarks named", header)
	}
}

// TestDoltArchiveReaderFailures: an unusable database name, a pool that cannot
// be opened, a failed query and an unreadable count are each the reader's
// error, so the status line shows it instead of "0 behind".
func TestDoltArchiveReaderFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config := &doltserver.Config{}
	commitWM := daemon.ArchiveWatermark{}
	eventsWM := daemon.EventsArchiveWatermark{}

	t.Run("a commit count refuses a database name that is not a plain identifier", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{config: config}
		if _, err := reader.commitsBehind(ctx, "../gt", commitWM); err == nil {
			t.Error("a path-escaping database name was queried")
		}
	})

	t.Run("a commit count reports a pool it cannot open", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) { return nil, errors.New("no pool") },
		}
		if _, err := reader.commitsBehind(ctx, "gt", commitWM); err == nil || !strings.Contains(err.Error(), "no pool") {
			t.Errorf("count with an unopenable pool = %v, want the open error", err)
		}
	})

	t.Run("a commit count reports a count it cannot scan", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{null: true}),
		}
		if _, err := reader.commitsBehind(ctx, "gt", commitWM); err == nil {
			t.Error("a NULL count was read as a number")
		}
	})

	t.Run("a commit count reads the rows behind the archive", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{config: config, openDB: archiveCountOpener(t, &archiveCountServer{})}
		got, err := reader.commitsBehind(ctx, "gt", commitWM)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got != 3 {
			t.Errorf("commits behind = %d, want 3", got)
		}
	})

	t.Run("a commit count with no watermark counts the whole log", func(t *testing.T) {
		t.Parallel()
		server := &archiveCountServer{}
		reader := doltArchiveReader{config: config, openDB: archiveCountOpener(t, server)}
		got, err := reader.commitsBehind(ctx, "gt", daemon.ArchiveWatermark{})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got != 3 {
			t.Errorf("commits behind = %d, want 3", got)
		}
		if strings.Contains(server.query, "WHERE") {
			t.Errorf("never-archived count carried a cursor: %s", server.query)
		}
	})

	t.Run("a commit count refuses a watermark without hashes", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) {
				t.Error("a connection was opened for a watermark with no hashes")
				return nil, errors.New("not reached")
			},
		}
		wm := daemon.ArchiveWatermark{Date: time.Now()}
		if _, err := reader.commitsBehind(ctx, "gt", wm); err == nil || !strings.Contains(err.Error(), "lists no hashes") {
			t.Errorf("count with a hashless watermark = %v, want it refused", err)
		}
	})

	t.Run("a commit count binds the boundary date and the hashes at it", func(t *testing.T) {
		t.Parallel()
		server := &archiveCountServer{}
		reader := doltArchiveReader{config: config, openDB: archiveCountOpener(t, server)}
		when := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
		wm := daemon.ArchiveWatermark{Date: when, Hashes: []string{"a", "b"}}
		if _, err := reader.commitsBehind(ctx, "gt", wm); err != nil {
			t.Fatalf("count: %v", err)
		}
		// The count asks for rows from the cursor's date, then removes the
		// hashes already archived at it: the same cursor the archive reads by.
		if len(server.args) != 3 {
			t.Fatalf("count args = %v, want the date and both hashes", server.args)
		}
		if got, ok := server.args[0].Value.(time.Time); !ok || !got.Equal(when) {
			t.Errorf("first bound = %v, want the watermark's date %v", server.args[0].Value, when)
		}
		if server.args[1].Value != "a" || server.args[2].Value != "b" {
			t.Errorf("hash bounds = %v, want a then b", server.args[1:])
		}
	})

	t.Run("an events count refuses a database name that is not a plain identifier", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{config: config}
		if _, err := reader.eventsBehind(ctx, "../gt", eventsWM); err == nil {
			t.Error("a path-escaping database name was queried")
		}
	})

	t.Run("an events count refuses an uncomparable watermark", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{config: config}
		bad := daemon.EventsArchiveWatermark{CreatedAt: "yesterday", ID: "1"}
		if _, err := reader.eventsBehind(ctx, "gt", bad); err == nil {
			t.Error("a watermark Dolt cannot compare was queried")
		}
	})

	t.Run("an events count reports a pool it cannot open", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) { return nil, errors.New("no pool") },
		}
		if _, err := reader.eventsBehind(ctx, "gt", eventsWM); err == nil || !strings.Contains(err.Error(), "no pool") {
			t.Errorf("count with an unopenable pool = %v, want the open error", err)
		}
	})

	t.Run("an events count reports a query that fails", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{queryErr: errors.New("query failed")}),
		}
		if _, err := reader.eventsBehind(ctx, "gt", eventsWM); err == nil || !strings.Contains(err.Error(), "query failed") {
			t.Errorf("failed count = %v, want the server error", err)
		}
	})

	t.Run("an events count reports a count it cannot scan", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{null: true}),
		}
		if _, err := reader.eventsBehind(ctx, "gt", eventsWM); err == nil {
			t.Error("a NULL count was read as a number")
		}
	})

	t.Run("an events count reads the rows behind the archive", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{config: config, openDB: archiveCountOpener(t, &archiveCountServer{})}
		got, err := reader.eventsBehind(ctx, "gt", eventsWM)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got != 3 {
			t.Errorf("events behind = %d, want 3", got)
		}
	})
}
