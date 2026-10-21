package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/jev/drift"
)

const (
	// jevDriftInterval is how often a pass is due. A pass takes the landings
	// since the last one, so the interval is the most a landing waits before
	// its docs are checked.
	jevDriftInterval = 15 * time.Minute

	// jevDriftRunBudget bounds one pass: the clone's fetch, the git reads, and
	// the Jev call for every doc section of every landing it reaches.
	jevDriftRunBudget = 10 * time.Minute
)

// triggerJevDrift starts one pass on its own goroutine when the patrol is on
// and due. The pass makes network calls and runs git, so it never runs on the
// heartbeat. A single-flight guard keeps one pass in flight at a time.
func (d *Daemon) triggerJevDrift() {
	if !d.isPatrolActive("jev_drift") {
		return
	}
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "jev_drift", time.Time{}, now, jevDriftInterval)
	if !dec.due {
		return
	}
	if !d.jevDriftRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("jev_drift: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.jevDriftRunning.Store(false)
		d.runJevDrift()
		if err := savePatrolLastRun(d.config.TownRoot, "jev_drift", d.clk().Now()); err != nil {
			d.logger.Printf("jev_drift: WARNING: cannot persist last-run time (%v)", err)
		}
	}()
}

// runJevDrift does one pass with the settings in the patrol config. A failure
// is logged, and the last-run time still advances, so a missing price costs
// one line per interval rather than one per heartbeat.
func (d *Daemon) runJevDrift() {
	var settings drift.Settings
	if c := d.patrolConfig; c != nil && c.Patrols != nil && c.Patrols.JevDrift != nil {
		settings = drift.Settings{
			MaxUSDPerDay:        c.Patrols.JevDrift.MaxUSDPerDay,
			USDPerBillionTokens: c.Patrols.JevDrift.USDPerBillionTokens,
		}
	}
	open := d.jevDriftOpen
	if open == nil {
		open = drift.Open
	}
	pass, err := open(d.config.TownRoot, settings, d.logger.Writer())
	if err != nil {
		d.logger.Printf("jev_drift: error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), jevDriftRunBudget)
	defer cancel()
	sum, err := pass.Run(ctx)
	if errors.Is(err, drift.ErrBusy) {
		d.logger.Printf("jev_drift: skipped: %v", err)
		return
	}
	if err != nil {
		d.logger.Printf("jev_drift: error: %v", err)
		return
	}
	if sum.Asked > 0 || sum.Skipped > 0 || sum.CapReached {
		d.logger.Printf("jev_drift: asked %d landings, skipped %d (daily cap reached: %v)", sum.Asked, sum.Skipped, sum.CapReached)
	}
}
