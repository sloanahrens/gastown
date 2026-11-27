package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// eventsTestRow is one row of a fake events table, in the form Dolt exports it
// for the two columns the append orders by.
type eventsTestRow struct {
	id        string
	createdAt string
}

// eventsTableFake stands in for a database's events table: rows is what the
// table holds, and each export writes in created_at, id order the rows its
// query admits, as Dolt would: its created_at bound or page keyset, up to its
// page limit. Every row goes through the bound, so a row stamped before it is
// not returned however it arrived in the table.
type eventsTableFake struct {
	rows    []eventsTestRow
	queries []string
}

// exporter is the fake's jsonlTableExportFn: an events read lands in dir as
// the rows the query admits (its created_at bound, or its page keyset, and its
// page limit), and every other table as empty lines.
func (f *eventsTableFake) exporter(t *testing.T, others map[string]int) jsonlTableExporter {
	t.Helper()
	return func(table string, q beadsql.Query, dir, _ string, _ time.Duration) (int, error) {
		if table != "events" {
			writeNLines(t, filepath.Join(dir, table+".jsonl"), others[table])
			return others[table], nil
		}
		text, err := q.Inline()
		if err != nil {
			t.Fatalf("events query: %v", err)
		}
		f.queries = append(f.queries, text)
		bound, bounded := eventsQueryBound(text)
		var read []eventsTestRow
		for _, row := range f.rows {
			if bounded && row.createdAt < bound {
				continue
			}
			if !eventsRowPastKeyset(text, row) {
				continue
			}
			read = append(read, row)
		}
		sort.SliceStable(read, func(i, j int) bool {
			if read[i].createdAt != read[j].createdAt {
				return read[i].createdAt < read[j].createdAt
			}
			return read[i].id < read[j].id
		})
		if limit, ok := eventsQueryLimit(text); ok && len(read) > limit {
			read = read[:limit]
		}
		var lines []string
		for _, row := range read {
			line, err := json.Marshal(map[string]string{"id": row.id, "created_at": row.createdAt})
			if err != nil {
				t.Fatalf("marshalling row %s: %v", row.id, err)
			}
			lines = append(lines, string(line))
		}
		body := ""
		if len(lines) > 0 {
			body = strings.Join(lines, "\n") + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0644); err != nil {
			t.Fatalf("writing the fake events read: %v", err)
		}
		return len(lines), nil
	}
}

var eventsQueryBoundPattern = regexp.MustCompile(`created_at >= '([^']+)'`)

// eventsQueryBound is the created_at bound an events query carries, if any.
func eventsQueryBound(text string) (string, bool) {
	m := eventsQueryBoundPattern.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

var (
	eventsQueryKeysetPattern = regexp.MustCompile(`created_at > '([^']+)' OR \(created_at = '[^']+' AND id > '([^']+)'\)`)
	eventsQueryLimitPattern  = regexp.MustCompile(`LIMIT (\d+)$`)
)

// eventsRowPastKeyset reports whether row comes after the keyset a page query
// names. A query that names none admits every row.
func eventsRowPastKeyset(text string, row eventsTestRow) bool {
	m := eventsQueryKeysetPattern.FindStringSubmatch(text)
	if m == nil {
		return true
	}
	return row.createdAt > m[1] || (row.createdAt == m[1] && row.id > m[2])
}

// eventsQueryLimit is the row count a page query asks for, if it names one.
func eventsQueryLimit(text string) (int, bool) {
	m := eventsQueryLimitPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// eventsExportIDs is the ids the events export in dir holds, in file order.
func eventsExportIDs(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := jsonlShardPaths(dir, "events")
	if err != nil {
		t.Fatalf("listing the events export: %v", err)
	}
	var ids []string
	for _, path := range paths {
		lines, err := readEventLines(path)
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Base(path), err)
		}
		for _, line := range lines {
			var row struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("%s holds a line that is not a row: %v", filepath.Base(path), err)
			}
			ids = append(ids, row.ID)
		}
	}
	return ids
}

