package daemon

import (
	"context"
	"time"
)

// nodeImagePruneKeep is how many images of each repository a node prune keeps: the
// newest three, so the node can still roll back two releases.
const nodeImagePruneKeep = 3

// nodeImagePruneOnce runs one node image prune: the Job nodePruneJobName names from
// runID, running the running polecat image, as a dry run unless apply. It records
// nothing itself -- the dry run's log lines and the ledger rows are the beads that own
// them -- so every error is handed to the caller. now is the run's start time, which
// those rows record.
func (d *Daemon) nodeImagePruneOnce(ctx context.Context, kube nodePruneKube, runID string, apply bool, now time.Time) error { //nolint:unparam // now is the run's start time, which the ledger row beads gt-08qnmf and gt-i8zplo record
	name, err := nodePruneJobName(runID)
	if err != nil {
		return err
	}
	image, err := d.nodePruneImage()
	if err != nil {
		return err
	}
	_, err = runNodeImagePrune(ctx, kube, name, image, nodeImagePruneKeep, apply)
	return err
}
