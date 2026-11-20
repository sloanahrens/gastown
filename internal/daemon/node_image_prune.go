package daemon

import (
	"context"
	"time"

	"github.com/steveyegge/gastown/internal/jobledger"
)

// nodeImagePruneKeep is how many images of each repository a node prune keeps: the
// newest three, so the node can still roll back two releases.
const nodeImagePruneKeep = 3

// The job every node prune run records under in the town's job ledger, and the
// outcome of a run that reached its report.
const (
	nodeImagePruneJob       = "node_image_prune"
	nodeImagePruneOutcomeOK = "ok"
)

// nodeImagePruneOnce runs one node image prune: the Job nodePruneJobName names from
// runID, running the running polecat image, as a dry run unless apply. A dry run
// logs the lines of nodeImagePruneSummary, one logger call per line, and records one
// ok row of job nodeImagePruneJob with the summary's figures and now as its start.
// An apply run's row and a failed run's are beads gt-i8zplo and gt-eg0ji0. Every
// runner error is handed to the caller.
func (d *Daemon) nodeImagePruneOnce(ctx context.Context, kube nodePruneKube, runID string, apply bool, now time.Time) error {
	name, err := nodePruneJobName(runID)
	if err != nil {
		return err
	}
	image, err := d.nodePruneImage()
	if err != nil {
		return err
	}
	rep, err := runNodeImagePrune(ctx, kube, name, image, nodeImagePruneKeep, apply)
	if err != nil {
		return err
	}
	if apply {
		return nil
	}
	lines, stats := nodeImagePruneSummary(rep, false)
	for _, line := range lines {
		d.logger.Printf("%s", line)
	}
	return d.nodeImagePruneRecord(jobledger.Run{
		Job:       nodeImagePruneJob,
		StartedAt: now,
		EndedAt:   d.clk().Now(),
		Outcome:   nodeImagePruneOutcomeOK,
		Stats:     stats,
	})
}

// nodeImagePruneRecord appends one run to the town's job ledger. A ledger that
// cannot record it is logged once and returns nil: the ledger records the run and
// does not decide it, so the run's result is unchanged.
func (d *Daemon) nodeImagePruneRecord(run jobledger.Run) error { //nolint:unparam // a failed append is logged, not returned: the ledger records the run and does not decide it
	if err := d.appendJobRun(run); err != nil {
		d.logger.Printf("node_image_prune: the job ledger could not record the run: %v", err)
	}
	return nil
}
