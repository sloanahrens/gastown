package cmd

import (
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/csv"
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

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	gmssql "github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
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
	empty    bool
	// columns and rows, when set, are served as the result set: the events
	// window's rows. hashes, when set, is served as one text column: the
	// commit hashes a dolt_log read returns. Otherwise the query serves the
	// count 3.
	columns []string
	rows    [][]driver.Value
	hashes  []string
	nextErr error
	query   string
	args    []driver.NamedValue
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
	return &archiveCountRows{
		null:    c.server.null,
		empty:   c.server.empty,
		columns: c.server.columns,
		rows:    c.server.rows,
		hashes:  c.server.hashes,
		nextErr: c.server.nextErr,
	}, nil
}

// archivedSet is the archived-hash loader a test injects: it returns exactly
// the hashes the archive holds.
func archivedSet(hashes ...string) func(db string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		set[hash] = struct{}{}
	}
	return func(string) (map[string]struct{}, error) { return set, nil }
}

type archiveCountRows struct {
	null    bool
	empty   bool
	columns []string
	rows    [][]driver.Value
	hashes  []string
	nextErr error
	next    int
}

func (r *archiveCountRows) Columns() []string {
	if len(r.columns) > 0 {
		return r.columns
	}
	return []string{"value"}
}

func (*archiveCountRows) Close() error { return nil }

