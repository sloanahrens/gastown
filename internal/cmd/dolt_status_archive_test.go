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

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// The status printer's archive-lag reader, over an injected pool opener with a
// fake driver: every failure is shown to the operator rather than counted as
// zero (gt-7nxj4).

// archiveCountServer scripts the one count the reader asks for.
type archiveCountServer struct {
	queryErr error
	null     bool
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

func (c archiveCountConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
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
