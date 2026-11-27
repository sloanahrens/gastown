package rehearsal

import (
	"context"
	"fmt"
)

// closeReason is the reason Drive gives the run's beads when it closes them.
const closeReason = "rehearsal run ended"

// Drive runs one rehearsal. It sets the record's identity from o and Start, files
// o.Tasks, runs the load window, sets End, collects the record from the town, closes
// the run's beads and saves the record to o.Path. The first error from any step
// returns wrapped with the step's name and stops the run, so nothing is saved. The
// record returned with an error holds whatever the steps gathered up to that point.
func Drive(ctx context.Context, o Options) (Record, error) {
	rec := Record{RunID: o.RunID, Tag: o.Tag, ChangeSet: o.ChangeSet, Start: o.Now()}
	ids, err := FileSet(ctx, o.Town, o.Tasks)
	if err != nil {
		return rec, fmt.Errorf("drive: file set: %w", err)
	}
	slings, runs, err := runWindow(ctx, o, ids)
	rec.Slings, rec.SideRuns = slings, runs
	if err != nil {
		return rec, fmt.Errorf("drive: run window: %w", err)
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