func (r *archiveCountRows) Next(dest []driver.Value) error {
	if r.nextErr != nil {
		return r.nextErr
	}
	if r.empty {
		return io.EOF
	}
	if r.rows != nil {
		if r.next >= len(r.rows) {
			return io.EOF
		}
		copy(dest, r.rows[r.next])
		r.next++
		return nil
	}
	if r.hashes != nil {
		if r.next >= len(r.hashes) {
			return io.EOF
		}
		dest[0] = r.hashes[r.next]
		r.next++
		return nil
	}
	if r.next > 0 {
		return io.EOF
	}
	r.next++
	if r.null {
		return nil // NULL scans into *int64 (or *string) as an error, not as zero
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
	notes, err := archiveNotes(townRoot, stubArchiveBehindReader{}, []string{"gt", "hq"})
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
			config:         config,
			archivedHashes: archivedSet(),
			openDB:         func(string) (*sql.DB, error) { return nil, errors.New("no pool") },
		}
		if _, err := reader.commitsBehind(ctx, "gt", commitWM); err == nil || !strings.Contains(err.Error(), "no pool") {
			t.Errorf("count with an unopenable pool = %v, want the open error", err)
		}
	})

	t.Run("a commit count reports a hash it cannot scan", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config:         config,
			archivedHashes: archivedSet(),
			openDB:         archiveCountOpener(t, &archiveCountServer{null: true}),
		}
		if _, err := reader.commitsBehind(ctx, "gt", commitWM); err == nil {
			t.Error("a NULL hash was read as an archived commit")
		}
	})

	t.Run("a commit count reports a query that fails", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config:         config,
			archivedHashes: archivedSet(),
			openDB:         archiveCountOpener(t, &archiveCountServer{queryErr: errors.New("query failed")}),
		}
		if _, err := reader.commitsBehind(ctx, "gt", commitWM); err == nil || !strings.Contains(err.Error(), "query failed") {
			t.Errorf("failed count = %v, want the server error", err)
		}
	})

	t.Run("a commit count reports a row read that fails", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config:         config,
			archivedHashes: archivedSet(),
			openDB:         archiveCountOpener(t, &archiveCountServer{nextErr: errors.New("row read failed")}),
		}
		if _, err := reader.commitsBehind(ctx, "gt", commitWM); err == nil || !strings.Contains(err.Error(), "row read failed") {
			t.Errorf("failed row read = %v, want the row error", err)
		}
	})

	t.Run("a commit count refuses a watermark without hashes", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config:         config,
			archivedHashes: archivedSet(),
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

	t.Run("a commit count without an archive cannot call the log safe", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) {
				t.Error("a connection was opened with no archive to check against")
				return nil, errors.New("not reached")
			},
		}
		if _, err := reader.commitsBehind(ctx, "gt", commitWM); err == nil {
			t.Error("a reader with no archived-hash set reported a count")
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
		bad := daemon.EventsArchiveWatermark{
			CreatedAt: "yesterday",
			Archived:  []daemon.ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "1"}},
		}
		if _, err := reader.eventsBehind(ctx, "gt", bad); err == nil {
			t.Error("a watermark Dolt cannot compare was queried")
		}
	})

	t.Run("an events count refuses a name beadsql rejects", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) {
				t.Error("a connection was opened for a name beadsql refuses")
				return nil, errors.New("not reached")
			},
		}
		if _, err := reader.eventsBehind(ctx, "gt;", eventsWM); err == nil {
			t.Error("a database name beadsql refuses was counted")
		}
	})

	t.Run("an events count refuses a name the DSN check rejects", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) {
				t.Error("a connection was opened for a hyphenated name")
				return nil, errors.New("not reached")
			},
		}
		if _, err := reader.eventsBehind(ctx, "gt-2", eventsWM); err == nil {
			t.Error("a hyphenated database name was queried")
		}
	})

	t.Run("an events count refuses a date the parser cannot place", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) {
				t.Error("a connection was opened for a date the parser rejects")
				return nil, errors.New("not reached")
			},
		}
		// TimestampLiteral accepts this shape; no calendar has it.
		bad := daemon.EventsArchiveWatermark{
			CreatedAt: "2026-13-45 99:99:99",
			Archived:  []daemon.ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "1"}},
		}
		if _, err := reader.eventsBehind(ctx, "gt", bad); err == nil {
			t.Error("an impossible date was turned into a window")
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

	t.Run("a no-watermark events count is an existence check, not a COUNT", func(t *testing.T) {
		t.Parallel()
		server := &archiveCountServer{}
		reader := doltArchiveReader{config: config, openDB: archiveCountOpener(t, server)}
		got, err := reader.eventsBehind(ctx, "gt", eventsWM)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got != eventsBehindUncounted {
			t.Errorf("events behind = %d, want %d (rows exist, not counted)", got, eventsBehindUncounted)
		}
		if strings.Contains(strings.ToUpper(server.query), "COUNT") {
			t.Errorf("no-watermark count ran a full count: %s", server.query)
		}
		if !strings.Contains(server.query, "LIMIT 1") {
			t.Errorf("no-watermark count is not an existence check: %s", server.query)
		}
	})

	t.Run("a no-watermark events count of an empty table is zero", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{config: config, openDB: archiveCountOpener(t, &archiveCountServer{empty: true})}
		got, err := reader.eventsBehind(ctx, "gt", eventsWM)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got != 0 {
			t.Errorf("events behind = %d, want 0 for an empty table", got)
		}
	})

	t.Run("an events count subtracts the ids in the lookback window", func(t *testing.T) {
		t.Parallel()
		server := &archiveCountServer{
			columns: []string{"id", "created_at"},
			rows: [][]driver.Value{
				{"archived", "2026-10-01 00:00:00"},
				{"late", "2026-10-01 00:00:00"},
			},
		}
		reader := doltArchiveReader{config: config, openDB: archiveCountOpener(t, server)}
		wm := daemon.EventsArchiveWatermark{
			CreatedAt: "2026-11-02 09:15:00",
			Archived:  []daemon.ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "archived"}},
		}
		got, err := reader.eventsBehind(ctx, "gt", wm)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got != 1 {
			t.Errorf("events behind = %d, want the one id outside the set", got)
		}
		// The window itself is exercised against a real engine by
		// TestArchiveEventsBehindQueryReadsTheLookbackWindow; here the fake only has
		// to prove the count subtracts the archived ids.
	})

	windowed := daemon.EventsArchiveWatermark{
		CreatedAt: "2026-11-02 09:15:00",
		Archived:  []daemon.ArchivedEventsRow{{CreatedAt: "2026-11-02 09:15:00", ID: "archived"}},
	}
	t.Run("a windowed events count reports a pool it cannot open", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: func(string) (*sql.DB, error) { return nil, errors.New("no pool") },
		}
		if _, err := reader.eventsBehind(ctx, "gt", windowed); err == nil || !strings.Contains(err.Error(), "no pool") {
			t.Errorf("count with an unopenable pool = %v, want the open error", err)
		}
	})

	t.Run("a windowed events count reports a query that fails", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{queryErr: errors.New("query failed")}),
		}
		if _, err := reader.eventsBehind(ctx, "gt", windowed); err == nil || !strings.Contains(err.Error(), "query failed") {
			t.Errorf("failed count = %v, want the server error", err)
		}
	})

	t.Run("a windowed events count refuses a result set without an id column", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{
				columns: []string{"payload"},
				rows:    [][]driver.Value{{"x"}},
			}),
		}
		if _, err := reader.eventsBehind(ctx, "gt", windowed); err == nil || !strings.Contains(err.Error(), "no created_at and id columns") {
			t.Errorf("count of a result set without id = %v, want it refused", err)
		}
	})

	t.Run("a windowed events count reports a row it cannot scan", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{
				columns: []string{"id", "created_at"},
				rows:    [][]driver.Value{{struct{}{}, "2026-10-01 00:00:00"}},
			}),
		}
		if _, err := reader.eventsBehind(ctx, "gt", windowed); err == nil {
			t.Error("a row whose id is not a string was read")
		}
	})

	t.Run("a windowed events count reports a row read that fails", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{
				columns: []string{"id", "created_at"},
				nextErr: errors.New("row read failed"),
			}),
		}
		if _, err := reader.eventsBehind(ctx, "gt", windowed); err == nil || !strings.Contains(err.Error(), "row read failed") {
			t.Errorf("failed row read = %v, want the row error", err)
		}
	})
}

