package reaper

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
)

// eventsNow is the injected clock the retention tests run at. It is fixed and
// well away from the suite's own clock, so a pass that reads time.Now() instead
// of the injected now cannot delete the same rows by coincidence.
var eventsNow = time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)

// newEventsState builds a fake with an events table and the two sibling event
// tables retention must leave alone.
func newEventsState() *fakeReaperState {
	return &fakeReaperState{
		ops:           map[int][]string{},
		eventsTable:   true,
		wispEvents:    []fakeEvent{{id: 1}, {id: 2}},
		journalEvents: []fakeEvent{{id: 1}, {id: 2}},
	}
}

// eventIDs is the ids of rows, in ascending order.
func eventIDs(rows []fakeEvent) []int64 {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.id
	}
	slices.Sort(ids)
	return ids
}

// TestPurgeEventsDeletesOnlyRowsOlderThanTheCutoffInCountedBatches: a row at
// exactly the retention boundary survives, older rows go in passes no larger
// than the batch, the count is the rows removed, and a rerun deletes nothing.
func TestPurgeEventsDeletesOnlyRowsOlderThanTheCutoffInCountedBatches(t *testing.T) {
	t.Parallel()
	state := newEventsState()
	for id := int64(1); id <= 5; id++ {
		state.addEvent(id, eventsNow, 31*24*time.Hour)
	}
	state.addEvent(6, eventsNow, 30*24*time.Hour) // exactly the cutoff: kept
	state.addEvent(7, eventsNow, 29*24*time.Hour)
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	opts := EventsPurgeOptions{
		Now: eventsNow, Retain: 30 * 24 * time.Hour, Batch: 2, MaxBatches: 10,
		Database: eventsTestDatabase, StatePath: eventsWatermarkState(t, eventsTestDatabase, eventsNow),
	}
	result, err := PurgeEvents(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PurgeEvents: %v", err)
	}
	if result.Rows != 5 {
		t.Errorf("rows = %d, want 5 (the 31-day rows only)", result.Rows)
	}
	if result.Batches != 3 {
		t.Errorf("batches = %d, want 3 (2+2+1)", result.Batches)
	}
	if got := eventIDs(state.events); !slices.Equal(got, []int64{6, 7}) {
		t.Errorf("remaining events = %v, want [6 7]: the 30-day and 29-day rows stay", got)
	}

	second, err := PurgeEvents(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("second PurgeEvents: %v", err)
	}
	if second.Rows != 0 {
		t.Errorf("second run deleted %d rows, want 0", second.Rows)
	}
	if got := eventIDs(state.events); !slices.Equal(got, []int64{6, 7}) {
		t.Errorf("remaining events after the rerun = %v, want [6 7]", got)
	}
}

// TestPurgeEventsStopsAtMaxBatchesAndLeavesTheRest: more eligible rows than
// batch x maxBatches leaves the remainder for the next cycle.
func TestPurgeEventsStopsAtMaxBatchesAndLeavesTheRest(t *testing.T) {
	t.Parallel()
	state := newEventsState()
	for id := int64(1); id <= 10; id++ {
		state.addEvent(id, eventsNow, 31*24*time.Hour)
	}
	state.addEvent(11, eventsNow, 10*24*time.Hour) // inside retention: never eligible
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
		Now:        eventsNow,
		Retain:     30 * 24 * time.Hour,
		Batch:      2,
		MaxBatches: 3,
		Database:   eventsTestDatabase,
		StatePath:  eventsWatermarkState(t, eventsTestDatabase, eventsNow),
	})
	if err != nil {
		t.Fatalf("PurgeEvents: %v", err)
	}
	if result.Rows != 6 || result.Batches != 3 {
		t.Errorf("pass = %d rows in %d batches, want 6 rows in 3 batches", result.Rows, result.Batches)
	}
	if got := eventIDs(state.events); !slices.Equal(got, []int64{7, 8, 9, 10, 11}) {
		t.Errorf("remaining events = %v, want [7 8 9 10 11]: four eligible rows wait for the next cycle", got)
	}
}

