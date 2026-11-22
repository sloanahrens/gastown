package daemon

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

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
// reached its log keeps its report in both modes; one that failed earlier records the
// failed row of nodeImagePruneFailed instead. Every runner error reaches the caller,
// and a run that cannot start logs why and records nothing (bead gt-gejrwk).
func (d *Daemon) nodeImagePruneOnce(ctx context.Context, kube nodePruneKube, runID string, apply bool, now time.Time) error {
	name, err := nodePruneJobName(runID)
	if err != nil {
		d.nodeImagePruneNotStarted(err)
		return err
	}
	image, err := d.nodePruneImage()
	if err != nil {
		d.nodeImagePruneNotStarted(err)
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
	return d.nodeImagePruneReport(rep, err, now, apply)
}

// nodeImagePruneReport logs a run's summary lines and records its ok ledger row from
// the report the runner returned, in the mode it ran in, and returns its error. A run
// that reached its log keeps its figures in both modes, even when its Job delete
// failed (beads gt-i8zplo, gt-ri2ax4).
func (d *Daemon) nodeImagePruneReport(rep nodePruneReport, runErr error, started time.Time, apply bool) error {
	lines, stats := nodeImagePruneSummary(rep, apply)
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
	}, apply)
	return runErr
}

// nodeImagePruneNotStartedMax is the longest error text a "not started" line
// carries, in bytes.
const nodeImagePruneNotStartedMax = 200

// nodeImagePruneNotStarted logs why a prune that never reached the cluster did not
// start, so it is not silent until the hook acts (bead gt-gejrwk). Both errors it
// logs are the daemon's own text, not command output. The text is the error's first
// line, cut at nodeImagePruneNotStartedMax bytes without splitting a rune: one
// logger call is one line. Nothing ran, so no ledger row is recorded; returning the
// error leaves the failure count to the caller (bead gt-ypnfzq).
func (d *Daemon) nodeImagePruneNotStarted(err error) {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > nodeImagePruneNotStartedMax {
		msg = msg[:nodeImagePruneNotStartedMax]
		for len(msg) > 0 && !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	d.logger.Printf("node_image_prune: not started: %s", msg)
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