// writeCommitArchive writes one gzip CSV archive file for a test.
func writeCommitArchive(t *testing.T, path string, hashes []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	w := csv.NewWriter(gz)
	if err := w.Write([]string{"commit_hash", "committer", "date", "message"}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range hashes {
		if err := w.Write([]string{hash, "c", "2026-11-02T00:00:00Z", "m"}); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeDaemonConfig writes the daemon patrol config a test reads the archive
// directory from.
func writeDaemonConfig(t *testing.T, townRoot, body string) {
	t.Helper()
	path := daemon.PatrolConfigFile(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestArchivedHashesLoaderReadsTheConfiguredArchive: the pre-flatten check
// reads its set through the daemon config's maintenance.archive_dir, the same
// directory the archive writes; an unset setting or a config that cannot be
// read is an error, so the status shows it rather than the zero that would
// pass the check.
func TestArchivedHashesLoaderReadsTheConfiguredArchive(t *testing.T) {
	t.Parallel()

	t.Run("reads the files the archive wrote", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := t.TempDir()
		writeDaemonConfig(t, townRoot, fmt.Sprintf(
			`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":%q}}}`,
			dir))
		writeCommitArchive(t, filepath.Join(dir, "dolt-log-gt-20261102.csv.gz"), []string{"h1", "h2"})

		got, err := archivedHashesLoader(townRoot, []string{"gt"})("gt")
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("archived hashes = %v, want h1 and h2", got)
		}
	})

	t.Run("an unset setting is an error, not an empty set", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot,
			`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00"}}}`)
		if _, err := archivedHashesLoader(townRoot, []string{"gt"})("gt"); err == nil || !strings.Contains(err.Error(), "maintenance.archive_dir") {
			t.Errorf("loading with the setting unset = %v, want the setting named", err)
		}
	})

	t.Run("a missing config is an error, not an empty set", func(t *testing.T) {
		t.Parallel()
		if _, err := archivedHashesLoader(t.TempDir(), []string{"gt"})("gt"); err == nil {
			t.Error("loading with no daemon config reported an empty set")
		}
	})

	t.Run("a config that cannot be parsed is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot, "{not json")
		if _, err := archivedHashesLoader(townRoot, []string{"gt"})("gt"); err == nil {
			t.Error("loading with a corrupt daemon config reported an empty set")
		}
	})

	t.Run("an archive directory that cannot be read is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(t.TempDir(), "absent")
		writeDaemonConfig(t, townRoot, fmt.Sprintf(
			`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":%q}}}`,
			dir))
		if _, err := archivedHashesLoader(townRoot, []string{"gt"})("gt"); err == nil {
			t.Error("an unreadable archive directory loaded as an empty set")
		}
	})

	t.Run("a database outside the call's list is refused", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := t.TempDir()
		writeDaemonConfig(t, townRoot, fmt.Sprintf(
			`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":%q}}}`,
			dir))
		if _, err := archivedHashesLoader(townRoot, []string{"gt"})("hq"); err == nil || !strings.Contains(err.Error(), "not listed") {
			t.Errorf("a database outside the call's list = %v, want it refused", err)
		}
	})

	t.Run("a relative setting is refused", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot,
			`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":"archive"}}}`)
		if _, err := archivedHashesLoader(townRoot, []string{"gt"})("gt"); err == nil {
			t.Error("a relative archive_dir was resolved")
		}
	})
}

