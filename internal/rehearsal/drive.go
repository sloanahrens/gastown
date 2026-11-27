package rehearsal

import (
	"context"
	"errors"
	"fmt"
)

// closeReason is the reason Drive gives the run's beads when it closes them.
const closeReason = "rehearsal run ended"

// Drive runs one rehearsal. It sets the record's identity from o and Start, files
// o.Tasks, runs the load window, sets End, collects the record from the town, closes
// the run's beads and saves the record to o.Path. A failed file set, collect, close or
// save returns its error wrapped with the step's name and saves nothing. A failed run
// window, a canceled ctx included, stops the run: Drive marks the record Stopped with
// the step's name, sets End, collects the record from the town, closes the run's beads,
// saves the record and returns the window error joined with any error from the collect,
// the close or the save. The record returned with an error holds whatever the steps
// gathered up to that point.
func Drive(ctx context.Context, o Options) (Record, error) {
	rec := Record{RunID: o.RunID, Tag: o.Tag, ChangeSet: o.ChangeSet, Start: o.Now()}
	ids, err := FileSet(ctx, o.Town, o.Tasks)
	if err != nil {
		return rec, fmt.Errorf("drive: file set: %w", err)
	}
	slings, runs, err := runWindow(ctx, o, ids)
	rec.Slings, rec.SideRuns = slings, runs
	if err != nil {
		return finish(ctx, o, rec, ids, fmt.Errorf("drive: run window: %w", err))
	}
	rec.End = o.Now()
	if err := Collect(ctx, o.Town, &rec, ids, o.HostLoad); err != nil {
		return rec, fmt.Errorf("drive: collect: %w", err)
	}
	if err := CloseOpen(ctx, o.Town, ids, closeReason); err != nil {
		return rec, fmt.Errorf("drive: close beads: %w", err)
	}
	if err := Save(o.Path, rec); err != nil {
		return rec, fmt.Errorf("drive: save record: %w", err)
	}
	return rec, nil
}

// finish ends a run whose window failed with err. It marks rec Stopped with the
// failed step's name, sets End if it is unset, then collects rec from the town, closes
// the run's beads and saves rec. The collect, the close and the save run under
// context.WithoutCancel(ctx), so a canceled run still collects, closes and saves. It
// returns rec with err joined with any error from the collect, the close or the save.
func finish(ctx context.Context, o Options, rec Record, ids []string, err error) (Record, error) {
	rec.Stopped, rec.StopReason = true, "run window failed"
	if rec.End.IsZero() {
		rec.End = o.Now()
	}
	cleanup := context.WithoutCancel(ctx)
	errs := []error{err}
	if cerr := Collect(cleanup, o.Town, &rec, ids, o.HostLoad); cerr != nil {
		errs = append(errs, fmt.Errorf("drive: collect: %w", cerr))
	}
	if cerr := CloseOpen(cleanup, o.Town, ids, closeReason); cerr != nil {
		errs = append(errs, fmt.Errorf("drive: close beads: %w", cerr))
	}
	if serr := Save(o.Path, rec); serr != nil {
		errs = append(errs, fmt.Errorf("drive: save record: %w", serr))
	}
	return rec, errors.Join(errs...)
}