// TestExportEventsAppendsOnlyTheRowsSinceItsBoundary: the second cycle reads
// from the boundary the first recorded, adds the row stamped in that same
// second inside the run it belongs to, and leaves every row in the file once,
// in created_at, id order (gt-l7eeb).
func TestExportEventsAppendsOnlyTheRowsSinceItsBoundary(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
	fake := &eventsTableFake{rows: []eventsTestRow{
		{"e1", "2026-10-31 10:00:00"},
		{"e2", "2026-10-31 10:00:00"},
		{"e3a", "2026-10-31 10:01:00"},
		{"e3b", "2026-10-31 10:01:00"},
	}}
	d.jsonlTableExportFn = fake.exporter(t, nil)
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	dataDir := t.TempDir()

	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if bound, ok := eventsQueryBound(fake.queries[0]); ok {
		t.Fatalf("the first export read from the bound %q; it has no boundary yet", bound)
	}
	if got, want := eventsExportIDs(t, dbDir), []string{"e1", "e2", "e3a", "e3b"}; !slices.Equal(got, want) {
		t.Fatalf("first export holds %v, want %v", got, want)
	}

	// A new row lands in the boundary second, and one later.
	fake.rows = append(fake.rows,
		eventsTestRow{"e3c", "2026-10-31 10:01:00"},
		eventsTestRow{"e4", "2026-10-31 10:02:00"},
	)
	n, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if len(fake.queries) != 2 {
		t.Fatalf("the events table was read %d times, want 2", len(fake.queries))
	}
	if bound, ok := eventsQueryBound(fake.queries[1]); !ok || bound != "2026-10-31 10:01:00" {
		t.Errorf("the second export read from bound %q (bounded %v), want 2026-10-31 10:01:00:\n%s", bound, ok, fake.queries[1])
	}
	if got, want := eventsExportIDs(t, dbDir), []string{"e1", "e2", "e3a", "e3b", "e3c", "e4"}; !slices.Equal(got, want) {
		t.Errorf("the export holds %v, want every row once in created_at, id order: %v", got, want)
	}
	if n != 6 {
		t.Errorf("exportDatabaseToJsonl = %d, want the 6 rows the file holds", n)
	}
	state, err := readEventsExportState(dbDir)
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	if state.Rows != 6 || state.LastCreatedAt != "2026-10-31 10:02:00" {
		t.Errorf("state = %+v, want 6 rows ending at 2026-10-31 10:02:00", state)
	}
}