// TestPreFlattenCheckCountsLogHashesNotInTheArchive: the check compares every
// hash in dolt_log with the archived-hash set the archive keeps for the
// database, so a commit dated before the watermark's cursor is still counted
// when it is not archived. Zero only when every log hash is in the set; a set
// that cannot be read is an error, never a zero (gt-7nxj4).
func TestPreFlattenCheckCountsLogHashesNotInTheArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config := &doltserver.Config{}
	when := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	wm := daemon.ArchiveWatermark{Date: when, Hashes: []string{"archived-new"}}

	t.Run("a commit dated before the cursor but not archived is counted", func(t *testing.T) {
		t.Parallel()
		server := &archiveCountServer{hashes: []string{"archived-new", "backdated-not-archived"}}
		reader := doltArchiveReader{
			config:         config,
			archivedHashes: archivedSet("archived-new"),
			openDB:         archiveCountOpener(t, server),
		}
		got, err := reader.commitsBehind(ctx, "gt", wm)
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if got != 1 {
			t.Errorf("log hashes not in the archive = %d, want 1 (the backdated commit)", got)
		}
		if !strings.Contains(server.query, "commit_hash") || strings.Contains(server.query, "WHERE") {
			t.Errorf("check did not compare every log hash: %s", server.query)
		}
	})

	t.Run("every log hash in the set is safe", func(t *testing.T) {
		t.Parallel()
		server := &archiveCountServer{hashes: []string{"archived-new", "archived-old"}}
		reader := doltArchiveReader{
			config:         config,
			archivedHashes: archivedSet("archived-new", "archived-old"),
			openDB:         archiveCountOpener(t, server),
		}
		got, err := reader.commitsBehind(ctx, "gt", wm)
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if got != 0 {
			t.Errorf("log hashes not in the archive = %d, want 0", got)
		}
	})

	t.Run("an unreadable set is not safe", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			archivedHashes: func(string) (map[string]struct{}, error) {
				return nil, errors.New("archive directory unreadable")
			},
			openDB: func(string) (*sql.DB, error) {
				t.Error("the log was queried after the archived set could not be read")
				return nil, errors.New("not reached")
			},
		}
		if _, err := reader.commitsBehind(ctx, "gt", wm); err == nil || !strings.Contains(err.Error(), "archive directory unreadable") {
			t.Errorf("check with an unreadable set = %v, want the read error", err)
		}
	})
}

