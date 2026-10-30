package daemon

import (
	"context"
	"errors"
	"io"
	"time"

	jevnotes "github.com/steveyegge/gastown/internal/jev/notes"
)

const (
	// jevNotesInterval is how often a pass is due: once a week. A file is asked
	// about again only when its content changes or thirty days pass, whatever
	// the interval.
	jevNotesInterval = 7 * 24 * time.Hour

	// jevNotesRunBudget bounds one pass: the clone's fetch, the --help runs and
	// the Jev calls for every file it reaches. Each call has its own timeout.
	jevNotesRunBudget = time.Hour
)

// jevNotesOpener builds the pass a run executes.
type jevNotesOpener func(townRoot string, s jevnotes.Settings, warnings io.Writer) (*jevnotes.Pass, error)

// triggerJevNotes starts one pass on its own goroutine when the patrol is on
// and a week has passed since the last one. A pass makes network calls and runs
// git and the town's binaries, so it never runs on the heartbeat. A
// single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevNotes() {
	if !d.isPatrolActive("jev_notes") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_notes", d.jevNotesRun.last(), now, jevNotesInterval)
	if !dec.due {
		return
	}
	if !d.jevNotesRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_notes: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevNotesRunning.Store(false)
		if !d.runJevNotes() {
			return
		}
		d.recordPatrolRun(&d.jevNotesRun, "jev_notes", d.clk().Now())
	}()
}

// runJevNotes does one pass with the settings in the patrol config. It reports
// false when the E-stop ended the pass: that week is not spent, so the next
// heartbeat after the E-stop clears runs the pass again, and the log keeps it
// from asking about the same files twice. Any other end, including a failure,
// reports true and the last-run time advances, so a missing price costs one line
// a week rather than one per heartbeat.
func (d *Daemon) runJevNotes() bool {
	var settings jevnotes.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevNotes != nil {
		settings = jevnotes.Settings{
			MaxUSDPerDay:        c.Patrols.JevNotes.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevNotes.USDPerBillionTokens,
			Scrub:               c.Patrols.JevNotes.Scrub,
		}
	}
	open := d.jevNotesOpen
	if open == nil {
		open = jevnotes.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_notes: error: %v", err)
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevNotesRunBudget)
	defer cancel()
	// The E-stop holds the patrol from starting, but a pass already in flight
	// would go on paying until the daily cap or its budget. The gate checks
	// between billed calls, as jev_dedupe's does.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_notes: stopped: E-STOP active after asking %d file(s)", sum.Asked)
		return false
	}
	if errors.Is(err, jevnotes.ErrBusy) {
		d.logger.Printf("jev_notes: skipped: %v", err)
		return true
	}
	if err != nil {
		d.logger.Printf("jev_notes: error: %v", err)
		return true
	}
	if sum.Asked > 0 || sum.Unanswered > 0 || sum.Deferred > 0 || sum.CapReached || sum.Unread > 0 || sum.Failed > 0 {
		d.logger.Printf("jev_notes: asked %d files, %d unanswered, %d deferred, %d unread, %d failed (daily cap reached: %v)",
			sum.Asked, sum.Unanswered, sum.Deferred, sum.Unread, sum.Failed, sum.CapReached)
	}
	return true
}