// TestExportEventsFallsBackToTheWholeTable: a state it cannot read, and a file
// whose row count disagrees with it, each send the next export to the whole
// table — a boundary it does not hold is never appended from (gt-l7eeb).
func TestExportEventsFallsBackToTheWholeTable(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		damage func(t *testing.T, dbDir string)
	}{
		{"missing state", func(t *testing.T, dbDir string) {
			if err := os.Remove(eventsExportStatePath(dbDir)); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable state", func(t *testing.T, dbDir string) {
			if err := os.WriteFile(eventsExportStatePath(dbDir), []byte("not json\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"row count disagrees with the state", func(t *testing.T, dbDir string) {
			f, err := os.OpenFile(filepath.Join(dbDir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(`{"id":"stray","created_at":"2026-10-31 10:00:00"}` + "\n"); err != nil {
				t.Fatal(err)
			}
		}},
		{"export is missing", func(t *testing.T, dbDir string) {
			if err := os.Remove(filepath.Join(dbDir, "events.jsonl")); err != nil {
				t.Fatal(err)
			}
		}},
		{"state records no boundary", func(t *testing.T, dbDir string) {
			state := `{"version":1,"last_created_at":"","rows":2,"full_exported_at":"2026-10-31T10:05:00Z"}`
			if err := os.WriteFile(eventsExportStatePath(dbDir), []byte(state+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := backupTestDaemon(t)
			d.clock = clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
			fake := &eventsTableFake{rows: []eventsTestRow{
				{"e1", "2026-10-31 10:00:00"},
				{"e2", "2026-10-31 10:01:00"},
			}}
			d.jsonlTableExportFn = fake.exporter(t, nil)
			gitRepo := t.TempDir()
			dbDir := filepath.Join(gitRepo, "hq")
			dataDir := t.TempDir()

			if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
				t.Fatalf("first export: %v", err)
			}
			tt.damage(t, dbDir)
			if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
				t.Fatalf("second export: %v", err)
			}

			if len(fake.queries) != 2 {
				t.Fatalf("the events table was read %d times, want 2", len(fake.queries))
			}
			if bound, ok := eventsQueryBound(fake.queries[1]); ok {
				t.Errorf("the second export read from the bound %q; it cannot trust a state it could not check", bound)
			}
			if got, want := eventsExportIDs(t, dbDir), []string{"e1", "e2"}; !slices.Equal(got, want) {
				t.Errorf("the export holds %v, want the whole table %v", got, want)
			}
		})
	}
}

// TestExportEventsRereadsTheWholeTableEveryDay: an event of a deleted issue is
// removed from the table by the foreign-key cascade, and only a whole-table
// read takes it out of the append-only file. The read runs once the boundary
// is a day old (gt-l7eeb).
func TestExportEventsRereadsTheWholeTableEveryDay(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
	d.clock = clk
	fake := &eventsTableFake{rows: []eventsTestRow{
		{"e1", "2026-10-31 10:00:00"},
		{"e2", "2026-10-31 10:01:00"},
	}}
	d.jsonlTableExportFn = fake.exporter(t, nil)
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	dataDir := t.TempDir()

	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("first export: %v", err)
	}

	// e1's issue is deleted, and the day turns over.
	fake.rows = fake.rows[1:]
	clk.Advance(eventsFullExportEvery)

	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if len(fake.queries) != 2 {
		t.Fatalf("the events table was read %d times, want 2", len(fake.queries))
	}
	if bound, ok := eventsQueryBound(fake.queries[1]); ok {
		t.Errorf("the daily export read from the bound %q, want the whole table", bound)
	}
	if got, want := eventsExportIDs(t, dbDir), []string{"e2"}; !slices.Equal(got, want) {
		t.Errorf("the export holds %v, want %v once the deleted issue's event is gone", got, want)
	}
}

// TestExportEventsAppendNeverDuplicatesTheBoundary: an export that is a day
// short of the daily read appends the rows of the boundary second once, even
// though the query hands them back every cycle.
func TestExportEventsAppendNeverDuplicatesTheBoundary(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
	fake := &eventsTableFake{rows: []eventsTestRow{{"e1", "2026-10-31 10:00:00"}}}
	d.jsonlTableExportFn = fake.exporter(t, nil)
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	dataDir := t.TempDir()

	for i := 0; i < 3; i++ {
		if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
			t.Fatalf("export %d: %v", i+1, err)
		}
	}
	if got, want := eventsExportIDs(t, dbDir), []string{"e1"}; !slices.Equal(got, want) {
		t.Errorf("the export holds %v after three cycles, want %v", got, want)
	}
	for i, q := range fake.queries[1:] {
		if bound, ok := eventsQueryBound(q); !ok || bound != "2026-10-31 10:00:00" {
			t.Errorf("cycle %d read from bound %q (bounded %v), want the boundary the file ends at", i+2, bound, ok)
		}
	}
}

// TestExportEventsKeepsTheBoundaryRowsWhenAnAppendStaysInTheSecond: a cycle
// whose only new rows are stamped at the boundary second grows the run the file
// ends in, and the ids that run already held stay known to the next cycle. A
// boundary list replaced by the fresh rows alone reads them as new and appends
// them a second time (gt-l7eeb).
func TestExportEventsKeepsTheBoundaryRowsWhenAnAppendStaysInTheSecond(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
	fake := &eventsTableFake{rows: []eventsTestRow{{"e1", "2026-10-31 10:00:00"}}}
	d.jsonlTableExportFn = fake.exporter(t, nil)
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	dataDir := t.TempDir()
	export := func(cycle int) {
		t.Helper()
		if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}

	export(1) // the whole table, ending at 10:00:00 with e1
	fake.rows = append(fake.rows, eventsTestRow{"e2", "2026-10-31 10:01:00"})
	export(2) // the boundary moves to 10:01:00, whose run holds e2
	// Every row from here on is stamped at the boundary second the file ends
	// on, so each cycle adds to the run the previous one recorded.
	fake.rows = append(fake.rows, eventsTestRow{"e3", "2026-10-31 10:01:00"})
	export(3)
	export(4) // no new row: the boundary run read back must be recognised

	want := []string{"e1", "e2", "e3"}
	if got := eventsExportIDs(t, dbDir); !slices.Equal(got, want) {
		t.Errorf("the export holds %v, want every row once in created_at, id order: %v", got, want)
	}
	state, err := readEventsExportState(dbDir)
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	if state.Rows != len(want) {
		t.Errorf("state.Rows = %d, want the %d rows the file holds", state.Rows, len(want))
	}
	if state.LastCreatedAt != "2026-10-31 10:01:00" {
		t.Errorf("state.LastCreatedAt = %q, want the second the file ends at", state.LastCreatedAt)
	}
	for i, q := range fake.queries[2:] {
		if bound, ok := eventsQueryBound(q); !ok || bound != "2026-10-31 10:01:00" {
			t.Errorf("cycle %d read from bound %q (bounded %v), want the boundary the file ends at", i+3, bound, ok)
		}
	}
}

// TestAppendEventRowsKeepsAShardedExportInOrder: the append merges the new
// boundary rows into the run the file ends in and, when the last shard fills,
// continues in a shard of its own without losing a row.
func TestAppendEventRowsKeepsAShardedExportInOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rows := []json.RawMessage{
		json.RawMessage(`{"id":"e1","created_at":"2026-10-31 10:00:00","payload":"` + strings.Repeat("x", 120) + `"}`),
		json.RawMessage(`{"id":"e2","created_at":"2026-10-31 10:01:00","payload":"` + strings.Repeat("x", 120) + `"}`),
	}
	if _, err := writeTableShards(dir, "events", rows, 400); err != nil {
		t.Fatalf("writing the export: %v", err)
	}
	state := &eventsExportState{Version: eventsExportStateVersion, LastCreatedAt: "2026-10-31 10:01:00", BoundaryIDs: []string{"e2"}, Rows: 2}

	appended, err := appendEventRows(dir, []json.RawMessage{
		json.RawMessage(`{"id":"e2","created_at":"2026-10-31 10:01:00","payload":"` + strings.Repeat("x", 120) + `"}`),
		json.RawMessage(`{"id":"e3","created_at":"2026-10-31 10:02:00","payload":"` + strings.Repeat("x", 120) + `"}`),
	}, state, 400, jsonlShardHardLimit)
	if err != nil {
		t.Fatalf("appendEventRows: %v", err)
	}
	if appended != 1 {
		t.Errorf("appended = %d, want 1 (the boundary row is already in the file)", appended)
	}
	if got, want := eventsExportIDs(t, dir), []string{"e1", "e2", "e3"}; !slices.Equal(got, want) {
		t.Errorf("the export holds %v, want %v", got, want)
	}
	if paths, err := jsonlShardPaths(dir, "events"); err != nil || len(paths) < 2 {
		t.Errorf("the export holds %d shards (%v), want the append to have cut a new one", len(paths), err)
	}
	if state.Rows != 3 || state.LastCreatedAt != "2026-10-31 10:02:00" || !slices.Equal(state.BoundaryIDs, []string{"e3"}) {
		t.Errorf("state = %+v, want 3 rows ending at e3", state)
	}
}