// TestArchiveEventsBehindQueryReadsTheLookbackWindow runs the production query text
// (eventsBehindQuery) against an in-process MySQL engine, not a fake that
// repeats its WHERE clause: the count with no watermark covers the whole
// table, and the windowed read returns only rows at or after the lookback
// start, ordered by created_at then id (gt-98eug rework).
func TestArchiveEventsBehindQueryReadsTheLookbackWindow(t *testing.T) {
	t.Parallel()
	ctx, engine := newStatusArchiveEngine(t)
	for _, row := range [][2]string{
		{"old", "2026-09-30 23:59:59"},
		{"at-start", "2026-10-01 00:00:00"},
		{"inside-b", "2026-10-01 12:00:00"},
		{"inside-a", "2026-10-01 12:00:00"},
	} {
		statusEngineExec(t, ctx, engine, fmt.Sprintf(
			"INSERT INTO events (id, created_at) VALUES ('%s', '%s')", row[0], row[1]))
	}

	// No watermark: the count is an existence check, not a COUNT(*), and it
	// answers whether the table holds any row.
	probeQuery, err := eventsBehindQuery("gt", daemon.EventsArchiveWatermark{})
	if err != nil {
		t.Fatalf("building the no-watermark query: %v", err)
	}
	if strings.Contains(strings.ToUpper(probeQuery), "COUNT") {
		t.Errorf("no-watermark query counts the table: %s", probeQuery)
	}
	if got := statusEngineRowCount(t, ctx, engine, probeQuery); got != 1 {
		t.Errorf("no-watermark probe returned %d row(s), want one while the table holds rows", got)
	}

	// The archive's window: rows at or after the newest row minus 24h, which
	// is 2026-10-01 00:00:00 for a watermark of 2026-10-02 00:00:00.
	wm := daemon.EventsArchiveWatermark{
		CreatedAt: "2026-10-02 00:00:00",
		Archived:  []daemon.ArchivedEventsRow{{CreatedAt: "2026-10-02 00:00:00", ID: "archived"}},
	}
	windowQuery, err := eventsBehindQuery("gt", wm)
	if err != nil {
		t.Fatalf("building the window query: %v", err)
	}
	got := statusEngineIDs(t, ctx, engine, windowQuery)
	want := []string{"at-start", "inside-a", "inside-b"}
	if !slices.Equal(got, want) {
		t.Errorf("window rows = %v, want %v (the inclusive 24h lookback, created_at then id)", got, want)
	}
}