// TestPurgeEventsDryRunAndOtherTables: a dry run counts the eligible rows and
// deletes nothing, the sibling event tables are untouched by a live run, and a
// database without an events table is skipped without error.
func TestPurgeEventsDryRunAndOtherTables(t *testing.T) {
	t.Parallel()
	state := newEventsState()
	for id := int64(1); id <= 3; id++ {
		state.addEvent(id, eventsNow, 31*24*time.Hour)
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	opts := EventsPurgeOptions{
		Now: eventsNow, Retain: 30 * 24 * time.Hour, Batch: 10, MaxBatches: 5,
		Database: eventsTestDatabase, StatePath: eventsWatermarkState(t, eventsTestDatabase, eventsNow),
	}

	dry, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
		Now: eventsNow, Retain: opts.Retain, Batch: opts.Batch, MaxBatches: opts.MaxBatches, DryRun: true,
		Database: opts.Database, StatePath: opts.StatePath,
	})
	if err != nil {
		t.Fatalf("dry PurgeEvents: %v", err)
	}
	if dry.Rows != 3 || dry.Batches != 0 || !dry.DryRun {
		t.Errorf("dry run = %+v, want 3 rows counted, 0 batches, DryRun", dry)
	}
	if got := eventIDs(state.events); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("dry run left events %v, want all three rows", got)
	}

	live, err := PurgeEvents(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("live PurgeEvents: %v", err)
	}
	if live.Rows != 3 {
		t.Errorf("live run deleted %d rows, want 3", live.Rows)
	}
	if got := eventIDs(state.wispEvents); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("wisp_events rows = %v, want untouched [1 2]", got)
	}
	if got := eventIDs(state.journalEvents); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("bd_events_journal rows = %v, want untouched [1 2]", got)
	}

	bare := &fakeReaperState{ops: map[int][]string{}, eventsTable: false}
	bareDB := openFakeReaperDB(t, bare)
	t.Cleanup(func() { _ = bareDB.Close() })
	skipped, err := PurgeEvents(context.Background(), bareDB, opts)
	if err != nil {
		t.Fatalf("PurgeEvents on a database without events: %v", err)
	}
	if !skipped.Skipped || skipped.Rows != 0 {
		t.Errorf("result = %+v, want Skipped with no rows", skipped)
	}
}

// TestPurgeEventsRefusesUnusableBounds: a clock or a bound the pass cannot use
// is an error, never a pass that deletes everything older than a zero cutoff.
func TestPurgeEventsRefusesUnusableBounds(t *testing.T) {
	t.Parallel()
	valid := EventsPurgeOptions{Now: eventsNow, Retain: 30 * 24 * time.Hour, Batch: 2, MaxBatches: 2}
	tests := []struct {
		name string
		opts EventsPurgeOptions
	}{
		{name: "no clock", opts: EventsPurgeOptions{Retain: valid.Retain, Batch: valid.Batch, MaxBatches: valid.MaxBatches}},
		{name: "no retention", opts: EventsPurgeOptions{Now: eventsNow, Batch: valid.Batch, MaxBatches: valid.MaxBatches}},
		{name: "no batch", opts: EventsPurgeOptions{Now: eventsNow, Retain: valid.Retain, MaxBatches: valid.MaxBatches}},
		{name: "no maxBatches", opts: EventsPurgeOptions{Now: eventsNow, Retain: valid.Retain, Batch: valid.Batch}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			state.addEvent(1, eventsNow, 31*24*time.Hour)
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })

			if _, err := PurgeEvents(context.Background(), db, tt.opts); err == nil {
				t.Fatal("PurgeEvents accepted an unusable bound")
			}
			statements := 0
			for _, count := range state.opCounts() {
				statements += count
			}
			if statements != 0 {
				t.Errorf("a refused pass sent %d statements: %v", statements, state.ops)
			}
		})
	}
}

// TestPurgeEventsReportsAFailedStep: each failing database call is returned,
// never read as nothing to delete.
func TestPurgeEventsReportsAFailedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fail   func(*fakeReaperState) error
		dryRun bool
		want   string
	}{
		{
			name: "table probe",
			fail: func(s *fakeReaperState) error {
				s.probeErr = errors.New("probe failed")
				return s.probeErr
			},
			want: "check events table",
		},
		{
			name:   "dry-run count",
			dryRun: true,
			fail: func(s *fakeReaperState) error {
				s.countErr = errors.New("count failed")
				return s.countErr
			},
			want: "count failed",
		},
		{
			name: "delete",
			fail: func(s *fakeReaperState) error {
				s.deleteErr = errors.New("delete failed")
				return s.deleteErr
			},
			want: "delete failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			state.addEvent(1, eventsNow, 31*24*time.Hour)
			wantErr := tt.fail(state)
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })

			_, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
				Now:        eventsNow,
				Retain:     30 * 24 * time.Hour,
				Batch:      10,
				MaxBatches: 5,
				DryRun:     tt.dryRun,
				Database:   eventsTestDatabase,
				StatePath:  eventsWatermarkState(t, eventsTestDatabase, eventsNow),
			})
			if err == nil || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("PurgeEvents error = %v, want it to wrap %q", err, tt.want)
			}
		})
	}
}