// TestAppendEventRowsRefusesABoundaryRunAcrossShards: when the rows of the
// boundary second fill the last shard and continue into the one before it, the
// append cannot merge them in one file, so it reports that instead of writing a
// file that is out of order.
func TestAppendEventRowsRefusesABoundaryRunAcrossShards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	row := func(id string) json.RawMessage {
		return json.RawMessage(`{"id":"` + id + `","created_at":"2026-10-31 10:00:00","payload":"` + strings.Repeat("x", 120) + `"}`)
	}
	if _, err := writeTableShards(dir, "events", []json.RawMessage{row("e1"), row("e2")}, 150); err != nil {
		t.Fatalf("writing the export: %v", err)
	}
	state := &eventsExportState{Version: eventsExportStateVersion, LastCreatedAt: "2026-10-31 10:00:00", BoundaryIDs: []string{"e1", "e2"}, Rows: 2}

	_, err := appendEventRows(dir, []json.RawMessage{row("e3")}, state, 150, jsonlShardHardLimit)
	if err == nil {
		t.Fatal("the append accepted a boundary run that spans shards")
	}
	if !strings.Contains(err.Error(), "span") {
		t.Errorf("error = %v, want it to name the span", err)
	}
}

// TestAppendNewEventsRefusesARowOlderThanTheBoundary covers the defensive
// guard alone: Dolt's bound does not return a row stamped before it, but a
// staged read that did would be unplaceable at the end of the file, so the
// append refuses it and leaves the export as it was (gt-l7eeb).
func TestAppendNewEventsRefusesARowOlderThanTheBoundary(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(eventsFailureNow)
	dir := t.TempDir()
	seedEventsExport(t, dir, eventsTestRow{"e1", "2026-10-31 10:00:00"})
	before := snapshotEventsExport(t, dir)
	state, err := readEventsExportState(dir)
	if err != nil {
		t.Fatalf("reading the seeded state: %v", err)
	}

	_, err = d.appendNewEvents("hq", dir, t.TempDir(), time.Minute,
		stagedEventsExport(eventsLine("e0", "2026-10-31 09:00:00")), state)
	if err == nil {
		t.Fatal("the append accepted a row stamped before the boundary")
	}
	if !strings.Contains(err.Error(), "older than the boundary") {
		t.Errorf("error = %v, want it to say the row is older than the boundary", err)
	}
	before.assertHeld(t, dir)
}

