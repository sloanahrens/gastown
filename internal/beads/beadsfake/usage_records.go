package beadsfake

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/usagerecord"
)

// WriteUsageRecord appends row to the fake's usage records. As bd's sample add
// does, it refuses a zero BuiltAt, a record with no bead and a cost JSON cannot
// carry, and it stores BuiltAt in UTC at second precision and Start and End in
// UTC. The row is a copy, so changing row afterwards does not change what a
// read returns.
func (f *Fake) WriteUsageRecord(_ context.Context, row beads.UsageRecordRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("sample add"); err != nil {
		return err
	}
	if row.BuiltAt.IsZero() {
		return fmt.Errorf("sample add: no sample time given")
	}
	if row.Record.Bead == "" {
		return fmt.Errorf("sample add: record has no bead, so no reader can file the row")
	}
	if err := checkUsageCosts(row.Record); err != nil {
		return fmt.Errorf("sample add: %w", err)
	}
	row.BuiltAt = row.BuiltAt.UTC().Truncate(time.Second)
	row.Record.Start = row.Record.Start.UTC()
	row.Record.End = row.Record.End.UTC()
	f.usageRecords = append(f.usageRecords, cloneUsageRecordRow(row))
	return nil
}

// ReadUsageRecords returns the fake's usage records newest built first: a row
// with a later BuiltAt is listed before an earlier one, and rows at the same
// BuiltAt in reverse write order. A non-zero since keeps the rows at or after it
// and a positive limit caps the count; zero returns every row, where bd leaves
// its own default. A negative limit is an error, and the rows are copies.
func (f *Fake) ReadUsageRecords(_ context.Context, since time.Time, limit int) ([]beads.UsageRecordRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("sample list"); err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, fmt.Errorf("sample list: limit %d is negative", limit)
	}
	type row struct {
		row beads.UsageRecordRow
		seq int
	}
	rows := make([]row, 0, len(f.usageRecords))
	for i, r := range f.usageRecords {
		rows = append(rows, row{row: r, seq: i})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].row.BuiltAt.Equal(rows[j].row.BuiltAt) {
			return rows[i].row.BuiltAt.After(rows[j].row.BuiltAt)
		}
		return rows[i].seq > rows[j].seq
	})
	floor := since.UTC().Truncate(time.Second)
	var out []beads.UsageRecordRow
	for _, r := range rows {
		if !since.IsZero() && r.row.BuiltAt.Before(floor) {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, cloneUsageRecordRow(r.row))
	}
	return out, nil
}

// checkUsageCosts refuses a cost or share JSON cannot carry, named by its field,
// as *Beads does.
func checkUsageCosts(rec usagerecord.Record) error {
	if err := finite("est_cost_usd", rec.EstCostUSD); err != nil {
		return err
	}
	if err := finite("subagents.share", rec.Subagents.Share); err != nil {
		return err
	}
	for _, model := range slices.Sorted(maps.Keys(rec.Models)) {
		if err := finite("models."+model+".est_cost_usd", rec.Models[model].EstCostUSD); err != nil {
			return err
		}
	}
	return nil
}

// finite refuses a value JSON cannot carry.
func finite(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s %v is not a finite number", field, v)
	}
	return nil
}

// cloneUsageRecordRow copies row so a stored row and the row a caller holds
// never share memory: the models map, the seat pointer and the session slice are
// copied. A nil stays nil and an empty collection stays empty, as it does
// through the row's JSON.
func cloneUsageRecordRow(row beads.UsageRecordRow) beads.UsageRecordRow {
	out := row
	out.Record.Seat = clonePtr(row.Record.Seat)
	if row.Record.Models != nil {
		out.Record.Models = make(map[string]usagerecord.Model, len(row.Record.Models))
		for name, model := range row.Record.Models {
			out.Record.Models[name] = model
		}
	}
	if row.Sessions != nil {
		out.Sessions = append([]string{}, row.Sessions...)
	}
	return out
}
