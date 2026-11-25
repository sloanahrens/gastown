package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// eventsPageCall is one call of the paged read's readPage seam.
type eventsPageCall struct {
	afterCreatedAt string
	afterID        string
	limit          int
}

// eventsPagedRows is a fake events table read in pages: lines are in
// created_at, id order, each call returns the rows after its keyset, and a
// stuck fake returns the first page whatever keyset it is given. err fails
// every call; errAt fails the call at that 1-based position and serves the
// calls before it.
type eventsPagedRows struct {
	lines []string
	calls []eventsPageCall
	err   error
	errAt int
	stuck bool
}

func (f *eventsPagedRows) readPage(afterCreatedAt, afterID string, limit int) ([]json.RawMessage, error) {
	f.calls = append(f.calls, eventsPageCall{afterCreatedAt, afterID, limit})
	if f.err != nil && (f.errAt == 0 || f.errAt == len(f.calls)) {
		return nil, f.err
	}
	start := 0
	if afterID != "" && !f.stuck {
		start = -1
		for i, line := range f.lines {
			var row struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				return nil, err
			}
			if row.ID == afterID {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, errors.New("no row " + afterID)
		}
	}
	end := min(start+limit, len(f.lines))
	page := make([]json.RawMessage, 0, end-start)
	for _, line := range f.lines[start:end] {
		page = append(page, json.RawMessage(line))
	}
	return page, nil
}

// eventsExportLines is every line the events export in dir holds, in file
// order.
func eventsExportLines(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := jsonlShardPaths(dir, "events")
	if err != nil {
		t.Fatalf("listing the events export: %v", err)
	}
	var lines []string
	for _, path := range paths {
		got, err := readEventLines(path)
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Base(path), err)
		}
		lines = append(lines, got...)
	}
	return lines
}