// TestExportEventsRebuildsTheWholeTableWhenTheAppendFails: an incremental read
// that fails cannot be built on, so the cycle reads the whole table with what
// is left of its budget and replaces the export (gt-l7eeb).
func TestExportEventsRebuildsTheWholeTableWhenTheAppendFails(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	d.clock = clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
	fake := &eventsTableFake{rows: []eventsTestRow{
		{"e1", "2026-10-31 10:00:00"},
		{"e2", "2026-10-31 10:01:00"},
	}}
	base := fake.exporter(t, nil)
	killIncremental := true
	d.jsonlTableExportFn = func(table string, q beadsql.Query, dir, dataDir string, budget time.Duration) (int, error) {
		if table == "events" && killIncremental {
			text, err := q.Inline()
			if err != nil {
				t.Fatalf("events query: %v", err)
			}
			if _, bounded := eventsQueryBound(text); bounded {
				killIncremental = false
				return 0, errors.New("the incremental read was killed")
			}
		}
		return base(table, q, dir, dataDir, budget)
	}
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	dataDir := t.TempDir()

	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if len(fake.queries) != 2 {
		t.Fatalf("the events table was read %d times, want 2", len(fake.queries))
	}
	if bound, ok := eventsQueryBound(fake.queries[1]); ok {
		t.Errorf("the fallback still read from bound %q, want the whole table", bound)
	}
	if got, want := eventsExportIDs(t, dbDir), []string{"e1", "e2"}; !slices.Equal(got, want) {
		t.Errorf("the export holds %v, want the whole table %v", got, want)
	}
}

// TestFullExportCatchesALateCommittedRow: a row that commits after the export
// passed its second, carrying an older created_at, is not returned by the
// incremental read; the daily whole-table pass is what includes it, exactly
// once (gt-l7eeb).
func TestFullExportCatchesALateCommittedRow(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
	d.clock = clk
	fake := &eventsTableFake{rows: []eventsTestRow{
		{"e1", "2026-10-31 10:00:00"},
		{"e2", "2026-10-31 10:01:00"},
	}}
	d.jsonlTableExportFn = fake.exporter(t, nil)
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	dataDir := t.TempDir()

	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("first export: %v", err)
	}
	// A row commits late with a created_at older than the recorded boundary.
	fake.rows = append(fake.rows, eventsTestRow{"late", "2026-10-31 09:30:00"})

	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("incremental export: %v", err)
	}
	if bound, ok := eventsQueryBound(fake.queries[1]); !ok || bound != "2026-10-31 10:01:00" {
		t.Errorf("the incremental export read from bound %q (bounded %v), want 2026-10-31 10:01:00", bound, ok)
	}
	if got := eventsExportIDs(t, dbDir); slices.Contains(got, "late") {
		t.Errorf("the incremental export holds %v, want the late row left to the full pass", got)
	}

	clk.Advance(eventsFullExportEvery)
	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("daily export: %v", err)
	}
	if bound, ok := eventsQueryBound(fake.queries[2]); ok {
		t.Errorf("the daily export read from bound %q, want the whole table", bound)
	}
	want := []string{"late", "e1", "e2"}
	if got := eventsExportIDs(t, dbDir); !slices.Equal(got, want) {
		t.Errorf("the export holds %v after the full pass, want %v", got, want)
	}
}

