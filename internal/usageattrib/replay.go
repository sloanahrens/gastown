package usageattrib

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// ReplayStore is the usage_records store a replay reads existing rows from, checks new ones against and writes them to.
type ReplayStore interface {
	ReadUsageRecords(ctx context.Context, since time.Time, limit int) ([]beads.UsageRecordRow, error)
	// CheckUsageRecordRow refuses what WriteUsageRecord refuses before it
	// writes, and writes nothing: a dry run counts a row it accepts as written.
	CheckUsageRecordRow(ctx context.Context, row beads.UsageRecordRow) error
	WriteUsageRecord(ctx context.Context, row beads.UsageRecordRow) error
}

// replayRowsLimit caps the one read of the existing rows. A zero limit is bd's
// own default, not every row, so the read names a cap; a read that fills it may
// have left rows behind, and a duplicate check that cannot see every row would
// write a record twice, so the pass refuses instead. The dashboard's rows read
// names the same cap.
const replayRowsLimit = 20000

// ReplayResult counts one pass; on a dry run Written is the rows an apply would write.
type ReplayResult struct {
	Files, Written, Skipped int
	Failed                  []error // the record files not written, with their first error
}

// Replay writes a usage_records row for each record file under usageDir whose bead and kind has none, and none for the rest: bd sample add has no retry, so a second row would store the record twice.
// The existing rows are read once from store, the store the new rows go to, so the check never sees another store's rows.
// A file that cannot be read or filed is counted failed and the pass goes on; a rows read that fails, or that fills its cap, returns before any write.
// dryRun puts each row through store's own precheck and writes nothing, so its counts are an apply's.
func Replay(ctx context.Context, usageDir string, store ReplayStore, dryRun bool) (ReplayResult, error) {
	paths, err := RecordFilePaths(usageDir)
	if err != nil {
		return ReplayResult{}, err
	}
	res := ReplayResult{Files: len(paths)}
	rows, err := store.ReadUsageRecords(ctx, time.Time{}, replayRowsLimit)
	if err != nil {
		return res, fmt.Errorf("usageattrib: reading the usage rows: %w", err)
	}
	if len(rows) >= replayRowsLimit {
		return res, fmt.Errorf("usageattrib: the usage rows read filled its cap of %d, so the existing rows cannot be taken whole; refusing to write", replayRowsLimit)
	}
	existing := make(map[[2]string]bool, len(rows))
	for _, row := range rows {
		existing[[2]string{row.Record.Bead, row.Record.Kind}] = true
	}
	for _, path := range paths {
		st, err := ReadRecordFile(path)
		if err != nil {
			res.Failed = append(res.Failed, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if existing[[2]string{st.Record.Bead, st.Record.Kind}] {
			res.Skipped++
			continue
		}
		row := beads.UsageRecordRow{BuiltAt: st.Record.End, Record: st.Record, Sessions: st.Sessions}
		err = checkRecord(st.Record)
		switch {
		case err == nil && dryRun:
			err = store.CheckUsageRecordRow(ctx, row)
		case err == nil:
			err = store.WriteUsageRecord(ctx, row)
		}
		if err != nil {
			res.Failed = append(res.Failed, fmt.Errorf("%s: %w", path, err))
			continue
		}
		res.Written++
	}
	return res, nil
}
