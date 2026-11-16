package beadsfake

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usagerecord"
)

// TestFakeUsageRecordsContract runs the usage-record suite against the fake, so
// it is runnable alone: go test ./internal/beads/beadsfake -run UsageRecords.
func TestFakeUsageRecordsContract(t *testing.T) {
	t.Parallel()
	RunUsageRecordsContract(t, func(t *testing.T) beads.Client { return New() })
}

// everyFieldUsageRow is the row the round-trip test writes: every record field
// and the session ids at once.
func everyFieldUsageRow() beads.UsageRecordRow {
	seat := "furiosa"
	return beads.UsageRecordRow{
		BuiltAt: time.Date(2026, 11, 15, 17, 5, 0, 0, time.FixedZone("CST", -6*3600)),
		Record: usagerecord.Record{
			Bead:       "gt-usage-1",
			Kind:       dashboard.UsageKindWork,
			Class:      dashboard.UsageClassPolecat,
			Rig:        "gastown",
			Workspace:  "ws-town",
			Seat:       &seat,
			Complete:   true,
			Start:      time.Date(2026, 11, 15, 16, 0, 0, 0, time.FixedZone("CST", -6*3600)),
			End:        time.Date(2026, 11, 15, 17, 0, 0, 0, time.FixedZone("CST", -6*3600)),
			WallS:      3600,
			EstCostUSD: 1.25,
			Models: map[string]usagerecord.Model{
				"opus":   {Calls: 2, Input: 10, Output: 20, CacheCreation: 1, CacheRead: 3, EstCostUSD: 0.75},
				"sonnet": {Calls: 1, Input: 5, Output: 6, CacheCreation: 0, CacheRead: 1, EstCostUSD: 0.5},
			},
			Totals:    usagerecord.Totals{Calls: 3, Input: 15, Output: 26, CacheCreation: 1, CacheRead: 4, Uncached: 42},
			Subagents: usagerecord.Subagents{Calls: 1, Share: 0.5, Uncached: 7},
			Activity:  usagerecord.Activity{ToolCalls: 9, Edits: 3, TestRuns: 2, FailedTestRuns: 1},
		},
		Sessions: []string{"sess-1", "sess-2"},
	}
}

// TestFakeUsageRecordsRoundTrip: a record written as a row reads back equal,
// with the built at in UTC at second precision, as bd sample add stores it, and
// the run's bounds in UTC.
func TestFakeUsageRecordsRoundTrip(t *testing.T) {
	t.Parallel()
	f := New()
	ctx := context.Background()
	want := everyFieldUsageRow()
	mustDo(t, "WriteUsageRecord", f.WriteUsageRecord(ctx, want))
	rows, err := f.ReadUsageRecords(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ReadUsageRecords: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ReadUsageRecords = %v, want the one row written", rows)
	}
	got := rows[0]
	want.BuiltAt = want.BuiltAt.UTC().Truncate(time.Second)
	want.Record.Start = want.Record.Start.UTC()
	want.Record.End = want.Record.End.UTC()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadUsageRecords = %+v, want the row written: %+v", got, want)
	}
}

// TestFakeUsageRecordsCopyTheRow: changing the row a caller wrote after the
// write, or a row a read returned, does not change what the store holds.
func TestFakeUsageRecordsCopyTheRow(t *testing.T) {
	t.Parallel()
	f := New()
	ctx := context.Background()
	row := everyFieldUsageRow()
	row.BuiltAt = sampleAt
	mustDo(t, "WriteUsageRecord", f.WriteUsageRecord(ctx, row))
	*row.Record.Seat = "someone-else"
	row.Record.Models["opus"] = usagerecord.Model{EstCostUSD: 99}
	row.Sessions[0] = "sess-edited"

	rows, err := f.ReadUsageRecords(ctx, time.Time{}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ReadUsageRecords = %v, %v; want the one row written", rows, err)
	}
	if seat := rows[0].Record.Seat; seat == nil || *seat != "furiosa" {
		t.Errorf("Seat = %v, want the written seat, not the caller's later edit", seat)
	}
	if model := rows[0].Record.Models["opus"]; model.EstCostUSD != 0.75 {
		t.Errorf("Models[opus] = %+v, want the written cost, not the caller's later edit", model)
	}
	if rows[0].Sessions[0] != "sess-1" {
		t.Errorf("Sessions = %v, want the written ids, not the caller's later edit", rows[0].Sessions)
	}

	*rows[0].Record.Seat = "someone-else"
	rows[0].Record.Models["opus"] = usagerecord.Model{EstCostUSD: 99}
	rows[0].Sessions[0] = "sess-edited"
	again, err := f.ReadUsageRecords(ctx, time.Time{}, 0)
	if err != nil || len(again) != 1 {
		t.Fatalf("ReadUsageRecords after editing a returned row = %v, %v; want the row still stored", again, err)
	}
	if seat := again[0].Record.Seat; seat == nil || *seat != "furiosa" {
		t.Errorf("Seat after editing a returned row = %v, want the stored seat", seat)
	}
	if model := again[0].Record.Models["opus"]; model.EstCostUSD != 0.75 {
		t.Errorf("Models[opus] after editing a returned row = %+v, want the stored cost", model)
	}
	if again[0].Sessions[0] != "sess-1" {
		t.Errorf("Sessions after editing a returned row = %v, want the stored ids", again[0].Sessions)
	}
}

// TestFakeUsageRecordsReportScriptedFailures: both sample commands honor
// FailWith, so a caller test can script a store that did not answer.
func TestFakeUsageRecordsReportScriptedFailures(t *testing.T) {
	t.Parallel()
	f := New()
	ctx := context.Background()
	boom := errors.New("boom")
	f.FailWith("sample add", boom)
	row := everyFieldUsageRow()
	row.BuiltAt = sampleAt
	if err := f.WriteUsageRecord(ctx, row); !errors.Is(err, boom) {
		t.Errorf("WriteUsageRecord with FailWith(sample add) = %v, want boom", err)
	}
	f.FailWith("sample list", boom)
	if _, err := f.ReadUsageRecords(ctx, time.Time{}, 0); !errors.Is(err, boom) {
		t.Errorf("ReadUsageRecords with FailWith(sample list) = %v, want boom", err)
	}
}

// TestFakeUsageRecordsStoreTheBuiltAtAsBdDoes: as bd sample add writes it,
// BuiltAt is stored in UTC at second precision, and two rows at the same
// BuiltAt list the later write first.
func TestFakeUsageRecordsStoreTheBuiltAtAsBdDoes(t *testing.T) {
	t.Parallel()
	f := New()
	ctx := context.Background()
	at := time.Date(2026, 11, 15, 20, 0, 0, 500, time.UTC)
	for _, bead := range []string{"gt-first", "gt-second"} {
		row := everyFieldUsageRow()
		row.BuiltAt = at
		row.Record.Bead = bead
		mustDo(t, "WriteUsageRecord("+bead+")", f.WriteUsageRecord(ctx, row))
	}
	rows, err := f.ReadUsageRecords(ctx, time.Time{}, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ReadUsageRecords = %v, %v; want both rows", rows, err)
	}
	if rows[0].Record.Bead != "gt-second" || rows[1].Record.Bead != "gt-first" {
		t.Errorf("rows = %s then %s, want the later write first", rows[0].Record.Bead, rows[1].Record.Bead)
	}
	if rows[0].BuiltAt.Nanosecond() != 0 || rows[0].BuiltAt.Location() != time.UTC {
		t.Errorf("BuiltAt = %v, want UTC at second precision", rows[0].BuiltAt)
	}
}