// TestEventsPurgeResultSummary: the pass's one line names the rows, the
// batches, the cutoff, and whether it wrote.
func TestEventsPurgeResultSummary(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2029, 12, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		result EventsPurgeResult
		want   []string
	}{
		{
			name:   "live",
			result: EventsPurgeResult{Rows: 5000, Batches: 2, Cutoff: cutoff},
			want:   []string{"deleted 5000 rows", "2 batches", cutoff.Format(time.RFC3339)},
		},
		{
			name:   "dry",
			result: EventsPurgeResult{Rows: 42, Batches: 0, Cutoff: cutoff, DryRun: true},
			want:   []string{"would delete 42 rows", "dry run", cutoff.Format(time.RFC3339)},
		},
		{
			name:   "skipped",
			result: EventsPurgeResult{Skipped: true},
			want:   []string{"no events table"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			line := tt.result.Summary()
			for _, part := range tt.want {
				if !strings.Contains(line, part) {
					t.Errorf("Summary() = %q, want it to name %q", line, part)
				}
			}
		})
	}
}

// eventsTestDatabase is the database name openFakeReaperDB connects as, and the
// key its events archive watermark is recorded under.
const eventsTestDatabase = "hq"

// eventsWatermarkState writes a maintenance state file whose events archive
// watermark for database is createdAt, and returns its path. The watermark
// carries one archived id at that created_at: a watermark with a created_at
// and no ids fails Validate, so it cannot stand in for a healthy archive.
func eventsWatermarkState(t *testing.T, database string, createdAt time.Time) string {
	t.Helper()
	return eventsWatermarkStateArchived(t, database, createdAt, createdAt)
}

// eventsWatermarkStateArchived writes a maintenance state file whose events
// archive watermark for database is createdAt, whose archived set holds one
// row at archivedAt, and returns its path.
func eventsWatermarkStateArchived(t *testing.T, database string, createdAt, archivedAt time.Time) string {
	t.Helper()
	at := createdAt.UTC().Format("2006-01-02 15:04:05")
	archived := archivedAt.UTC().Format("2006-01-02 15:04:05")
	body := map[string]any{
		"archive_events_watermarks": map[string]any{
			database: map[string]any{
				"created_at": at,
				"archived":   []map[string]string{{"created_at": archived, "id": "archived-row"}},
			},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return writeEventsState(t, data)
}

// writeEventsState writes raw bytes as a maintenance state file and returns its
// path.
func writeEventsState(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maintenance_state.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return path
}

// TestEventsRetentionKeepsRowsInsideTheLookbackWindow: the archive re-reads
// the 24h before its watermark on every run, so a row inside that window is
// kept whether or not the Archived set lists it. A row older than the window
// and older than the retention cutoff is deleted.
func TestEventsRetentionKeepsRowsInsideTheLookbackWindow(t *testing.T) {
	t.Parallel()
	state := newEventsState()
	watermark := eventsNow.Add(-10 * time.Hour)
	windowStart := watermark.Add(-24 * time.Hour)
	state.addEvent(1, eventsNow, 11*time.Hour) // inside the window, not in Archived
	state.addEvent(2, eventsNow, 11*time.Hour) // inside the window, in Archived
	state.addEvent(3, eventsNow, 40*time.Hour) // older than the window and the retention cutoff
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
		Now:        eventsNow,
		Retain:     1 * time.Hour,
		Batch:      10,
		MaxBatches: 5,
		Database:   eventsTestDatabase,
		StatePath:  eventsWatermarkStateArchived(t, eventsTestDatabase, watermark, eventsNow.Add(-11*time.Hour)),
	})
	if err != nil {
		t.Fatalf("PurgeEvents: %v", err)
	}
	if result.Rows != 1 {
		t.Errorf("rows = %d, want 1 (only the row older than the window)", result.Rows)
	}
	if !result.Cutoff.Equal(windowStart) {
		t.Errorf("cutoff = %s, want the window start %s", result.Cutoff, windowStart)
	}
	if got := eventIDs(state.events); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("remaining events = %v, want [1 2]: both rows inside the lookback window stay", got)
	}
}

