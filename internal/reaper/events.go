package reaper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	// Database names the database this pass purges, and StatePath is
	// daemon/maintenance_state.json, where the events archive records its
	// watermark for that database. A pass deletes only rows created before the
	// watermark (gt-2i850).
	Database  string
	StatePath string
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
	// Skipped is set when the database has no events table, or when it is
	// waiting for the events archive (Waiting).
	Skipped bool
	// Waiting is why a skipped pass deleted nothing because the events archive
	// has no watermark for the database: archiving is off, or the state file
	// is unreadable.
	Waiting string
}

// Summary is the one-line report of a pass: rows, batches, cutoff and whether
// it wrote.
func (r EventsPurgeResult) Summary() string {
	switch {
	case r.Waiting != "":
		return "skipped: waiting for the events archive watermark (" + r.Waiting + ")"
	case r.Skipped:
		return "skipped: no events table"
	case r.DryRun:
		return fmt.Sprintf("would delete %d rows older than %s (dry run, %d batches run)", r.Rows, r.Cutoff.Format(time.RFC3339), r.Batches)
	default:
		return fmt.Sprintf("deleted %d rows in %d batches older than %s", r.Rows, r.Batches, r.Cutoff.Format(time.RFC3339))
	}
}

// PurgeEvents deletes the events rows created before the cutoff in counted
// batches and reports the rows and batches the pass used. The cutoff is the
// older of Now-Retain and the events archive watermark for opts.Database, so no
// row newer than the archive's watermark is deleted (gt-2i850). A database with
// no watermark, or an unreadable state file, deletes nothing and is reported as
// waiting for the archive, which counts as skipped.
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
	if opts.Database == "" || opts.StatePath == "" {
		return EventsPurgeResult{}, fmt.Errorf("reaper: events retention needs the database and the maintenance state path")
	}

	exists, err := tableExists(ctx, db, "events")
	if err != nil {
		return EventsPurgeResult{}, fmt.Errorf("check events table: %w", err)
	}
	if !exists {
		return EventsPurgeResult{Skipped: true}, nil
	}

	watermark, waiting, err := readEventsArchiveWatermark(opts.StatePath, opts.Database)
	if err != nil {
		return EventsPurgeResult{}, err
	}
	if waiting != "" {
		return EventsPurgeResult{Skipped: true, Waiting: waiting, DryRun: opts.DryRun}, nil
	}

	// The cutoff is computed once so every pass of the run asks the same
	// question; recomputing it per pass would let a long run keep deleting
	// rows it has just made old enough.
	cutoff := opts.Now.Add(-opts.Retain)
	if watermark.Before(cutoff) {
		cutoff = watermark
	}
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

// eventsWatermarkLayouts are the forms a watermark's created_at takes: the
// Dolt datetime forms beadsql.TimestampLiteral accepts. A form with no zone is
// read as UTC, the zone the retention cutoff is compared in.
var eventsWatermarkLayouts = []string{
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
}

// readEventsArchiveWatermark returns the created_at of database's events
// archive watermark in the daemon's maintenance state file. It is a minimal
// read-only decoder: the daemon's state type cannot be imported here, since the
// daemon imports this package. The file has no version field to check.
//
// A missing or unreadable state file, a database with no watermark, and a
// watermark that has not written a row yet all return a waiting reason and no
// error: the pass deletes nothing, and the caller counts it as skipped. A
// watermark whose created_at is not a timestamp is an error, so a corrupt
// record is refused rather than read as no watermark.
func readEventsArchiveWatermark(statePath, database string) (time.Time, string, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return time.Time{}, "maintenance state file is unreadable", nil
	}
	var state struct {
		ArchiveEventsWatermarks map[string]struct {
			CreatedAt string `json:"created_at"`
		} `json:"archive_events_watermarks"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return time.Time{}, "maintenance state file is unreadable", nil
	}
	entry, ok := state.ArchiveEventsWatermarks[database]
	if !ok || entry.CreatedAt == "" {
		return time.Time{}, fmt.Sprintf("no events archive watermark recorded for %s", database), nil
	}
	if !beadsql.TimestampLiteral(entry.CreatedAt) {
		return time.Time{}, "", fmt.Errorf("reaper: events archive watermark for %s is not a timestamp", database)
	}
	for _, layout := range eventsWatermarkLayouts {
		if at, err := time.Parse(layout, entry.CreatedAt); err == nil {
			return at.UTC(), "", nil
		}
	}
	return time.Time{}, "", fmt.Errorf("reaper: events archive watermark for %s is not a timestamp", database)
}
