package reaper

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// EventsPurgeOptions bounds one events retention pass.
type EventsPurgeOptions struct {
	// Now is the clock the cutoff is computed from, so a test injects it.
	Now time.Time
	// Retain is how long an events row is kept; rows created before
	// Now-Retain are eligible. It must be positive.
	Retain time.Duration
	// Batch is the most rows one DELETE removes, and MaxBatches is the most
	// DELETEs a pass runs, so one pass is bounded work the patrol can finish
	// and a backlog drains over several cycles.
	Batch      int
	MaxBatches int
	// DryRun counts the eligible rows and deletes nothing.
	DryRun bool
}

// EventsPurgeResult reports one events retention pass.
type EventsPurgeResult struct {
	// Rows is how many rows were deleted, or with DryRun how many are older
	// than Cutoff.
	Rows int64
	// Batches is how many DELETEs ran.
	Batches int
	// Cutoff is the single created_at boundary the pass used.
	Cutoff time.Time
	DryRun bool
	// Skipped is set when the database has no events table.
	Skipped bool
}

// Summary is the one-line report of a pass: rows, batches, cutoff and whether
// it wrote.
func (r EventsPurgeResult) Summary() string {
	switch {
	case r.Skipped:
		return "skipped: no events table"
	case r.DryRun:
		return fmt.Sprintf("would delete %d rows older than %s (dry run, %d batches run)", r.Rows, r.Cutoff.Format(time.RFC3339), r.Batches)
	default:
		return fmt.Sprintf("deleted %d rows in %d batches older than %s", r.Rows, r.Batches, r.Cutoff.Format(time.RFC3339))
	}
}

// PurgeEvents deletes the events rows created before Now-Retain in counted
// batches and reports the rows and batches the pass used.
//
// It is the reaper's one write to a bd table with SQL, and the one exception
// to the rule in writer.go that writes go through bd: bd has no verb that
// deletes audit rows, events is dolt_ignored so the DELETE creates no Dolt
// commit, and nothing is derived from an old row (gt-hk21d). No other table is
// touched — not wisp_events, bd_events_journal or provenance_events — and a
// database without an events table is skipped without error.
func PurgeEvents(ctx context.Context, db *beadsql.DB, opts EventsPurgeOptions) (EventsPurgeResult, error) {
	if opts.Now.IsZero() {
		return EventsPurgeResult{}, fmt.Errorf("reaper: events retention needs a clock")
	}
	if opts.Retain <= 0 || opts.Batch <= 0 || opts.MaxBatches <= 0 {
		return EventsPurgeResult{}, fmt.Errorf(
			"reaper: events retention needs a positive retain, batch and maxBatches (got retain=%s batch=%d maxBatches=%d)",
			opts.Retain, opts.Batch, opts.MaxBatches)
	}

	exists, err := tableExists(ctx, db, "events")
	if err != nil {
		return EventsPurgeResult{}, fmt.Errorf("check events table: %w", err)
	}
	if !exists {
		return EventsPurgeResult{Skipped: true}, nil
	}

	// The cutoff is computed once so every pass of the run asks the same
	// question; recomputing it per pass would let a long run keep deleting
	// rows it has just made old enough.
	cutoff := opts.Now.Add(-opts.Retain)
	result := EventsPurgeResult{Cutoff: cutoff, DryRun: opts.DryRun}

	if opts.DryRun {
		rows, err := db.CountEventsBefore(ctx, cutoff)
		if err != nil {
			return result, err
		}
		result.Rows = rows
		return result, nil
	}

	for result.Batches < opts.MaxBatches {
		rows, err := db.DeleteEventsBefore(ctx, cutoff, opts.Batch)
		if err != nil {
			return result, err
		}
		result.Batches++
		result.Rows += rows
		if rows < int64(opts.Batch) {
			break
		}
	}
	return result, nil
}