// TestStatusReadsArchivedHashesOncePerDatabaseAndBoundsTheEventsCount: one
// status call reads each database's archive files once, and the no-watermark
// events count is an existence check rather than a full COUNT(*) (gt-3w3hk).
func TestStatusReadsArchivedHashesOncePerDatabaseAndBoundsTheEventsCount(t *testing.T) {
	t.Parallel()

	t.Run("one status call reads each database's archive files once", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := t.TempDir()
		writeDaemonConfig(t, townRoot, fmt.Sprintf(
			`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":%q}}}`,
			dir))
		gtPath := filepath.Join(dir, "dolt-log-gt-20261102.csv.gz")
		hqPath := filepath.Join(dir, "dolt-log-hq-20261102.csv.gz")
		writeCommitArchive(t, gtPath, []string{"gt-1"})
		writeCommitArchive(t, hqPath, []string{"hq-1"})

		load := archivedHashesLoader(townRoot, []string{"gt", "hq"})
		if got, err := load("gt"); err != nil || len(got) != 1 {
			t.Fatalf("gt's archived hashes = %v, %v; want its own one hash", got, err)
		}
		// Rewrite both files: a status call that read the archive once must not
		// see the rewrite, for either database.
		writeCommitArchive(t, gtPath, []string{"gt-2"})
		writeCommitArchive(t, hqPath, []string{"hq-2"})
		if got, err := load("gt"); err != nil || len(got) != 1 {
			t.Fatalf("gt's second read = %v, %v; want its first reading", got, err)
		} else if _, ok := got["gt-1"]; !ok {
			t.Errorf("gt's archived hashes = %v, want gt-1 from the call's one reading", got)
		}
		got, err := load("hq")
		if err != nil || len(got) != 1 {
			t.Fatalf("hq's archived hashes = %v, %v; want its own one hash", got, err)
		}
		if _, ok := got["hq-1"]; !ok {
			t.Errorf("hq's archived hashes = %v, want hq-1, not the rewritten file", got)
		}
		// A new status call is a new reading.
		fresh := archivedHashesLoader(townRoot, []string{"gt"})
		if got, err := fresh("gt"); err != nil || len(got) != 1 {
			t.Fatalf("next status call's read = %v, %v", got, err)
		} else if _, ok := got["gt-2"]; !ok {
			t.Errorf("next status call's archived hashes = %v, want the rewritten gt-2", got)
		}
	})

	t.Run("the no-watermark events count issues no full COUNT", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		server := &archiveCountServer{}
		reader := doltArchiveReader{config: &doltserver.Config{}, openDB: archiveCountOpener(t, server)}
		wm := daemon.EventsArchiveWatermark{}
		got, err := reader.eventsBehind(ctx, "gt", wm)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got != eventsBehindUncounted {
			t.Errorf("events behind = %d, want %d (rows exist, not counted)", got, eventsBehindUncounted)
		}
		if strings.Contains(strings.ToUpper(server.query), "COUNT") {
			t.Errorf("no-watermark count ran a full count: %s", server.query)
		}
		note := eventsArchiveNote(ctx, reader, "gt", wm)
		if !strings.Contains(note, "at least one event behind") || !strings.Contains(note, "existence check") {
			t.Errorf("note = %q, want the bounded count labelled as an existence check", note)
		}
	})
}

// newStatusArchiveEngine builds an in-memory go-mysql-server engine with a gt
// database and the events table the status count reads.
func newStatusArchiveEngine(t *testing.T) (*gmssql.Context, *sqle.Engine) {
	t.Helper()
	db := memory.NewDatabase("gt")
	pro := memory.NewDBProvider(db)
	session := memory.NewSession(gmssql.NewBaseSession(), pro)
	ctx := gmssql.NewContext(context.Background(), gmssql.WithSession(session))
	ctx.SetCurrentDatabase("gt")
	db.AddTable("events", memory.NewTable(ctx, db, "events", gmssql.NewPrimaryKeySchema(gmssql.Schema{
		{Name: "id", Type: types.Text, Nullable: false, Source: "events"},
		{Name: "created_at", Type: types.Datetime, Nullable: false, Source: "events"},
	}), db.GetForeignKeyCollection()))
	engine := sqle.NewDefault(pro)
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("closing the test engine: %v", err)
		}
	})
	return ctx, engine
}

// statusEngineExec runs one statement against the in-process engine.
func statusEngineExec(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if _, err := gmssql.RowIterToRows(ctx, iter); err != nil {
		t.Fatalf("draining %q: %v", query, err)
	}
}

// statusEngineIDs runs query and returns its first column as strings.
func statusEngineIDs(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) []string {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	rows, err := gmssql.RowIterToRows(ctx, iter)
	if err != nil {
		t.Fatalf("reading rows for %q: %v", query, err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id, ok := row[0].(string)
		if !ok {
			t.Fatalf("row id is %T, want string: %v", row[0], row[0])
		}
		ids = append(ids, id)
	}
	return ids
}

// statusEngineRowCount runs query against the in-process engine and returns how
// many rows it answered with.
func statusEngineRowCount(t *testing.T, ctx *gmssql.Context, engine *sqle.Engine, query string) int {
	t.Helper()
	_, iter, _, err := engine.Query(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	rows, err := gmssql.RowIterToRows(ctx, iter)
	if err != nil {
		t.Fatalf("reading rows for %q: %v", query, err)
	}
	return len(rows)
}