// TestEventsRetentionBoundaryIsStrict: a row exactly at the bound is kept and
// a row one second older is deleted, and the DELETE the pass sends asks for
// created_at < bound, not <=.
func TestEventsRetentionBoundaryIsStrict(t *testing.T) {
	t.Parallel()
	state := newEventsState()
	watermark := eventsNow.Add(-10 * time.Hour)
	bound := watermark.Add(-24 * time.Hour)
	state.addEvent(1, eventsNow, 34*time.Hour)             // exactly at the bound: kept
	state.addEvent(2, eventsNow, 34*time.Hour+time.Second) // one second older: deleted
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
		Now:        eventsNow,
		Retain:     1 * time.Hour, // the retention cutoff is later, so the window governs
		Batch:      10,
		MaxBatches: 5,
		Database:   eventsTestDatabase,
		StatePath:  eventsWatermarkState(t, eventsTestDatabase, watermark),
	})
	if err != nil {
		t.Fatalf("PurgeEvents: %v", err)
	}
	if result.Rows != 1 {
		t.Errorf("rows = %d, want 1 (only the row one second older than the bound)", result.Rows)
	}
	if !result.Cutoff.Equal(bound) {
		t.Errorf("cutoff = %s, want %s", result.Cutoff, bound)
	}
	if got := eventIDs(state.events); !slices.Equal(got, []int64{1}) {
		t.Errorf("remaining events = %v, want [1]: the row exactly at the bound is kept", got)
	}
	deletes := 0
	for _, ops := range state.ops {
		for _, op := range ops {
			if !strings.Contains(op, "DELETE FROM events") {
				continue
			}
			deletes++
			if strings.Contains(op, "created_at <=") {
				t.Errorf("delete statement = %q, want the strict predicate created_at < bound", op)
			}
		}
	}
	if deletes == 0 {
		t.Fatal("the pass sent no bounded events delete")
	}
}

// TestEventsRetentionWaitsWithoutAWatermark: a database with no watermark
// entry, a zero watermark, or a missing state file deletes nothing. The pass
// says it is waiting for the archive and is counted as skipped, not as a pass
// that deleted zero rows and not as a failure.
func TestEventsRetentionWaitsWithoutAWatermark(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		statePath func(t *testing.T) string
	}{
		{
			name: "no entry for the database",
			statePath: func(t *testing.T) string {
				return eventsWatermarkState(t, "other_db", eventsNow.Add(-40*24*time.Hour))
			},
		},
		{
			name: "entry with no created_at",
			statePath: func(t *testing.T) string {
				return writeEventsState(t, []byte(`{"archive_events_watermarks":{"hq":{}}}`))
			},
		},
		{
			name: "state file missing",
			statePath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "maintenance_state.json")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			state.addEvent(1, eventsNow, 400*24*time.Hour)
			state.addEvent(2, eventsNow, 31*24*time.Hour)
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })

			result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
				Now:        eventsNow,
				Retain:     30 * 24 * time.Hour,
				Batch:      10,
				MaxBatches: 5,
				Database:   eventsTestDatabase,
				StatePath:  tt.statePath(t),
			})
			if err != nil {
				t.Fatalf("PurgeEvents: %v, want a skipped pass with no error", err)
			}
			if !result.Skipped || result.Waiting == "" {
				t.Errorf("result = %+v, want Skipped with a waiting reason", result)
			}
			if result.Rows != 0 || result.Batches != 0 {
				t.Errorf("waiting pass = %d rows in %d batches, want none", result.Rows, result.Batches)
			}
			if got := eventIDs(state.events); !slices.Equal(got, []int64{1, 2}) {
				t.Errorf("remaining events = %v, want [1 2] untouched", got)
			}
			if line := result.Summary(); !strings.Contains(line, "waiting for the events archive watermark") {
				t.Errorf("Summary() = %q, want it to say the pass is waiting for the archive", line)
			}
		})
	}
}

