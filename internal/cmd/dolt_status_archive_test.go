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
	// hashes, when set, is served as one text column: the commit hashes a
	// dolt_log read returns. Otherwise the query serves the count 3.
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
	return &archiveCountRows{null: c.server.null, hashes: c.server.hashes, nextErr: c.server.nextErr}, nil
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
	hashes  []string
	nextErr error
	next    int
}

func (*archiveCountRows) Columns() []string { return []string{"value"} }

func (*archiveCountRows) Close() error { return nil }

func (r *archiveCountRows) Next(dest []driver.Value) error {
	if r.nextErr != nil {
		return r.nextErr
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

	t.Run("an events count subtracts the ids in the lookback window", func(t *testing.T) {
		t.Parallel()
		server := &archiveCountServer{hashes: []string{"archived", "late"}}
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
		// The window starts 24h before the watermark's newest row, the same
		// re-read the archive makes.
		if !strings.Contains(server.query, "created_at >= '2026-11-01 09:15:00'") {
			t.Errorf("events count did not start at the lookback window: %s", server.query)
		}
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

	t.Run("a windowed events count reports an id it cannot scan", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{null: true}),
		}
		if _, err := reader.eventsBehind(ctx, "gt", windowed); err == nil {
			t.Error("a NULL id was read as an unarchived row")
		}
	})

	t.Run("a windowed events count reports a row read that fails", func(t *testing.T) {
		t.Parallel()
		reader := doltArchiveReader{
			config: config,
			openDB: archiveCountOpener(t, &archiveCountServer{nextErr: errors.New("row read failed")}),
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

		got, err := archivedHashesLoader(townRoot)("gt")
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
		if _, err := archivedHashesLoader(townRoot)("gt"); err == nil || !strings.Contains(err.Error(), "maintenance.archive_dir") {
			t.Errorf("loading with the setting unset = %v, want the setting named", err)
		}
	})

	t.Run("a missing config is an error, not an empty set", func(t *testing.T) {
		t.Parallel()
		if _, err := archivedHashesLoader(t.TempDir())("gt"); err == nil {
			t.Error("loading with no daemon config reported an empty set")
		}
	})

	t.Run("a config that cannot be parsed is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot, "{not json")
		if _, err := archivedHashesLoader(townRoot)("gt"); err == nil {
			t.Error("loading with a corrupt daemon config reported an empty set")
		}
	})

	t.Run("a relative setting is refused", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot,
			`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":"archive"}}}`)
		if _, err := archivedHashesLoader(townRoot)("gt"); err == nil {
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