// TestExportEventsPages: the paged read writes every page as it arrives, in
// order, stops on a short page, and refuses the inputs it cannot continue from
// (gt-y77tfo).
func TestExportEventsPages(t *testing.T) {
	t.Parallel()
	rows := []string{
		`{"id":"e1","created_at":"2026-10-31 10:00:00","type":"created"}`,
		`{"id":"e2","created_at":"2026-10-31 10:00:01","type":"created"}`,
		`{"id":"e3","created_at":"2026-10-31 10:00:01","type":"updated"}`,
		`{"id":"e4","created_at":"2026-10-31 10:00:02","type":"created"}`,
		`{"id":"e5","created_at":"2026-10-31 10:00:03","type":"deleted"}`,
	}
	unwanted := strings.Join(rows, "\n")

	t.Run("five rows in pages of two", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fake := &eventsPagedRows{lines: rows}
		written, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", dir, 2, fake.readPage)
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if written != 5 {
			t.Errorf("written = %d, want 5", written)
		}
		want := []eventsPageCall{{"", "", 2}, {"2026-10-31 10:00:01", "e2", 2}, {"2026-10-31 10:00:02", "e4", 2}}
		if !slices.Equal(fake.calls, want) {
			t.Errorf("page reads = %+v, want %+v", fake.calls, want)
		}
		if got := strings.Join(eventsExportLines(t, dir), "\n"); got != unwanted {
			t.Errorf("the export holds\n%s\nwant\n%s", got, unwanted)
		}
	})

	t.Run("an older export is replaced", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeEventsShard(t, dir, "events.jsonl", `{"id":"old1","created_at":"2026-10-30 10:00:00"}`)
		writeEventsShard(t, dir, "events.2.jsonl", `{"id":"old2","created_at":"2026-10-30 10:00:01"}`)
		fake := &eventsPagedRows{lines: rows}
		if _, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", dir, 2, fake.readPage); err != nil {
			t.Fatalf("export: %v", err)
		}
		if got := strings.Join(eventsExportLines(t, dir), "\n"); got != unwanted {
			t.Errorf("the export holds\n%s\nwant\n%s", got, unwanted)
		}
	})

	t.Run("a page of no rows is refused", func(t *testing.T) {
		t.Parallel()
		fake := &eventsPagedRows{lines: rows}
		if _, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", t.TempDir(), 0, fake.readPage); err == nil {
			t.Error("a page of 0 rows was read")
		}
		if len(fake.calls) != 0 {
			t.Errorf("the read was asked for %d page(s), want none", len(fake.calls))
		}
	})

	t.Run("an export that cannot be listed", func(t *testing.T) {
		t.Parallel()
		fake := &eventsPagedRows{lines: rows}
		if _, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", filepath.Join(t.TempDir(), "missing"), 2, fake.readPage); err == nil {
			t.Error("an export that cannot be listed was written")
		}
	})

	t.Run("a page cannot be read", func(t *testing.T) {
		t.Parallel()
		fake := &eventsPagedRows{err: errors.New("the read failed")}
		written, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", t.TempDir(), 2, fake.readPage)
		if err == nil {
			t.Error("a failed page read reported success")
		}
		if written != 0 {
			t.Errorf("written = %d, want 0", written)
		}
	})

	t.Run("the last row cannot move the keyset", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fake := &eventsPagedRows{lines: []string{`{"id":"e1"}`}}
		written, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", dir, 2, fake.readPage)
		if err == nil {
			t.Error("a page whose last row has no created_at was written")
		}
		if written != 0 {
			t.Errorf("written = %d, want 0", written)
		}
		if got := eventsExportLines(t, dir); len(got) != 0 {
			t.Errorf("the export holds %v, want no rows", got)
		}
	})

	t.Run("a page that does not move the keyset stops", func(t *testing.T) {
		t.Parallel()
		fake := &eventsPagedRows{lines: rows, stuck: true}
		written, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", t.TempDir(), 2, fake.readPage)
		if err == nil {
			t.Error("a page that repeats its keyset was read again")
		}
		if written != 2 {
			t.Errorf("written = %d, want the 2 rows of the first page", written)
		}
	})

	t.Run("the filesystem work fails", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			ops  eventsExportOps
		}{
			{"an old shard cannot be cleared", func() eventsExportOps {
				ops := osEventsExportOps()
				ops.remove = func(string) error { return errors.New("the disk is read-only") }
				return ops
			}()},
			{"the first shard cannot be created", eventsOpsWithWriteFile(errors.New("the disk is full"))},
			{"a page cannot be appended", func() eventsExportOps {
				ops := osEventsExportOps()
				writes := 0
				ops.writeFile = func(name string, data []byte, perm os.FileMode) error {
					if writes++; writes > 1 {
						return errors.New("the disk is full")
					}
					return os.WriteFile(name, data, perm)
				}
				return ops
			}()},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				writeEventsShard(t, dir, "events.jsonl", `{"id":"old","created_at":"2026-10-30 10:00:00"}`)
				fake := &eventsPagedRows{lines: rows}
				if _, err := (&Daemon{}).exportEventsPagesWith(context.Background(), tc.ops, "gt", dir, 2, fake.readPage); err == nil {
					t.Error("the export reported success")
				}
			})
		}
	})
}

