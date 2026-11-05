package reaper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// The safe delete rule, used in code, tests and logs (gt-2i850): for one
// database, bound is the earlier of the retention cutoff (Now-Retain) and the
// start of the events archive's lookback window (the watermark's CreatedAt
// minus eventsArchiveLookback). A pass deletes only rows whose created_at is
// STRICTLY OLDER than bound; a row exactly at bound is kept. Because bound is
// never later than the window start, a row that sits before CreatedAt but
// inside the window is kept whether or not the archive has written it yet. The
// Archived id set is not consulted for the delete: it covers only the window,
// which the bound already keeps, and cannot cover a row older than the window.
// Since the window start is truncated to whole seconds, a sub-second timestamp
// only lowers the bound, so it can only keep more.

// EventsPurgeOptions bounds one events retention pass.
type EventsPurgeOptions struct {
	// Now is the clock the cutoff is computed from, so a test injects it.
	Now time.Time
	// Retain is how long an events row is kept; rows created before
	// Now-Retain are eligible. It must be positive.
	Retain time.Duration
	// Database names the database this pass purges, and StatePath is
	// daemon/maintenance_state.json, where the events archive records its
	// watermark for that database. A pass deletes only rows the archive has
	// already covered (gt-2i850).
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
	// Cutoff is the single created_at boundary the pass used: the earlier of
	// the retention cutoff and the archive's window start.
	Cutoff time.Time
	DryRun bool
	// Skipped is set when the database has no events table, or when it is
	// waiting for the events archive (Waiting).
	Skipped bool
	// Waiting is why a skipped pass deleted nothing: the events archive has no
	// watermark for the database yet, so archiving is off or has not started.
	// A state file that cannot be read, parsed or trusted is not waiting; it
	// is refused as an error.
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
// earlier of Now-Retain and the start of the events archive's lookback window
// for opts.Database, so no row the archive has not covered is deleted
// (gt-2i850). A database with no watermark yet deletes nothing and is reported
// as waiting for the archive, which counts as skipped; a state file or
// watermark that cannot be trusted is refused as an error, which counts as
// failed.
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

	windowStart, waiting, err := readEventsArchiveWatermark(opts.StatePath, opts.Database)
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
	if windowStart.Before(cutoff) {
		cutoff = windowStart
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

// eventsArchiveLookback mirrors internal/daemon's eventsArchiveLookback: how
// far before its watermark the events archive re-reads on the next run.
// internal/reaper cannot import internal/daemon (the daemon imports this
// package), so the value is repeated here. The daemon test
// TestEventsRetentionWindowStartMatchesTheArchiveSince pins this copy to
// EventsArchiveWatermark.Since, so changing the window in either place turns a
// test red.
const eventsArchiveLookback = 24 * time.Hour

// eventsArchiveWatermark is the part of daemon.EventsArchiveWatermark the
// retention pass reads: the newest created_at the archive wrote out, and the
// rows it wrote inside the lookback window.
type eventsArchiveWatermark struct {
	CreatedAt string              `json:"created_at,omitempty"`
	Archived  []archivedEventsRow `json:"archived,omitempty"`
}

// archivedEventsRow is one events row the archive wrote, as the state file
// records it.
type archivedEventsRow struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

// validate mirrors daemon.EventsArchiveWatermark.Validate, which this package
// cannot import: a created_at is usable only with the ids written in its
// window, every archived row must carry a timestamp and an id, and an id
// without a created_at cannot be placed in the window at all. The zero
// watermark (nothing archived yet) is valid here; the caller reports it as
// waiting rather than bounding a read with it.
func (w eventsArchiveWatermark) validate(database string) error {
	if w.CreatedAt == "" {
		if len(w.Archived) > 0 {
			return fmt.Errorf("reaper: events archive watermark for %s lists %d id(s) but no created_at; refusing to read past it", database, len(w.Archived))
		}
		return nil
	}
	if !beadsql.TimestampLiteral(w.CreatedAt) {
		return fmt.Errorf("reaper: events archive watermark for %s has created_at %q, which is not a timestamp", database, w.CreatedAt)
	}
	if len(w.Archived) == 0 {
		return fmt.Errorf("reaper: events archive watermark for %s lists no ids; refusing to read past it", database)
	}
	for _, row := range w.Archived {
		if !beadsql.TimestampLiteral(row.CreatedAt) {
			return fmt.Errorf("reaper: events archive watermark for %s carries an id with created_at %q, which is not a timestamp", database, row.CreatedAt)
		}
		if row.ID == "" {
			return fmt.Errorf("reaper: events archive watermark for %s carries an empty id; refusing to read past it", database)
		}
	}
	return nil
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

// supportedMaintenanceStateVersion is the only version of
// daemon/maintenance_state.json this reader knows. The daemon writes no
// version field today, so its absence (the zero value) is the one supported
// version; a file naming another is refused rather than read with guessed
// semantics.
const supportedMaintenanceStateVersion = 0

// EventsArchiveWindowStart returns the first created_at the events archive's
// next read looks at: the watermark's CreatedAt minus eventsArchiveLookback,
// truncated to whole seconds exactly as EventsArchiveWatermark.Since does. A
// pass bounds its delete by it, so a row inside the window is never deleted
// before the archive has re-read that window.
func EventsArchiveWindowStart(createdAt string) (time.Time, error) {
	at, err := parseEventsWatermarkTimestamp(createdAt)
	if err != nil {
		return time.Time{}, err
	}
	return at.Add(-eventsArchiveLookback).UTC().Truncate(time.Second), nil
}

// readEventsArchiveWatermark returns the start of database's events archive
// lookback window in the daemon's maintenance state file. It is a minimal
// read-only decoder: the daemon's state type cannot be imported here, since the
// daemon imports this package.
//
// The state file's absence, a database with no watermark, and a zero watermark
// all return a waiting reason and no error: the pass deletes nothing, and the
// caller counts it as skipped. A state file that exists but cannot be read,
// parsed or trusted (an unsupported version, a watermark that fails Validate,
// a created_at that does not parse) is an error, so a corrupt record is
// refused rather than deleted past or read as no watermark.
func readEventsArchiveWatermark(statePath, database string) (time.Time, string, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, "the maintenance state file does not exist", nil
		}
		return time.Time{}, "", fmt.Errorf("reaper: cannot read the maintenance state file for %s: %w", database, err)
	}
	var state struct {
		Version                 int                               `json:"version"`
		ArchiveEventsWatermarks map[string]eventsArchiveWatermark `json:"archive_events_watermarks"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return time.Time{}, "", fmt.Errorf("reaper: the maintenance state file for %s cannot be parsed: %w", database, err)
	}
	if state.Version != supportedMaintenanceStateVersion {
		return time.Time{}, "", fmt.Errorf("reaper: the maintenance state file for %s carries version %d, which this reader does not support", database, state.Version)
	}
	entry, ok := state.ArchiveEventsWatermarks[database]
	if !ok {
		return time.Time{}, fmt.Sprintf("no events archive watermark recorded for %s", database), nil
	}
	if entry.CreatedAt == "" && len(entry.Archived) == 0 {
		return time.Time{}, fmt.Sprintf("the events archive has not written a row for %s yet", database), nil
	}
	if err := entry.validate(database); err != nil {
		return time.Time{}, "", err
	}
	windowStart, err := EventsArchiveWindowStart(entry.CreatedAt)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("reaper: events archive watermark for %s cannot bound a read: %w", database, err)
	}
	return windowStart, "", nil
}

// parseEventsWatermarkTimestamp parses a watermark timestamp in the forms Dolt
// emits for a datetime column.
func parseEventsWatermarkTimestamp(s string) (time.Time, error) {
	for _, layout := range eventsWatermarkLayouts {
		if at, err := time.Parse(layout, s); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("created_at %q is not a timestamp", s)
}