// TestEventsRetentionUsesTheEarlierOfCutoffAndWindowStart: the pass cuts at
// the earlier of the retention cutoff and the archive's window start (the
// watermark minus its 24h lookback), so whichever is older bounds the delete.
func TestEventsRetentionUsesTheEarlierOfCutoffAndWindowStart(t *testing.T) {
	t.Parallel()
	retentionCutoff := eventsNow.Add(-30 * 24 * time.Hour)
	tests := []struct {
		name          string
		watermark     time.Time
		retain        time.Duration
		wantCutoff    time.Time
		wantRemaining []int64
	}{
		{
			name:          "retention cutoff earlier than the window start",
			watermark:     eventsNow.Add(-1 * time.Hour), // window start = now - 25h
			retain:        30 * 24 * time.Hour,
			wantCutoff:    retentionCutoff,
			wantRemaining: []int64{2, 3},
		},
		{
			name:          "window start earlier than the retention cutoff",
			watermark:     eventsNow.Add(-10 * time.Hour), // window start = now - 34h
			retain:        1 * time.Hour,
			wantCutoff:    eventsNow.Add(-34 * time.Hour),
			wantRemaining: []int64{3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			state.addEvent(1, eventsNow, 31*24*time.Hour)
			state.addEvent(2, eventsNow, 40*time.Hour)
			state.addEvent(3, eventsNow, 20*time.Hour)
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })

			result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
				Now:        eventsNow,
				Retain:     tt.retain,
				Batch:      10,
				MaxBatches: 5,
				Database:   eventsTestDatabase,
				StatePath:  eventsWatermarkState(t, eventsTestDatabase, tt.watermark),
			})
			if err != nil {
				t.Fatalf("PurgeEvents: %v", err)
			}
			if !result.Cutoff.Equal(tt.wantCutoff) {
				t.Errorf("cutoff = %s, want %s", result.Cutoff, tt.wantCutoff)
			}
			if got := eventIDs(state.events); !slices.Equal(got, tt.wantRemaining) {
				t.Errorf("remaining events = %v, want %v", got, tt.wantRemaining)
			}
		})
	}

	// The window start itself is the watermark minus the archive's 24h
	// lookback, truncated to whole seconds. The daemon's
	// EventsArchiveWatermark.Since is pinned to this in
	// TestEventsRetentionWindowStartMatchesTheArchiveSince (package daemon, which
	// can import both), so changing the lookback in either place turns a test
	// red.
	t.Run("window start is the watermark minus the lookback", func(t *testing.T) {
		t.Parallel()
		got, err := EventsArchiveWindowStart("2030-01-15 02:00:00.5")
		if err != nil {
			t.Fatalf("EventsArchiveWindowStart: %v", err)
		}
		want := time.Date(2030, 1, 14, 2, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("window start = %s, want %s", got, want)
		}
	})
}

// TestEventsRetentionReadsEveryWatermarkForm: the watermark's created_at is
// read in each datetime form beadsql.TimestampLiteral accepts, and a form with
// no zone is read as UTC. The cutoff is the window start, the watermark minus
// the lookback, truncated to whole seconds.
func TestEventsRetentionReadsEveryWatermarkForm(t *testing.T) {
	t.Parallel()
	// Older than the retention cutoff (2029-12-16 12:00 UTC), so the archive's
	// window start is the cutoff the pass reports.
	want := time.Date(2029, 12, 13, 11, 0, 0, 0, time.UTC)
	for _, form := range []string{
		"2029-12-14 11:00:00.5",
		"2029-12-14T11:00:00.5",
		"2029-12-14 11:00:00.5Z",
		"2029-12-14T11:00:00.5+00:00",
	} {
		t.Run(form, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })
			statePath := writeEventsState(t, []byte(`{"archive_events_watermarks":{"hq":{"created_at":"`+form+`","archived":[{"created_at":"`+form+`","id":"row-1"}]}}}`))

			result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
				Now:        eventsNow,
				Retain:     30 * 24 * time.Hour,
				Batch:      10,
				MaxBatches: 5,
				DryRun:     true,
				Database:   eventsTestDatabase,
				StatePath:  statePath,
			})
			if err != nil {
				t.Fatalf("PurgeEvents: %v", err)
			}
			if !result.Cutoff.Equal(want) {
				t.Errorf("cutoff = %s, want %s", result.Cutoff, want)
			}
		})
	}
}

