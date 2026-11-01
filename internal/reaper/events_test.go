package reaper

import (
	"context"
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

	opts := EventsPurgeOptions{Now: eventsNow, Retain: 30 * 24 * time.Hour, Batch: 2, MaxBatches: 10}
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
	opts := EventsPurgeOptions{Now: eventsNow, Retain: 30 * 24 * time.Hour, Batch: 10, MaxBatches: 5}

	dry, err := PurgeEvents(context.Background(), db, EventsPurgeOptions{
		Now: eventsNow, Retain: opts.Retain, Batch: opts.Batch, MaxBatches: opts.MaxBatches, DryRun: true,
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
