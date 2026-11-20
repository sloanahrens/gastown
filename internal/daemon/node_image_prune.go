package daemon

import (
	"context"
	"errors"
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
// ok row of job nodeImagePruneJob with the summary's figures and now as its start;
// an applied run logs its summary line and records its row the same way. The failed
// run's row is bead gt-eg0ji0. Every runner error is handed to the caller.
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
	if apply {
		return d.nodeImagePruneApply(rep, err, now)
	}
	if err != nil {
		return err
	}
	lines, stats := nodeImagePruneSummary(rep, false)
	for _, line := range lines {
		d.logger.Printf("%s", line)
	}
	d.nodeImagePruneRecord(jobledger.Run{
		Job:       nodeImagePruneJob,
		StartedAt: now,
		EndedAt:   d.clk().Now(),
		Outcome:   nodeImagePruneOutcomeOK,
		Stats:     stats,
	})
	return nil
}

// nodeImagePruneApply logs an applied run's summary line and records its ledger row,
// both from the report the runner returned, and returns the runner's error. A run
// that failed before its log was read has no figures to record and returns before
// either; the runner marks the one error that comes after a parsed log, so a
// zero-removal apply whose Job delete failed still records the report it returned.
func (d *Daemon) nodeImagePruneApply(rep nodePruneReport, runErr error, started time.Time) error {
	var logRead *nodePruneLogReadError
	if runErr != nil && !errors.As(runErr, &logRead) {
		return runErr
	}
	lines, stats := nodeImagePruneSummary(rep, true)
	for _, line := range lines {
		d.logger.Printf("%s", line)
	}
	if runErr != nil {
		d.logger.Printf("node_image_prune: %v", runErr)
	}
	d.nodeImagePruneRecord(jobledger.Run{
		Job:       nodeImagePruneJob,
		StartedAt: started,
		EndedAt:   d.clk().Now(),
		Outcome:   nodeImagePruneOutcomeOK,
		Stats:     stats,
	})
	return runErr
}

// nodeImagePruneRecord appends one run to the town's job ledger. A ledger that
// cannot record it is logged once: the ledger records the run and does not decide
// it, so the run's result is unchanged.
func (d *Daemon) nodeImagePruneRecord(run jobledger.Run) {
	if err := d.appendJobRun(run); err != nil {
		d.logger.Printf("node_image_prune: the job ledger could not record the run: %v", err)
	}
}

// nodePruneLogReadError is the runner's error for a run whose log was read and
// parsed but whose Job could not be deleted. The report returned beside it is the
// log's, so an apply records it instead of dropping it as a run that failed before
// the log, whose report is the zero one.
type nodePruneLogReadError struct{ err error }

func (e *nodePruneLogReadError) Error() string { return e.err.Error() }
func (e *nodePruneLogReadError) Unwrap() error { return e.err }
