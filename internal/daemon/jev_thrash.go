package daemon

import (
	"context"
	"errors"
	"time"

	jevthrash "github.com/steveyegge/gastown/internal/jev/thrash"
)

const (
	// jevThrashInterval is how often a pass is due. A pass takes the placements
	// made since the last one, including any that closed in between, so the
	// interval is the most a placement waits before it is asked about.
	jevThrashInterval = 15 * time.Minute

	// jevThrashRunBudget bounds one pass: the bead list and the Jev calls for
	// every placement it reaches.
	jevThrashRunBudget = 10 * time.Minute
)

// triggerJevThrash starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls, so it never runs on the heartbeat. A
// single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevThrash() {
	if !d.isPatrolActive("jev_thrash") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_thrash", d.jevThrashRun.last(), now, jevThrashInterval)
	if !dec.due {
		return
	}
	if !d.jevThrashRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_thrash: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevThrashRunning.Store(false)
		d.runJevThrash()
		d.recordPatrolRun(&d.jevThrashRun, "jev_thrash", d.clk().Now())
	}()
}

// runJevThrash does one pass with the settings in the patrol config. A failure
// is logged, and the last-run time still advances, so a missing price or an
// unreadable bead store costs one line per interval rather than one per
// heartbeat.
func (d *Daemon) runJevThrash() {
	var settings jevthrash.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevThrash != nil {
		settings = jevthrash.Settings{
			MaxUSDPerDay:        c.Patrols.JevThrash.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevThrash.USDPerBillionTokens,
			Scrub:               c.Patrols.JevThrash.Scrub,
			ScrubCheck:          c.Patrols.JevThrash.ScrubCheck,
		}
	}
	open := d.jevThrashOpen
	if open == nil {
		open = jevthrash.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_thrash: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevThrashRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_thrash: stopped: E-STOP active after asking %d bead(s)", sum.Asked)
		return
	}
	if errors.Is(err, jevthrash.ErrBusy) {
		d.logger.Printf("jev_thrash: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_thrash: error: %v", err)
		return
	}
	for _, f := range sum.Failures {
		d.logger.Printf("jev_thrash: %s failed, not asked again: %v", f.Bead, f.Err)
	}
	if sum.Asked > 0 || sum.Failed > 0 || sum.CapReached {
		d.logger.Printf("jev_thrash: asked %d beads, %d failed (daily cap reached: %v)",
			sum.Asked, sum.Failed, sum.CapReached)
	}
}
