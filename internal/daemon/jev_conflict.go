package daemon

import (
	"context"
	"errors"
	"time"

	jevconflict "github.com/steveyegge/gastown/internal/jev/conflict"
)

const (
	// jevConflictInterval is how often a pass is due. A pass takes the
	// placements made since the last one, so the interval is the most a
	// placement waits before its pairs are asked about.
	jevConflictInterval = 15 * time.Minute

	// jevConflictRunBudget bounds one pass: the bead list and the Jev calls
	// for every pair it reaches.
	jevConflictRunBudget = 10 * time.Minute
)

// triggerJevConflict starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls, so it never runs on the heartbeat. A
// single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevConflict() {
	if !d.isPatrolActive("jev_conflict") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_conflict", d.jevConflictRun.last(), now, jevConflictInterval)
	if !dec.due {
		return
	}
	if !d.jevConflictRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_conflict: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevConflictRunning.Store(false)
		d.runJevConflict()
		d.recordPatrolRun(&d.jevConflictRun, "jev_conflict", d.clk().Now())
	}()
}

// runJevConflict does one pass with the settings in the patrol config. A
// failure is logged, and the last-run time still advances, so a missing price
// or an unreadable bead store costs one line per interval rather than one per
// heartbeat.
func (d *Daemon) runJevConflict() {
	var settings jevconflict.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevConflict != nil {
		settings = jevconflict.Settings{
			MaxUSDPerDay:        c.Patrols.JevConflict.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevConflict.USDPerBillionTokens,
			Scrub:               c.Patrols.JevConflict.Scrub,
			ScrubCheck:          c.Patrols.JevConflict.ScrubCheck,
		}
	}
	open := d.jevConflictOpen
	if open == nil {
		open = jevconflict.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_conflict: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevConflictRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_conflict: stopped: E-STOP active after asking %d pair(s)", sum.Asked)
		return
	}
	if errors.Is(err, jevconflict.ErrBusy) {
		d.logger.Printf("jev_conflict: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_conflict: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Failed > 0 || sum.CapReached {
		d.logger.Printf("jev_conflict: asked %d pairs, %d failed, %d given up (daily cap reached: %v)",
			sum.Asked, sum.Failed, sum.GivenUp, sum.CapReached)
	}
}
