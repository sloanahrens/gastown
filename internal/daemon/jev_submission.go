package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/jev/submission"
)

const (
	// jevSubmissionInterval is how often a pass is due. A pass takes the
	// submissions since the last one, so the interval is the most a
	// submission waits before it is asked about.
	jevSubmissionInterval = 15 * time.Minute

	// jevSubmissionRunBudget bounds one pass: the clone's fetch, the git
	// reads and the Jev call for every submission it reaches.
	jevSubmissionRunBudget = 10 * time.Minute
)

// triggerJevSubmission starts one pass on its own goroutine when the patrol
// is on and due. The pass makes network calls and runs git, so it never runs
// on the heartbeat. A single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevSubmission() {
	if !d.isPatrolActive("jev_submission") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_submission", d.jevSubmissionRun.last(), now, jevSubmissionInterval)
	if !dec.due {
		return
	}
	if !d.jevSubmissionRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_submission: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevSubmissionRunning.Store(false)
		d.runJevSubmission()
		d.recordPatrolRun(&d.jevSubmissionRun, "jev_submission", d.clk().Now())
	}()
}

// runJevSubmission does one pass with the settings in the patrol config. A
// failure is logged, and the last-run time still advances, so a missing price
// or a down clone costs one line per interval rather than one per heartbeat.
func (d *Daemon) runJevSubmission() {
	var settings submission.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevSubmission != nil {
		settings = submission.Settings{
			MaxUSDPerDay:        c.Patrols.JevSubmission.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevSubmission.USDPerBillionTokens,
			Scrub:               c.Patrols.JevSubmission.Scrub,
		}
	}
	open := d.jevSubmissionOpen
	if open == nil {
		open = submission.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_submission: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevSubmissionRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_submission: stopped: E-STOP active after asking %d submission(s)", sum.Asked)
		return
	}
	if errors.Is(err, submission.ErrBusy) {
		d.logger.Printf("jev_submission: skipped: %v", err)
		return
	}
	// A clone that could not be fetched skips the pass and leaves the cursor
	// unmoved, so a warning is the right size: the submissions wait, and the
	// next interval tries again.
	if errors.Is(err, submission.ErrFetch) {
		d.logger.Printf("jev_submission: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_submission: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Skipped > 0 || sum.Repeated > 0 || sum.CapReached {
		d.logger.Printf("jev_submission: asked %d submissions, skipped %d, repeated %d (daily cap reached: %v)", sum.Asked, sum.Skipped, sum.Repeated, sum.CapReached)
	}
}
