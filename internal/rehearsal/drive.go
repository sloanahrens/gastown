package rehearsal

import (
	"context"
	"errors"
	"fmt"
)

// closeReason is the reason Drive gives the run's beads when it closes them.
const closeReason = "rehearsal run ended"

// driveStep names a Drive step; finish runs only the steps after the failed one.
type driveStep int

const (
	stepFileSet driveStep = iota
	stepRunWindow
	stepCollect
	stepCloseBeads
	stepSaveRecord
)

// stepName is what StopReason and the returned error call each driveStep.
var stepName = [...]string{"file set", "run window", "collect", "close beads", "save record"}

// Drive runs one rehearsal. It sets the record's identity from o and Start, files
// o.Tasks, runs the load window, sets End, collects the record from the town, closes
// the run's beads and saves the record to o.Path. A failed file set, run window,
// collect, close or save stops the run: Drive marks the record Stopped with the step's
// name, sets End if it is unset, runs the steps after the failed one and returns the
// error wrapped with that name, joined with any error from those steps. The record
// returned with an error holds whatever the steps gathered up to that point.
func Drive(ctx context.Context, o Options) (Record, error) {
	rec := Record{RunID: o.RunID, Tag: o.Tag, ChangeSet: o.ChangeSet, Start: o.Now()}
	ids, err := FileSet(ctx, o.Town, o.Tasks)
	if err != nil {
		return finish(ctx, o, rec, ids, stepFileSet, err)
	}
	slings, runs, err := runWindow(ctx, o, ids)
	rec.Slings, rec.SideRuns = slings, runs
	if err != nil {
		return finish(ctx, o, rec, ids, stepRunWindow, err)
	}
	rec.End = o.Now()
	if err := Collect(ctx, o.Town, &rec, ids, o.HostLoad); err != nil {
		return finish(ctx, o, rec, ids, stepCollect, err)
	}
	if err := CloseOpen(ctx, o.Town, ids, closeReason); err != nil {
		return finish(ctx, o, rec, ids, stepCloseBeads, err)
	}
	if err := Save(o.Path, rec); err != nil {
		return finish(ctx, o, rec, ids, stepSaveRecord, err)
	}
	return rec, nil
}

// finish ends a run whose step failed with err. It marks rec Stopped with the step's
// name, sets End if it is unset, then runs only the steps after the failed one: the
// collect for a failed file set, run window or collect, the close for any of those and
// the save for every step but itself, all under context.WithoutCancel(ctx). It returns
// rec with err wrapped with the step's name, joined with any error from those steps.
func finish(ctx context.Context, o Options, rec Record, ids []string, step driveStep, err error) (Record, error) {
	rec.Stopped, rec.StopReason = true, stepName[step]+" failed"
	if rec.End.IsZero() {
		rec.End = o.Now()
	}
	cleanup := context.WithoutCancel(ctx)
	errs := []error{fmt.Errorf("drive: %s: %w", stepName[step], err)}
	if step < stepCollect {
		if cerr := Collect(cleanup, o.Town, &rec, ids, o.HostLoad); cerr != nil {
			errs = append(errs, fmt.Errorf("drive: collect: %w", cerr))
		}
	}
	if step < stepCloseBeads {
		if cerr := CloseOpen(cleanup, o.Town, ids, closeReason); cerr != nil {
			errs = append(errs, fmt.Errorf("drive: close beads: %w", cerr))
		}
	}
	if step < stepSaveRecord {
		if serr := Save(o.Path, rec); serr != nil {
			errs = append(errs, fmt.Errorf("drive: save record: %w", serr))
		}
	}
	return rec, errors.Join(errs...)
}
