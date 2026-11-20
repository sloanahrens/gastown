package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jobledger"
)

// nodeImagePruneKeep is how many images of each repository a node prune keeps: the
// newest three, so the node can still roll back two releases.
const nodeImagePruneKeep = 3

// The job every node prune run records under in the town's job ledger, and the
// outcome of a run that reached its report or failed before it.
const (
	nodeImagePruneJob           = "node_image_prune"
	nodeImagePruneOutcomeOK     = "ok"
	nodeImagePruneOutcomeFailed = "failed"
)

// nodeImagePruneApplyStat is the ledger stat every node prune row carries the mode
// of: 1 for an apply run and 0 for a dry run. Without it a dry run that planned no
// images and an apply that removed none are the same row (bead gt-1752lv). The
// value is a plain literal because the ledger's schema is this package's alone; the
// daemon's kubectl-apply guard reads every "apply" literal as a verb, and this key's
// reviewed entry in cacheTrimSmallApplies says it is not one.
const nodeImagePruneApplyStat = "apply"

// nodeImagePruneStepPrefix opens the message of every runNodeImagePrune error.
const nodeImagePruneStepPrefix = "node-prune: "

// nodeImagePruneOnce runs one node image prune: the Job nodePruneJobName names from
// runID, running the running polecat image, as a dry run unless apply. A dry run
// logs the lines of nodeImagePruneSummary, one logger call per line, and records one
// ok row of job nodeImagePruneJob with the summary's figures, the mode it ran in and
// now as its start;
// an applied run logs its summary line and records its row the same way. A run that
// returns an error before its log gave a report records the failed row of
// nodeImagePruneFailed instead. Every runner error is handed to the caller.
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
		var logRead *nodePruneLogReadError
		if !errors.As(err, &logRead) {
			d.nodeImagePruneFailed(err, apply, now)
			return err
		}
	}
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
	}, false)
	return nil
}

// nodeImagePruneApply logs an applied run's summary line and records its ledger row,
// both from the report the runner returned, and returns the runner's error. The caller
// hands it a run that reached its log, with no error or with the nodePruneLogReadError
// a failed Job delete leaves, so a zero-removal apply still records the zeros its log
// gave (bead gt-i8zplo).
func (d *Daemon) nodeImagePruneApply(rep nodePruneReport, runErr error, started time.Time) error {
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
	}, true)
	return runErr
}

// nodeImagePruneFailed logs a run that returned an error before its log gave a
// report and records its failed row: no figures, because the run has none, and the
// mode it ran in alone. The log names the step alone; the runner's error text can
// carry the command's output, which neither the log nor the ledger keeps. The caller
// returns the error (beads gt-eg0ji0, gt-1752lv).
func (d *Daemon) nodeImagePruneFailed(runErr error, apply bool, started time.Time) {
	d.logger.Printf("node_image_prune: failed: %s", nodeImagePruneStep(runErr))
	d.nodeImagePruneRecord(jobledger.Run{
		Job:       nodeImagePruneJob,
		StartedAt: started,
		EndedAt:   d.clk().Now(),
		Outcome:   nodeImagePruneOutcomeFailed,
	}, apply)
}

// nodeImagePruneSteps are the steps a runNodeImagePrune error can name, in the order
// the runner reaches them: each message reads "node-prune: <step>: <cause>", and a
// failed Job delete is joined after its cause.
var nodeImagePruneSteps = []string{
	"list the gt namespace",
	"work out the spare digests",
	"build the job manifest",
	"apply job",
	"wait for job",
	"read the log of job",
}

// nodeImagePruneStep names the step a failed run stopped at, from that list alone: a
// parse error quotes the log, so no error's own text is repeated here, and the one
// failure that names no step of its own is the parse.
func nodeImagePruneStep(runErr error) string {
	msg := strings.TrimPrefix(runErr.Error(), nodeImagePruneStepPrefix)
	for _, step := range nodeImagePruneSteps {
		if strings.HasPrefix(msg, step) {
			return step
		}
	}
	return "parse the log"
}

// nodeImagePruneRecord appends one run to the town's job ledger with the mode it
// ran in stamped on its stats: every node prune row carries nodeImagePruneApplyStat,
// and a run that gave no report carries it alone. A ledger
// that cannot record it is logged once: the ledger records the run and does not
// decide it, so the run's result is unchanged.
func (d *Daemon) nodeImagePruneRecord(run jobledger.Run, apply bool) {
	stats := make(map[string]int64, len(run.Stats)+1)
	for key, value := range run.Stats {
		stats[key] = value
	}
	stats[nodeImagePruneApplyStat] = 0
	if apply {
		stats[nodeImagePruneApplyStat] = 1
	}
	run.Stats = stats
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
