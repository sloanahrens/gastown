package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// usageRunRecord is one run's work record for bead, counting session's messages.
func usageRunRecord(bead, session string, start, end time.Time, input int64) dashboard.UsageRecord {
	return usageattrib.Build(usageattrib.Facts{
		Bead:      bead,
		Kind:      dashboard.UsageKindWork,
		Class:     dashboard.UsageClassPolecat,
		Rig:       "gastown",
		Workspace: "gastown",
		Start:     start,
		End:       end,
		Complete:  true,
	}, []usageattrib.SessionUsage{{
		ID:       session,
		Complete: true,
		Main: []usageattrib.Message{{
			Model:  "claude-opus-5-5",
			TS:     start.Add(time.Minute).Format(time.RFC3339),
			Input:  input,
			Output: 20,
		}},
	}})
}

// TestWriteUsageRecordRowCarriesTheFilesMergedRecord: a bead that runs twice is
// filed as two rows beside one merged file, and the newest row carries the
// file's whole record and its sessions, not the second run's alone. The
// dashboard drops the file of a bead and kind a row holds, so a row of one run
// would lose the other run's usage from the panel.
func TestWriteUsageRecordRowCarriesTheFilesMergedRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	usageDir := t.TempDir()
	store := beadsfake.New()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	first := usageRunRecord("gt-abc", "s-1", start, start.Add(time.Hour), 100)
	second := usageRunRecord("gt-abc", "s-2", start.Add(time.Hour), start.Add(2*time.Hour), 200)

	if err := writeUsageRecord(ctx, store, usageDir, first, []string{"s-1"}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := writeUsageRecord(ctx, store, usageDir, second, []string{"s-2"}); err != nil {
		t.Fatalf("second run: %v", err)
	}

	rows, err := store.ReadUsageRecords(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("reading the usage records: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d, want one per run", len(rows))
	}
	file, err := usageattrib.ReadWork(usageDir, "gt-abc")
	if err != nil {
		t.Fatalf("reading the record file: %v", err)
	}
	newest := rows[0]
	if got, want := strings.Join(newest.Sessions, ","), strings.Join(file.Sessions, ","); got != want {
		t.Errorf("newest row counts sessions %q, want the file's %q", got, want)
	}
	if newest.Record.Totals != file.Record.Totals {
		t.Errorf("newest row totals %+v, want the file's %+v", newest.Record.Totals, file.Record.Totals)
	}
}

// TestWriteUsageRecordWithoutAStoreReportsTheRowNotWritten: a daemon whose store
// was not wired writes the record file and reports the row it did not write, so
// an unwired store costs the row and never the record.
func TestWriteUsageRecordWithoutAStoreReportsTheRowNotWritten(t *testing.T) {
	t.Parallel()
	usageDir := t.TempDir()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	rec := usageRunRecord("gt-abc", "s-1", start, start.Add(time.Hour), 100)

	err := writeUsageRecord(context.Background(), nil, usageDir, rec, []string{"s-1"})
	var rowOnly *usageRowNotWrittenError
	if !errors.As(err, &rowOnly) {
		t.Fatalf("write with no store: error = %v, want a row-not-written error", err)
	}
	if !errors.Is(err, errNoUsageStore) {
		t.Errorf("error %v does not name the missing store", err)
	}
	if _, err := usageattrib.ReadWork(usageDir, "gt-abc"); err != nil {
		t.Errorf("the record file is not written: %v", err)
	}
}

// TestUsageRecordsUnwiredDaemonHasNoStore: a daemon built the way a unit test
// builds one opens no usage_records store, so no test reaches the town's
// database. New wires the production store.
func TestUsageRecordsUnwiredDaemonHasNoStore(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(io.Discard, "", 0)}
	if got := d.usageRecords(); got != nil {
		t.Errorf("an unwired daemon's usage_records store = %T, want none", got)
	}
}
