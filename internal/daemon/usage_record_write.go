package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// errNoUsageStore is the refusal when no usage_records store is wired: the
// record file is written and the row is not, and the caller's error names the
// row as the missing half.
var errNoUsageStore = errors.New("no usage_records store is wired")

// usageRecordStore is the town's usage_records rows: the one method the usage
// writers need. *beads.Beads and beadsfake.Fake both implement it.
type usageRecordStore interface {
	WriteUsageRecord(ctx context.Context, row beads.UsageRecordRow) error
}

// usageRowNotWrittenError is the error of a record whose file was written and
// whose usage_records row was not. The record still reads, so the caller reports
// the path it wrote beside this error rather than calling the record missing.
type usageRowNotWrittenError struct {
	err error
}

func (e *usageRowNotWrittenError) Error() string { return e.err.Error() }

func (e *usageRowNotWrittenError) Unwrap() error { return e.err }

// writeUsageRecord files one run's usage in one call: its record file under
// usageDir, which the dashboard's readers read until a later bead removes the
// file path, and its usage_records row in store, which outlives the file. The
// file is written first: it refuses a record no reader could file (no bead, no
// workspace, a kind neither work nor om), and a record it refuses must not
// reach the rows. An error is returned for each half that failed, so a file
// written without its row is never reported as filed whole; that case's error
// is a *usageRowNotWrittenError, so the caller can report the path it wrote.
//
// The row carries the record the file holds, which is the bead's merged record
// and not this run's alone: the dashboard drops the file of a bead and kind a
// row holds, so a row of this run alone would lose every earlier run as soon as
// one run's row was not written. The row's sample time is the run's end: the
// record is built when the run returns, so bd's newest-built order is the run
// order.
func writeUsageRecord(ctx context.Context, store usageRecordStore, usageDir string, rec dashboard.UsageRecord, sessionIDs []string) error {
	stored, fileErr := usageattrib.Merge(usageDir, rec, sessionIDs)
	if fileErr != nil {
		return fmt.Errorf("its record file is not written: %w", fileErr)
	}
	if store == nil {
		return &usageRowNotWrittenError{fmt.Errorf("its usage_records row is not written: %w", errNoUsageStore)}
	}
	row := beads.UsageRecordRow{BuiltAt: rec.End, Record: stored.Record, Sessions: stored.Sessions}
	if rowErr := store.WriteUsageRecord(ctx, row); rowErr != nil {
		return &usageRowNotWrittenError{fmt.Errorf("its usage_records row is not written: %w", rowErr)}
	}
	return nil
}

// usageRecords is the town's usage_records store: the rows live in the town's
// own database, beside the cost samples, which is the database the dashboard
// reads back from the town root. It has no default: a daemon whose store was not
// wired opens none, so a writer reports the row not written rather than reaching
// a database no caller named. New wires the town's store.
func (d *Daemon) usageRecords() usageRecordStore {
	if d.openUsageRecords == nil {
		return nil
	}
	return d.openUsageRecords()
}