// TestExportEventsPagesReadError: a page read that fails after a page was
// written returns the error with the rows already written, reads no further
// page, and writes no state file (gt-t4eldj).
func TestExportEventsPagesReadError(t *testing.T) {
	t.Parallel()
	rows := []string{
		`{"id":"e1","created_at":"2026-10-31 10:00:00","type":"created"}`,
		`{"id":"e2","created_at":"2026-10-31 10:00:01","type":"created"}`,
		`{"id":"e3","created_at":"2026-10-31 10:00:01","type":"updated"}`,
		`{"id":"e4","created_at":"2026-10-31 10:00:02","type":"created"}`,
		`{"id":"e5","created_at":"2026-10-31 10:00:03","type":"deleted"}`,
	}
	dir := t.TempDir()
	readErr := errors.New("the second page read failed")
	fake := &eventsPagedRows{lines: rows, err: readErr, errAt: 2}

	written, err := (&Daemon{}).exportEventsPagesWith(context.Background(), osEventsExportOps(), "gt", dir, 2, fake.readPage)
	if err == nil {
		t.Fatal("a failed second page reported success")
	}
	if !errors.Is(err, readErr) {
		t.Errorf("error %q does not wrap the read error", err)
	}
	if !strings.Contains(err.Error(), "events") {
		t.Errorf("error %q does not name events", err)
	}
	if written != 2 {
		t.Errorf("written = %d, want the 2 rows of the first page", written)
	}
	if len(fake.calls) != 2 {
		t.Errorf("readPage was called %d time(s), want 2", len(fake.calls))
	}
	if _, statErr := os.Stat(eventsExportStatePath(dir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the state file was written after the failed read (stat error %v)", statErr)
	}
}

// TestExportEventsPagesStops: the paged read stops before a page when its ctx
// is done, returning the rows already written with no boundary recorded, so
// the next cycle reads the table again (gt-lnz787).
func TestExportEventsPagesStops(t *testing.T) {
	t.Parallel()
	rows := []string{
		`{"id":"e1","created_at":"2026-10-31 10:00:00","type":"created"}`,
		`{"id":"e2","created_at":"2026-10-31 10:00:01","type":"created"}`,
		`{"id":"e3","created_at":"2026-10-31 10:00:01","type":"updated"}`,
		`{"id":"e4","created_at":"2026-10-31 10:00:02","type":"created"}`,
		`{"id":"e5","created_at":"2026-10-31 10:00:03","type":"deleted"}`,
	}

	t.Run("a cancel after the first page", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		fake := &eventsPagedRows{lines: rows}
		readPage := func(afterCreatedAt, afterID string, limit int) ([]json.RawMessage, error) {
			page, err := fake.readPage(afterCreatedAt, afterID, limit)
			cancel()
			return page, err
		}
		written, err := (&Daemon{}).exportEventsPagesWith(ctx, osEventsExportOps(), "gt", dir, 2, readPage)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("export error = %v, want context.Canceled", err)
		}
		for _, want := range []string{"events", "2 rows"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
		if written != 2 {
			t.Errorf("written = %d, want 2", written)
		}
		if len(fake.calls) != 1 {
			t.Errorf("page reads = %d, want 1", len(fake.calls))
		}
		if _, err := os.Stat(eventsExportStatePath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the state file was written: %v", err)
		}
	})

	t.Run("a ctx that is already done", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fake := &eventsPagedRows{lines: rows}
		written, err := (&Daemon{}).exportEventsPagesWith(ctx, osEventsExportOps(), "gt", dir, 2, fake.readPage)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("export error = %v, want context.Canceled", err)
		}
		if written != 0 {
			t.Errorf("written = %d, want 0", written)
		}
		if len(fake.calls) != 0 {
			t.Errorf("page reads = %d, want none", len(fake.calls))
		}
		if _, err := os.Stat(eventsExportStatePath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the state file was written: %v", err)
		}
	})
}

// eventsPageExport is the page reader's table exporter: it records each call's
// query and directory and writes the rows as one events.jsonl shard there.
type eventsPageExport struct {
	rows      []string
	skipWrite bool // report the rows but write no shard
	err       error
	calls     int
	dirs      []string
	queries   []string
}

func (f *eventsPageExport) export(_ string, q beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
	f.calls++
	f.dirs = append(f.dirs, dir)
	inline, err := q.Inline()
	if err != nil {
		return 0, err
	}
	f.queries = append(f.queries, inline)
	switch {
	case f.err != nil:
		return 0, f.err
	case len(f.rows) == 0 || f.skipWrite:
		return len(f.rows), nil
	}
	body := strings.Join(f.rows, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0644); err != nil {
		return 0, err
	}
	return len(f.rows), nil
}

// eventsPageReaderDaemon is a daemon on a temp town, and its gt events export dir.
func eventsPageReaderDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	town := t.TempDir()
	repo, _ := JsonlBackupRepo(town, nil)
	return &Daemon{config: &Config{TownRoot: town}}, filepath.Join(repo, "gt")
}