// TestEventsExportBoundaryCoversTheRunAcrossShards: the rows sharing the
// newest created_at can fill a shard and continue into the one before it, and
// the boundary must name all of them, or the next append adds them a second
// time (gt-l7eeb).
func TestEventsExportBoundaryCoversTheRunAcrossShards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	row := func(id, at string) json.RawMessage {
		return json.RawMessage(`{"id":"` + id + `","created_at":"` + at + `","payload":"` + strings.Repeat("x", 120) + `"}`)
	}
	// Cut at 150 bytes: every row lands in a shard of its own, so the rows of
	// the newest second run across all of them.
	if _, err := writeTableShards(dir, "events", []json.RawMessage{
		row("e1", "2026-10-31 10:01:00"),
		row("e2a", "2026-10-31 10:02:00"),
		row("e2b", "2026-10-31 10:02:00"),
	}, 150); err != nil {
		t.Fatalf("writing the export: %v", err)
	}

	boundary, ids, err := eventsExportBoundary(dir)
	if err != nil {
		t.Fatalf("eventsExportBoundary: %v", err)
	}
	if boundary != "2026-10-31 10:02:00" {
		t.Errorf("boundary = %q, want 2026-10-31 10:02:00", boundary)
	}
	slices.Sort(ids)
	if want := []string{"e2a", "e2b"}; !slices.Equal(ids, want) {
		t.Errorf("boundary ids = %v, want every row of that second %v", ids, want)
	}
}

// TestExportAllEventsIsPaged: a full export reads the events table through the
// page query and never the whole-table one, and a read or a state write that
// fails is an error. Only a read that succeeds is sampled.
func TestExportAllEventsIsPaged(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		readErr     error
		ops         eventsExportOps
		wantErr     bool
		wantSampled bool
	}{
		{name: "a finished read", ops: osEventsExportOps(), wantSampled: true},
		{name: "a state that cannot be written", ops: eventsOpsWithRename(errors.New("the state cannot be replaced")), wantErr: true, wantSampled: true},
		{name: "a page that fails", ops: osEventsExportOps(), readErr: errors.New("the read was cut off"), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := backupTestDaemon(t)
			clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 10, 5, 0, 0, time.UTC))
			d.clock = clk
			fake := &eventsTableFake{rows: []eventsTestRow{{"e1", "2026-10-31 10:00:00"}, {"e2", "2026-10-31 10:01:00"}}}
			table := fake.exporter(t, nil)
			// Every read takes time on the daemon clock: a sample needs it.
			read := jsonlTableExporter(func(name string, q beadsql.Query, dir, dataDir string, budget time.Duration) (int, error) {
				clk.Advance(time.Second)
				return table(name, q, dir, dataDir, budget)
			})
			if tt.readErr != nil {
				read = func(_ string, q beadsql.Query, _, _ string, _ time.Duration) (int, error) {
					clk.Advance(time.Second)
					text, err := q.Inline()
					if err != nil {
						t.Fatalf("events query: %v", err)
					}
					fake.queries = append(fake.queries, text)
					return 0, tt.readErr
				}
			}
			dir := t.TempDir()

			n, err := d.exportAllEventsToJsonlWith(tt.ops, "hq", dir, t.TempDir(), time.Minute, read)

			if (err != nil) != tt.wantErr {
				t.Fatalf("export = (%d, %v), want an error: %v", n, err, tt.wantErr)
			}
			page, err := beadsql.BackupEventsPage("hq", "", "", eventsFullPageRows).Inline()
			if err != nil {
				t.Fatalf("the page query: %v", err)
			}
			if want := []string{page}; !slices.Equal(fake.queries, want) {
				t.Errorf("the export asked %q, want the first page only %q", fake.queries, want)
			}
			if _, _, sampled := d.jsonlRates.PerRow("hq", "events"); sampled != tt.wantSampled {
				t.Errorf("events sampled = %v, want %v", sampled, tt.wantSampled)
			}
			state, stateErr := readEventsExportState(dir)
			if tt.wantErr {
				if stateErr == nil {
					t.Errorf("a failed export recorded the state %+v", *state)
				}
				return
			}
			if stateErr != nil || n != 2 || state.Rows != 2 {
				t.Errorf("export = %d rows, state %+v (%v), want 2 rows recorded", n, state, stateErr)
			}
		})
	}
}

