package daemon

import (
	"context"
	"errors"
	"time"

	jevdispatch "github.com/steveyegge/gastown/internal/jev/dispatch"
)

const (
	// jevDispatchInterval is how often a pass is due. A pass takes the
	// placements made since the last one, including any that closed in
	// between, so the interval is the most a placement waits before it is
	// asked about.
	jevDispatchInterval = 15 * time.Minute

	// jevDispatchRunBudget bounds one pass: the bead list and the Jev calls
	// for every placement it reaches.
	jevDispatchRunBudget = 10 * time.Minute
)

// triggerJevDispatch starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls, so it never runs on the heartbeat. A
// single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevDispatch() {
	if !d.isPatrolActive("jev_dispatch") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_dispatch", d.jevDispatchRun.last(), now, jevDispatchInterval)
	if !dec.due {
		return
	}
	if !d.jevDispatchRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_dispatch: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevDispatchRunning.Store(false)
		d.runJevDispatch()
		d.recordPatrolRun(&d.jevDispatchRun, "jev_dispatch", d.clk().Now())
	}()
}

// runJevDispatch does one pass with the settings in the patrol config. A
// failure is logged, and the last-run time still advances, so a missing
// price or an unreadable bead store costs one line per interval rather than
// one per heartbeat.
func (d *Daemon) runJevDispatch() {
	var settings jevdispatch.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevDispatch != nil {
		settings = jevdispatch.Settings{
			MaxUSDPerDay:        c.Patrols.JevDispatch.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevDispatch.USDPerBillionTokens,
			Scrub:               c.Patrols.JevDispatch.Scrub,
			ScrubCheck:          c.Patrols.JevDispatch.ScrubCheck,
		}
	}
	open := d.jevDispatchOpen
	if open == nil {
		open = jevdispatch.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_dispatch: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevDispatchRunBudget)
	defer cancel()
	// The E-stop gate stops a pass already in flight between billed calls.
	gate := newEstopGate(d.config.TownRoot, pass.Asker, cancel)
	pass.Asker = gate
	sum, err := pass.Run(ctx)
	if gate.stopped {
		d.logger.Printf("jev_dispatch: stopped: E-STOP active after asking %d bead(s)", sum.Asked)
		return
	}
	if errors.Is(err, jevdispatch.ErrBusy) {
		d.logger.Printf("jev_dispatch: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_dispatch: error: %v", err)
		return
	}
	for _, f := range sum.Failures {
		if f.Final {
			d.logger.Printf("jev_dispatch: %s failed %d times, giving up: %v", f.Bead, f.Attempts, f.Err)
			continue
		}
		d.logger.Printf("jev_dispatch: %s failed, retrying next pass: %v", f.Bead, f.Err)
	}
	if sum.Asked > 0 || sum.Failed > 0 || sum.CapReached {
		d.logger.Printf("jev_dispatch: asked %d beads, %d failed, %d given up (daily cap reached: %v)",
			sum.Asked, sum.Failed, sum.GivenUp, sum.CapReached)
	}
}