// TestEventsPageReader: the production page reader exports one events page
// into the export's staging directory, returns its rows, and removes it (gt-mjw5a7).
func TestEventsPageReader(t *testing.T) {
	t.Parallel()
	rows := []string{
		eventsLine("e1", "2026-10-31 10:00:00"),
		eventsLine("e2", "2026-10-31 10:00:01"),
		eventsLine("e3", "2026-10-31 10:00:01"),
	}
	const (
		bound = "2026-10-30 10:00:00"
		id    = "11111111-1111-1111-1111-111111111111"
	)

	t.Run("returns the exported rows in order", func(t *testing.T) {
		t.Parallel()
		d, _ := eventsPageReaderDaemon(t)
		exp := &eventsPageExport{rows: rows}
		page, err := d.eventsPageReader("gt", "/data", time.Minute, exp.export)(bound, id, 3)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var got []string
		for _, row := range page {
			got = append(got, string(row))
		}
		if !slices.Equal(got, rows) {
			t.Errorf("page = %q, want %q", got, rows)
		}
	})

	t.Run("reads the page built from the bound and limit", func(t *testing.T) {
		t.Parallel()
		d, _ := eventsPageReaderDaemon(t)
		exp := &eventsPageExport{rows: rows}
		if _, err := d.eventsPageReader("gt", "/data", time.Minute, exp.export)(bound, id, 3); err != nil {
			t.Fatalf("read: %v", err)
		}
		want, err := beadsql.BackupEventsPage("gt", bound, id, 3).Inline()
		if err != nil {
			t.Fatal(err)
		}
		if len(exp.queries) != 1 || exp.queries[0] != want {
			t.Errorf("queries = %q, want one %q", exp.queries, want)
		}
	})

	t.Run("stages the read in the events export's incoming directory and removes it", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		for _, tc := range []struct {
			name   string
			patrol *DaemonPatrolConfig
		}{
			{"the default repo", nil},
			{"a configured repo", &DaemonPatrolConfig{Patrols: &PatrolsConfig{JsonlGitBackup: &JsonlGitBackupConfig{GitRepo: filepath.Join(t.TempDir(), "backup")}}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				d := &Daemon{config: &Config{TownRoot: town}, patrolConfig: tc.patrol}
				exp := &eventsPageExport{rows: rows}
				if _, err := d.eventsPageReader("gt", "/data", time.Minute, exp.export)(bound, id, 3); err != nil {
					t.Fatalf("read: %v", err)
				}
				repo, _ := JsonlBackupRepo(town, tc.patrol)
				if want, got := filepath.Join(repo, "gt", eventsIncomingDir), filepath.Dir(exp.dirs[0]); got != want {
					t.Errorf("stage parent = %s, want %s", got, want)
				}
				if _, err := os.Stat(exp.dirs[0]); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the stage %s survived the read: %v", exp.dirs[0], err)
				}
			})
		}
	})

	t.Run("an export of no rows is an empty page", func(t *testing.T) {
		t.Parallel()
		d, _ := eventsPageReaderDaemon(t)
		exp := &eventsPageExport{}
		page, err := d.eventsPageReader("gt", "/data", time.Minute, exp.export)(bound, id, 3)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(page) != 0 {
			t.Errorf("page = %q, want none", page)
		}
	})

	t.Run("a failed export is an error", func(t *testing.T) {
		t.Parallel()
		d, _ := eventsPageReaderDaemon(t)
		exp := &eventsPageExport{err: errors.New("the dolt read timed out")}
		if _, err := d.eventsPageReader("gt", "/data", time.Minute, exp.export)(bound, id, 3); err == nil || !strings.Contains(err.Error(), "the dolt read timed out") {
			t.Errorf("read error = %v, want the export's error", err)
		}
	})

	t.Run("an export reporting rows it did not write is refused", func(t *testing.T) {
		t.Parallel()
		d, _ := eventsPageReaderDaemon(t)
		exp := &eventsPageExport{rows: rows, skipWrite: true}
		if _, err := d.eventsPageReader("gt", "/data", time.Minute, exp.export)(bound, id, 3); err == nil {
			t.Error("an export that reported rows but wrote none was read as a page")
		}
	})

	t.Run("a page it cannot stage is refused", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name  string
			db    string
			block bool // a file where the incoming directory belongs
		}{
			{"a database name outside the town", "../gt", false},
			{"an incoming directory in the way", "gt", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				d, exportDir := eventsPageReaderDaemon(t)
				if tc.block {
					if err := os.MkdirAll(exportDir, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(exportDir, eventsIncomingDir), []byte("in the way"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				exp := &eventsPageExport{rows: rows}
				if _, err := d.eventsPageReader(tc.db, "/data", time.Minute, exp.export)(bound, id, 3); err == nil {
					t.Error("a page was read")
				}
				if exp.calls != 0 {
					t.Errorf("the export was asked for %d page(s), want none", exp.calls)
				}
			})
		}
	})
}