// TestExportAllEventsPagedState: a full export read in pages records the state
// the next append resumes from. The last two rows share a created_at and the
// page boundary falls between them, so the boundary run is split across two
// pages: the state must still hold every row and both boundary ids, and the
// incremental cycle after it must append only the rows newer than the boundary
// (gt-hlyx5t).
func TestExportAllEventsPagedState(t *testing.T) {
	t.Parallel()
	// One row more than a page, so the full export reads two pages.
	n := eventsFullPageRows + 1
	base := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	stamp := func(sec int) string { return base.Add(time.Duration(sec) * time.Second).Format("2006-01-02 15:04:05") }
	uuid := func(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", i) }
	var rows []eventsTestRow
	var wantIDs []string
	for i := range n {
		sec := i
		if i == n-1 {
			sec = n - 2 // the last two rows share a created_at
		}
		rows = append(rows, eventsTestRow{uuid(i), stamp(sec)})
		wantIDs = append(wantIDs, uuid(i))
	}

	d, _ := backupTestDaemon(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC))
	d.clock = clk
	fake := &eventsTableFake{rows: rows}
	d.jsonlTableExportFn = fake.exporter(t, nil)
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "hq")
	dataDir := t.TempDir()

	got, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false) // no state yet: the paged full export
	if err != nil {
		t.Fatalf("full export: %v", err)
	}
	if got != n {
		t.Errorf("full export count = %d, want %d", got, n)
	}
	if len(fake.queries) != 2 {
		t.Fatalf("the full export read %d page(s), want 2", len(fake.queries))
	}
	state, err := readEventsExportState(dbDir)
	if err != nil {
		t.Fatalf("reading the state: %v", err)
	}
	if state.Rows != n {
		t.Errorf("state.Rows = %d, want the %d rows written", state.Rows, n)
	}
	if !state.FullExportedAt.Equal(clk.Now()) {
		t.Errorf("state.FullExportedAt = %v, want the daemon clock %v", state.FullExportedAt, clk.Now())
	}
	if want := stamp(n - 2); state.LastCreatedAt != want {
		t.Errorf("state.LastCreatedAt = %q, want the shared stamp %q", state.LastCreatedAt, want)
	}
	boundaryIDs := slices.Clone(state.BoundaryIDs)
	slices.Sort(boundaryIDs)
	if want := []string{uuid(n - 2), uuid(n - 1)}; !slices.Equal(boundaryIDs, want) {
		t.Errorf("state.BoundaryIDs = %v, want both rows of the shared stamp %v", boundaryIDs, want)
	}
	if got := eventsExportIDs(t, dbDir); !slices.Equal(got, wantIDs) {
		t.Fatalf("the full export holds %d rows in order, want the %d written once each", len(got), len(wantIDs))
	}

	// Two rows newer than the boundary: the next cycle appends only them.
	fake.rows = append(fake.rows, eventsTestRow{uuid(n), stamp(n + 1)}, eventsTestRow{uuid(n + 1), stamp(n + 2)})
	wantIDs = append(wantIDs, uuid(n), uuid(n+1))
	if _, err := d.exportDatabaseToJsonl("hq", gitRepo, dataDir, false); err != nil {
		t.Fatalf("incremental export: %v", err)
	}
	if len(fake.queries) != 3 {
		t.Fatalf("the cycles read %d queries, want 2 pages then 1 incremental read", len(fake.queries))
	}
	if bound, ok := eventsQueryBound(fake.queries[2]); !ok || bound != stamp(n-2) {
		t.Errorf("the incremental read came from bound %q (bounded %v), want the boundary %q", bound, ok, stamp(n-2))
	}
	if got := eventsExportIDs(t, dbDir); !slices.Equal(got, wantIDs) {
		t.Errorf("after the incremental cycle the export holds %d rows, want every row once: %d", len(got), len(wantIDs))
	}
	state, err = readEventsExportState(dbDir)
	if err != nil {
		t.Fatalf("reading the state after the incremental cycle: %v", err)
	}
	if state.Rows != len(wantIDs) || state.LastCreatedAt != stamp(n+2) || !slices.Equal(state.BoundaryIDs, []string{uuid(n + 1)}) {
		t.Errorf("state after the incremental cycle = rows %d, boundary %q %v; want rows %d, boundary %q [%s]",
			state.Rows, state.LastCreatedAt, state.BoundaryIDs, len(wantIDs), stamp(n+2), uuid(n+1))
	}
}