// TestEventsRetentionRefusesACorruptStateFile: a state file that exists but
// cannot be read, is not valid JSON, or names an unsupported version, deletes
// nothing and returns an error naming the database. It is a refusal, never a
// waiting pass.
func TestEventsRetentionRefusesACorruptStateFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		statePath func(t *testing.T) string
	}{
		{
			name: "not JSON",
			statePath: func(t *testing.T) string {
				return writeEventsState(t, []byte(`{not json`))
			},
		},
		{
			name: "unsupported version",
			statePath: func(t *testing.T) string {
				return writeEventsState(t, []byte(`{"version":2,"archive_events_watermarks":{}}`))
			},
		},
		{
			name: "unreadable path",
			statePath: func(t *testing.T) string {
				return t.TempDir() // a directory cannot be read as a state file
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			state.addEvent(1, eventsNow, 400*24*time.Hour)
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })

			result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
				Now:        eventsNow,
				Retain:     30 * 24 * time.Hour,
				Batch:      10,
				MaxBatches: 5,
				Database:   eventsTestDatabase,
				StatePath:  tt.statePath(t),
			})
			if err == nil {
				t.Fatal("PurgeEvents read a corrupt state file as no watermark")
			}
			if !strings.Contains(err.Error(), eventsTestDatabase) {
				t.Errorf("error = %v, want it to name the database %s", err, eventsTestDatabase)
			}
			if result.Waiting != "" || result.Skipped {
				t.Errorf("result = %+v, want a refusal, not a waiting skipped pass", result)
			}
			if got := eventIDs(state.events); !slices.Equal(got, []int64{1}) {
				t.Errorf("remaining events = %v, want [1] untouched", got)
			}
		})
	}
}

// TestEventsRetentionRefusesACorruptWatermark: a watermark that fails
// Validate, or whose created_at does not parse, deletes nothing and returns an
// error naming the database. It is a refusal, never a waiting pass.
func TestEventsRetentionRefusesACorruptWatermark(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry string
	}{
		{name: "created_at with no ids", entry: `"created_at":"2029-12-14 11:00:00"`},
		{name: "created_at is not a timestamp", entry: `"created_at":"last tuesday"`},
		{name: "created_at with no such date", entry: `"created_at":"2030-13-45 11:00:00","archived":[{"created_at":"2030-13-45 11:00:00","id":"row-1"}]`},
		{name: "archived row with a created_at that is not a timestamp", entry: `"created_at":"2029-12-14 11:00:00","archived":[{"created_at":"last tuesday","id":"row-1"}]`},
		{name: "archived row with an empty id", entry: `"created_at":"2029-12-14 11:00:00","archived":[{"created_at":"2029-12-14 11:00:00","id":""}]`},
		{name: "ids without created_at", entry: `"archived":[{"created_at":"2029-12-14 11:00:00","id":"row-1"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			state.addEvent(1, eventsNow, 400*24*time.Hour)
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })

			statePath := writeEventsState(t, []byte(`{"archive_events_watermarks":{"hq":{`+tt.entry+`}}}`))
			result, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
				Now:        eventsNow,
				Retain:     30 * 24 * time.Hour,
				Batch:      10,
				MaxBatches: 5,
				Database:   eventsTestDatabase,
				StatePath:  statePath,
			})
			if err == nil {
				t.Fatal("PurgeEvents accepted a corrupt watermark")
			}
			if !strings.Contains(err.Error(), eventsTestDatabase) {
				t.Errorf("error = %v, want it to name the database %s", err, eventsTestDatabase)
			}
			if result.Waiting != "" || result.Skipped {
				t.Errorf("result = %+v, want a refusal, not a waiting skipped pass", result)
			}
			if got := eventIDs(state.events); !slices.Equal(got, []int64{1}) {
				t.Errorf("remaining events = %v, want [1] untouched", got)
			}
		})
	}
}

// TestEventsRetentionNeedsTheDatabaseAndStatePath: a pass with no database name
// or no state path cannot find its watermark, so it is refused before it
// touches the database.
func TestEventsRetentionNeedsTheDatabaseAndStatePath(t *testing.T) {
	t.Parallel()
	statePath := eventsWatermarkState(t, eventsTestDatabase, eventsNow.Add(-40*24*time.Hour))
	tests := []struct {
		name     string
		database string
		path     string
	}{
		{name: "no database", database: "", path: statePath},
		{name: "no state path", database: eventsTestDatabase, path: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := newEventsState()
			state.addEvent(1, eventsNow, 400*24*time.Hour)
			db := openFakeReaperDB(t, state)
			t.Cleanup(func() { _ = db.Close() })

			_, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
				Now:        eventsNow,
				Retain:     30 * 24 * time.Hour,
				Batch:      10,
				MaxBatches: 5,
				Database:   tt.database,
				StatePath:  tt.path,
			})
			if err == nil {
				t.Fatal("PurgeEvents accepted a pass with no watermark to read")
			}
			statements := 0
			for _, count := range state.opCounts() {
				statements += count
			}
			if statements != 0 {
				t.Errorf("a refused pass sent %d statements: %v", statements, state.ops)
			}
		})
	}
}
